package notify

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/config"
)

// fakeSMTP is just enough of an SMTP server to see what the notifier sends and when.
type fakeSMTP struct {
	t        *testing.T
	ln       net.Listener
	cert     tls.Certificate
	startTLS bool // offer STARTTLS
	implicit bool // TLS from the first byte (port 465)
	// replies replaces the reply to a command ("MAIL") for the first n connections.
	replies     map[string]string
	failFirst   int
	mu          sync.Mutex
	connections int
	mails       []fakeMail
	// log has every command, with "(tls)" when it came over an encrypted connection.
	log []string
}

type fakeMail struct {
	from, auth string
	to         []string
	data       string
	tls        bool
}

// newFakeSMTP starts the server; options set its behaviour before the first connection.
func newFakeSMTP(t *testing.T, cert tls.Certificate, options ...func(*fakeSMTP)) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{t: t, ln: ln, cert: cert}
	for _, option := range options {
		option(f)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) addr() string { return f.ln.Addr().String() }

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	f.mu.Lock()
	f.connections++
	n := f.connections
	f.mu.Unlock()
	encrypted := false
	if f.implicit {
		conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
		encrypted = true
	}
	r := textproto.NewReader(bufio.NewReader(conn))
	reply := func(s string) { fmt.Fprintf(conn, "%s\r\n", s) }
	reply("220 fake ESMTP")
	var m fakeMail
	for {
		line, err := r.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		f.mu.Lock()
		entry := verb
		if encrypted {
			entry += " (tls)"
		}
		f.log = append(f.log, entry)
		override, ok := f.replies[verb]
		f.mu.Unlock()
		if ok && n <= f.failFirst {
			reply(override)
			continue
		}
		switch verb {
		case "EHLO":
			if f.startTLS && !encrypted {
				reply("250-fake")
				reply("250-STARTTLS")
			} else {
				reply("250-fake")
				reply("250-AUTH PLAIN")
			}
			reply("250 8BITMIME")
		case "STARTTLS":
			reply("220 go ahead")
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			r = textproto.NewReader(bufio.NewReader(conn))
			encrypted = true
		case "AUTH":
			raw, _ := base64.StdEncoding.DecodeString(strings.Fields(line)[2])
			m.auth = string(raw)
			reply("235 ok")
		case "MAIL":
			m.from = strings.TrimSuffix(strings.TrimPrefix(line, "MAIL FROM:<"), ">")
			m.from = strings.Fields(m.from)[0]
			m.from = strings.TrimSuffix(m.from, ">")
			reply("250 ok")
		case "RCPT":
			m.to = append(m.to, strings.TrimSuffix(strings.TrimPrefix(line, "RCPT TO:<"), ">"))
			reply("250 ok")
		case "DATA":
			reply("354 go on")
			data, err := r.ReadDotBytes()
			if err != nil {
				return
			}
			m.data = string(data)
			m.tls = encrypted
			f.mu.Lock()
			f.mails = append(f.mails, m)
			f.mu.Unlock()
			m = fakeMail{}
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not here")
		}
	}
}

func (f *fakeSMTP) sent() []fakeMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeMail(nil), f.mails...)
}

func (f *fakeSMTP) conns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connections
}

func (f *fakeSMTP) commands() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.log, ", ")
}

// selfSigned makes a certificate for 127.0.0.1 and a pool that trusts it.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake smtp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func emailTo(f *fakeSMTP, pool *x509.CertPool, security string) *Email {
	return &Email{
		Label: "ops", Addr: f.addr(), Security: security,
		From: "pulsewatch <alerts@example.com>", To: []string{"ops@example.com", "Dana <dana@example.com>"},
		TLSConfig: &tls.Config{RootCAs: pool}, Sleep: noSleep(new([]time.Duration)),
		Now: func() time.Time { return started.Add(time.Minute) },
	}
}

