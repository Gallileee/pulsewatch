package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"github.com/Gallileee/pulsewatch/internal/config"
)

// Email sends the alert as a plain-text email over SMTP.
//
// With starttls (the default) nothing, not even the sender, goes out before the connection is
// encrypted: a server that doesn't offer STARTTLS gets no email, rather than one in plain text.
type Email struct {
	Label    string
	Addr     string // host:port
	Security string
	Username string
	Password string
	From     string
	To       []string
	// TLSConfig replaces the default TLS settings (tests trust their own certificate). The server
	// name is always the host from Addr.
	TLSConfig *tls.Config
	// Attempts is how many times to try (4 by default); between tries it waits 1s, 2s, 4s...
	Attempts int
	// Sleep waits between tries and Now gives the Date header (tests replace them).
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
}

// emailFromConfig makes an email notifier; the config has been validated.
func emailFromConfig(a config.Alert) *Email {
	label := a.Name
	if label == "" {
		label = "email"
	}
	return &Email{
		Label: label, Addr: a.SMTP, Security: a.Security, Username: a.Username, Password: a.Password,
		From: a.From, To: a.To,
	}
}

// Name is how logs refer to it.
func (e *Email) Name() string { return e.Label }

// Notify sends the alert to every recipient, trying again after a network error or a 4xx reply.
func (e *Email) Notify(ctx context.Context, a Alert) error {
	from, err := mail.ParseAddress(e.From)
	if err != nil {
		return errPermanent{fmt.Errorf("from: %w", err)}
	}
	to := make([]*mail.Address, 0, len(e.To))
	for _, s := range e.To {
		addr, err := mail.ParseAddress(s)
		if err != nil {
			return errPermanent{fmt.Errorf("to: %w", err)}
		}
		to = append(to, addr)
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	msg := Message(a, from, to, now())
	return retry(ctx, e.Attempts, e.Sleep, func() error { return e.send(ctx, from, to, msg) })
}

func (e *Email) send(ctx context.Context, from *mail.Address, to []*mail.Address, msg []byte) error {
	host, _, err := net.SplitHostPort(e.Addr)
	if err != nil {
		return errPermanent{fmt.Errorf("smtp %q isn't host:port", e.Addr)}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", e.Addr)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	// net/smtp knows nothing of contexts: closing the connection ends a stuck conversation.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()

	tlsConfig := e.tlsConfig(host)
	if e.Security == config.SecurityTLS {
		tc := tls.Client(conn, tlsConfig)
		if err := tc.HandshakeContext(ctx); err != nil {
			return tlsError(err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return smtpError("greeting", err)
	}
	if e.Security == config.SecurityStartTLS {
		if err := c.Hello("localhost"); err != nil {
			return smtpError("EHLO", err)
		}
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errPermanent{errors.New("the server doesn't offer STARTTLS; set security: tls for port 465")}
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return tlsError(err)
		}
	}
	if e.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.Username, e.Password, host)); err != nil {
			return smtpError("login", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return smtpError("sender", err)
	}
	for _, addr := range to {
		if err := c.Rcpt(addr.Address); err != nil {
			return smtpError("recipient "+addr.Address, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return smtpError("DATA", err)
	}
	if _, err := w.Write(msg); err != nil {
		return smtpError("message", err)
	}
	if err := w.Close(); err != nil {
		return smtpError("message", err)
	}
	// The server has the email now; a failed goodbye doesn't mean it wasn't sent.
	_ = c.Quit()
	return nil
}

func (e *Email) tlsConfig(host string) *tls.Config {
	conf := &tls.Config{MinVersion: tls.VersionTLS12}
	if e.TLSConfig != nil {
		conf = e.TLSConfig.Clone()
	}
	conf.ServerName = host
	return conf
}

// smtpError says which step failed. A 5xx reply (bad login, unknown recipient) won't change on
// another try; a 4xx reply or a dropped connection might. The password is never in the text:
// net/smtp doesn't echo it, and the server's reply is about the login, not its content.
func smtpError(step string, err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		err = fmt.Errorf("%s: %d %s", step, reply.Code, reply.Msg)
		if reply.Code >= 500 {
			return errPermanent{err}
		}
		return err
	}
	if strings.Contains(err.Error(), "unencrypted connection") {
		return errPermanent{fmt.Errorf("%s: %w", step, err)}
	}
	return fmt.Errorf("%s: %w", step, err)
}

// tlsError: an untrusted or wrong certificate is a setup problem, so it isn't retried.
func tlsError(err error) error {
	var verify *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	if errors.As(err, &verify) || errors.As(err, &unknown) || errors.As(err, &hostname) {
		return errPermanent{fmt.Errorf("TLS: %w", err)}
	}
	return fmt.Errorf("TLS: %w", err)
}

// Message writes the alert as an email. The recovery email answers the outage email (same
// thread in mail apps), since both carry Message-IDs made from the incident.
func Message(a Alert, from *mail.Address, to []*mail.Address, now time.Time) []byte {
	inc := a.Incident
	var subject string
	var body strings.Builder
	when := func(t time.Time) string { return t.UTC().Format("2 Jan 2006 15:04 UTC") }
	if a.Kind == KindRecovered {
		took := Duration(inc.Duration(*inc.Resolved))
		subject = fmt.Sprintf("🟢 %s is back up after %s", inc.TargetName, took)
		fmt.Fprintf(&body, "%s is back up after %s.\n\n", inc.TargetName, took)
		fmt.Fprintf(&body, "Down:        %s to %s\n", when(inc.Started), when(*inc.Resolved))
		fmt.Fprintf(&body, "First error: %s\n", inc.Cause)
		if inc.LastMessage != "" && inc.LastMessage != inc.Cause {
			fmt.Fprintf(&body, "Last error:  %s\n", inc.LastMessage)
		}
	} else {
		subject = fmt.Sprintf("🔴 %s is down: %s", inc.TargetName, inc.Cause)
		fmt.Fprintf(&body, "%s is down.\n\n", inc.TargetName)
		fmt.Fprintf(&body, "Error:         %s\n", inc.Cause)
		fmt.Fprintf(&body, "Since:         %s\n", when(inc.Started))
		if inc.Checks > 0 {
			fmt.Fprintf(&body, "Failed checks: %d in a row\n", inc.Checks)
		}
	}
	if a.StatusPage != "" {
		fmt.Fprintf(&body, "\nStatus page: %s\n", a.StatusPage)
	}
	body.WriteString("\n-- \nSent by pulsewatch when an incident opens or is resolved.\n")

	recipients := make([]string, len(to))
	for i, addr := range to {
		recipients[i] = addr.String()
	}
	domain := "pulsewatch.invalid"
	if at := strings.LastIndexByte(from.Address, '@'); at >= 0 {
		domain = from.Address[at+1:]
	}
	// The start time keeps IDs apart when the history (and with it the incident numbers) starts over.
	outage := fmt.Sprintf("<pulsewatch.%s.%d@%s>", headerSafe(inc.ID), inc.Started.Unix(), domain)

	var msg bytes.Buffer
	header := func(name, value string) { fmt.Fprintf(&msg, "%s: %s\r\n", name, value) }
	header("From", from.String())
	header("To", strings.Join(recipients, ", "))
	// Each encoded word on its own line keeps long subjects under 78 characters a line (RFC 5322);
	// a first word too long to follow "Subject: " starts on the next line.
	encoded := strings.ReplaceAll(mime.QEncoding.Encode("utf-8", oneLine(subject)), "?= =?", "?=\r\n =?")
	gap := " "
	if first, _, _ := strings.Cut(encoded, "\r\n"); len("Subject: ")+len(first) > 78 {
		gap = "\r\n "
	}
	fmt.Fprintf(&msg, "Subject:%s%s\r\n", gap, encoded)
	header("Date", now.Format(time.RFC1123Z))
	if a.Kind == KindRecovered {
		header("Message-ID", strings.Replace(outage, "<pulsewatch.", "<pulsewatch.recovered.", 1))
		header("In-Reply-To", outage)
		header("References", outage)
	} else {
		header("Message-ID", outage)
	}
	// RFC 3834: out-of-office replies leave it alone.
	header("Auto-Submitted", "auto-generated")
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	msg.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&msg)
	_, _ = qp.Write([]byte(strings.ReplaceAll(body.String(), "\n", "\r\n")))
	_ = qp.Close()
	return msg.Bytes()
}

// oneLine keeps a check's message (which may hold a line break) from starting a new header.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\r' || r == '\n' }), " ")
}

// headerSafe keeps only the characters a Message-ID may hold.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, s)
}