func TestEmailOverSTARTTLSWithLogin(t *testing.T) {
	cert, pool := selfSigned(t)
	f := newFakeSMTP(t, cert, func(f *fakeSMTP) { f.startTLS = true })
	e := emailTo(f, pool, config.SecurityStartTLS)
	e.Username, e.Password = "pw-user", "s3cret"
	if err := e.Notify(context.Background(), downAlert()); err != nil {
		t.Fatal(err)
	}
	mails := f.sent()
	if len(mails) != 1 {
		t.Fatalf("%d mails", len(mails))
	}
	m := mails[0]
	if !m.tls || m.auth != "\x00pw-user\x00s3cret" || m.from != "alerts@example.com" ||
		strings.Join(m.to, " ") != "ops@example.com dana@example.com" {
		t.Errorf("%+v", m)
	}
	// Nothing but the greeting and STARTTLS before the switch.
	if got := f.commands(); !strings.HasPrefix(got, "EHLO, STARTTLS, EHLO (tls), AUTH (tls), MAIL (tls)") {
		t.Errorf("commands: %s", got)
	}
	msg, err := mail.ReadMessage(strings.NewReader(m.data))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subject != "🔴 Website is down: HTTP 502 Bad Gateway" {
		t.Errorf("subject %q %v", subject, err)
	}
	if msg.Header.Get("To") != `<ops@example.com>, "Dana" <dana@example.com>` {
		t.Errorf("to %q", msg.Header.Get("To"))
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if !strings.Contains(string(body), "Website is down.") || !strings.Contains(string(body), "HTTP 502 Bad Gateway") {
		t.Errorf("body %q", body)
	}
}

func TestNoEmailWithoutSTARTTLS(t *testing.T) {
	cert, pool := selfSigned(t)
	f := newFakeSMTP(t, cert) // doesn't offer STARTTLS
	e := emailTo(f, pool, config.SecurityStartTLS)
	e.Username, e.Password = "pw-user", "s3cret"
	err := e.Notify(context.Background(), downAlert())
	if err == nil || !strings.Contains(err.Error(), "doesn't offer STARTTLS") {
		t.Fatalf("err %v", err)
	}
	if len(f.sent()) != 0 || f.conns() != 1 {
		t.Errorf("sent %d over %d connections", len(f.sent()), f.conns())
	}
	if got := f.commands(); strings.Contains(got, "AUTH") || strings.Contains(got, "MAIL") {
		t.Errorf("sent in plain text: %s", got)
	}
}

func TestEmailOverImplicitTLS(t *testing.T) {
	cert, pool := selfSigned(t)
	f := newFakeSMTP(t, cert, func(f *fakeSMTP) { f.implicit = true })
	e := emailTo(f, pool, config.SecurityTLS)
	e.Username, e.Password = "u", "p"
	if err := e.Notify(context.Background(), downAlert()); err != nil {
		t.Fatal(err)
	}
	if mails := f.sent(); len(mails) != 1 || !mails[0].tls || mails[0].auth != "\x00u\x00p" {
		t.Errorf("%+v", mails)
	}
}

func TestEmailWithoutEncryptionForALocalServer(t *testing.T) {
	cert, _ := selfSigned(t)
	f := newFakeSMTP(t, cert)
	e := emailTo(f, nil, config.SecurityNone)
	if err := e.Notify(context.Background(), downAlert()); err != nil {
		t.Fatal(err)
	}
	if mails := f.sent(); len(mails) != 1 || mails[0].tls {
		t.Errorf("%+v", mails)
	}
}

func TestUntrustedCertificateIsNotRetried(t *testing.T) {
	cert, _ := selfSigned(t)
	f := newFakeSMTP(t, cert, func(f *fakeSMTP) { f.startTLS = true })
	_, otherPool := selfSigned(t)
	e := emailTo(f, otherPool, config.SecurityStartTLS)
	err := e.Notify(context.Background(), downAlert())
	var permanent errPermanent
	if err == nil || !errors.As(err, &permanent) || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("err %v", err)
	}
	if f.conns() != 1 || len(f.sent()) != 0 {
		t.Errorf("%d connections, %d mails", f.conns(), len(f.sent()))
	}
}

func TestEmailRetriesA4xxButNotA5xx(t *testing.T) {
	cert, _ := selfSigned(t)
	f := newFakeSMTP(t, cert, func(f *fakeSMTP) {
		f.replies = map[string]string{"MAIL": "451 try again later"}
		f.failFirst = 2
	})
	e := emailTo(f, nil, config.SecurityNone)
	var waits []time.Duration
	e.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	if err := e.Notify(context.Background(), downAlert()); err != nil {
		t.Fatal(err)
	}
	if len(f.sent()) != 1 || f.conns() != 3 || fmt.Sprint(waits) != "[1s 2s]" {
		t.Errorf("%d mails, %d connections, waits %v", len(f.sent()), f.conns(), waits)
	}

	g := newFakeSMTP(t, cert, func(f *fakeSMTP) {
		f.replies = map[string]string{"RCPT": "550 no such user"}
		f.failFirst = 10
	})
	err := emailTo(g, nil, config.SecurityNone).Notify(context.Background(), downAlert())
	if err == nil || !strings.Contains(err.Error(), "recipient ops@example.com: 550 no such user") || g.conns() != 1 {
		t.Errorf("err %v after %d connections", err, g.conns())
	}
}

func TestEmailGivesUpWhenTheContextEnds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accepts and never says hello
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	e := &Email{Addr: ln.Addr().String(), Security: config.SecurityNone, From: "a@example.com", To: []string{"b@example.com"}, Attempts: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	begun := time.Now()
	if err := e.Notify(ctx, downAlert()); err == nil {
		t.Fatal("no error")
	}
	if took := time.Since(begun); took > 5*time.Second {
		t.Errorf("took %v", took)
	}
}

func TestRecoveryEmailAnswersTheOutageEmail(t *testing.T) {
	from := &mail.Address{Name: "pulsewatch", Address: "alerts@example.com"}
	to := []*mail.Address{{Address: "ops@example.com"}}
	down := downAlert()
	down.Incident.Checks = 3
	down.Incident.Cause = "connection refused\nSubject: injected"
	down.StatusPage = "https://status.example.com"
	downMsg, err := mail.ReadMessage(strings.NewReader(string(Message(down, from, to, started))))
	if err != nil {
		t.Fatal(err)
	}
	if downMsg.Header.Get("Message-Id") != fmt.Sprintf("<pulsewatch.web-1.%d@example.com>", started.Unix()) {
		t.Errorf("message id %q", downMsg.Header.Get("Message-Id"))
	}
	for _, line := range strings.Split(string(Message(down, from, to, started)), "\r\n") {
		if len(line) > 78 {
			t.Errorf("line of %d characters: %q", len(line), line)
		}
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(downMsg.Header.Get("Subject"))
	if subject != "🔴 Website is down: connection refused Subject: injected" {
		t.Errorf("subject %q", subject)
	}
	if downMsg.Header.Get("Auto-Submitted") != "auto-generated" || downMsg.Header.Get("Date") != "Sat, 21 Nov 2026 14:03:00 +0000" {
		t.Errorf("headers %v", downMsg.Header)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(downMsg.Body))
	for _, want := range []string{"Since:         21 Nov 2026 14:03 UTC", "Failed checks: 3 in a row", "Status page: https://status.example.com"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q in %q", want, body)
		}
	}

	up := down
	up.Kind = KindRecovered
	resolved := started.Add(3*time.Hour + 5*time.Minute)
	up.Incident.Resolved = &resolved
	up.Incident.LastMessage = "HTTP 503"
	upMsg, err := mail.ReadMessage(strings.NewReader(string(Message(up, from, to, resolved))))
	if err != nil {
		t.Fatal(err)
	}
	id := downMsg.Header.Get("Message-Id")
	if upMsg.Header.Get("In-Reply-To") != id || upMsg.Header.Get("References") != id || upMsg.Header.Get("Message-Id") == id {
		t.Errorf("threading %v", upMsg.Header)
	}
	subject, _ = new(mime.WordDecoder).DecodeHeader(upMsg.Header.Get("Subject"))
	if subject != "🟢 Website is back up after 3h 5m" {
		t.Errorf("subject %q", subject)
	}
	body, _ = io.ReadAll(quotedprintable.NewReader(upMsg.Body))
	for _, want := range []string{"Down:        21 Nov 2026 14:03 UTC to 21 Nov 2026 17:08 UTC", "Last error:  HTTP 503"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q in %q", want, body)
		}
	}
}

func TestFromConfigMakesAnEmailNotifier(t *testing.T) {
	n := FromConfig(config.Alert{Format: config.FormatEmail, SMTP: "smtp.test:587", Security: config.SecurityStartTLS,
		From: "a@example.com", To: []string{"b@example.com"}, Username: "u", Password: "p"})
	e, ok := n.(*Email)
	if !ok || e.Name() != "email" || e.Addr != "smtp.test:587" || e.Password != "p" || e.To[0] != "b@example.com" {
		t.Errorf("%#v", n)
	}
}
