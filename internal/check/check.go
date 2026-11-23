// Package check runs one check of a target: an HTTP request, a TLS handshake or a DNS lookup.
package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Gallileee/pulsewatch/internal/clock"
	"github.com/Gallileee/pulsewatch/internal/config"
)

// Status is how a check went.
type Status string

// Statuses. Degraded works but needs attention (slow, a certificate about to expire); it counts
// as up for uptime and doesn't open an incident.
const (
	Up       Status = "up"
	Degraded Status = "degraded"
	Down     Status = "down"
	// Maintenance is a failed check during planned maintenance (config.Maintenance): it opens
	// no incident and doesn't count for uptime. A good check during maintenance stays Up.
	Maintenance Status = "maintenance"
)

// UserAgent is sent with every HTTP check.
var UserAgent = "pulsewatch (+https://github.com/Gallileee/pulsewatch)"

// Result is the outcome of one check.
type Result struct {
	TargetID string        `json:"target"`
	At       time.Time     `json:"at"`
	Status   Status        `json:"status"`
	Latency  time.Duration `json:"latency_ns"`
	// Code is the HTTP status code (http checks).
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// CertExpiry is when the certificate expires (tls checks).
	CertExpiry *time.Time `json:"cert_expiry,omitempty"`
}

// Checker checks one target.
type Checker interface {
	Check(ctx context.Context) Result
}

// Options are what the checkers share; the zero value uses the real clock, network and roots.
type Options struct {
	Clock clock.Clock
	// HTTPClient is used for http checks (its Timeout is ignored: the target's applies).
	HTTPClient *http.Client
	// RootCAs verifies certificates in tls checks (nil: the system's).
	RootCAs  *x509.CertPool
	Resolver Resolver
}

// New makes the checker for a target.
func New(t config.Target, opts Options) (Checker, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	switch t.Type {
	case config.TypeHTTP:
		client := opts.HTTPClient
		if client == nil {
			client = &http.Client{}
		}
		return &HTTP{
			ID: t.ID, URL: t.URL, Timeout: time.Duration(t.Timeout), ExpectStatus: t.ExpectStatus,
			ExpectBody: t.ExpectBody, DegradedAfter: time.Duration(t.DegradedAfter), Client: client, Clock: opts.Clock,
		}, nil
	case config.TypeTLS:
		host, port, err := config.SplitHostPort(t.Host, "443")
		if err != nil {
			return nil, err
		}
		return &TLS{
			ID: t.ID, Host: host, Port: port, Timeout: time.Duration(t.Timeout), WarnDays: t.WarnDays,
			RootCAs: opts.RootCAs, Clock: opts.Clock,
		}, nil
	case config.TypeDNS:
		resolver := opts.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		return &DNS{
			ID: t.ID, Host: t.Host, Timeout: time.Duration(t.Timeout), ExpectAddresses: t.ExpectAddresses,
			Resolver: resolver, Clock: opts.Clock,
		}, nil
	}
	return nil, fmt.Errorf("unknown check type %q", t.Type)
}

// HTTP requests a URL with GET.
type HTTP struct {
	ID            string
	URL           string
	Timeout       time.Duration
	ExpectStatus  []int
	ExpectBody    string
	DegradedAfter time.Duration
	Client        *http.Client
	Clock         clock.Clock
}

// maxBody is how much of a body is read when looking for ExpectBody.
const maxBody = 1 << 20

// Check requests the URL.
func (h *HTTP) Check(ctx context.Context) Result {
	r := Result{TargetID: h.ID, At: h.Clock.Now()}
	ctx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return down(r, start, "bad request: %v", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := h.Client.Do(req)
	if err != nil {
		return down(r, start, "%s", describe(ctx, err, h.Timeout))
	}
	defer resp.Body.Close()
	r.Code = resp.StatusCode
	var body []byte
	if h.ExpectBody != "" {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	} else {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	}
	r.Latency = time.Since(start)
	if err != nil {
		return down(r, start, "reading the body: %s", describe(ctx, err, h.Timeout))
	}
	if !h.goodStatus(resp.StatusCode) {
		r.Status, r.Message = Down, fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		return r
	}
	if h.ExpectBody != "" && !strings.Contains(string(body), h.ExpectBody) {
		r.Status, r.Message = Down, fmt.Sprintf("HTTP %d, but the body doesn't contain %q", resp.StatusCode, h.ExpectBody)
		return r
	}
	r.Status, r.Message = Up, fmt.Sprintf("HTTP %d", resp.StatusCode)
	if h.DegradedAfter > 0 && r.Latency > h.DegradedAfter {
		r.Status = Degraded
		r.Message = fmt.Sprintf("HTTP %d, slow: %s (over %s)", resp.StatusCode, round(r.Latency), h.DegradedAfter)
	}
	return r
}

func (h *HTTP) goodStatus(code int) bool {
	if len(h.ExpectStatus) == 0 {
		return code >= 200 && code < 400
	}
	return slices.Contains(h.ExpectStatus, code)
}

// TLS connects and looks at the certificate.
type TLS struct {
	ID       string
	Host     string
	Port     string
	Timeout  time.Duration
	WarnDays int
	RootCAs  *x509.CertPool
	Clock    clock.Clock
}

// Check makes a TLS handshake: down if it fails or the certificate isn't valid (verified at the
// clock's time), degraded if it expires within WarnDays.
func (t *TLS) Check(ctx context.Context) Result {
	now := t.Clock.Now()
	r := Result{TargetID: t.ID, At: now}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	start := time.Now()
	dialer := &tls.Dialer{Config: &tls.Config{
		ServerName: t.Host,
		RootCAs:    t.RootCAs,
		Time:       t.Clock.Now,
		MinVersion: tls.VersionTLS12,
	}}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(t.Host, t.Port))
	if err != nil {
		return down(r, start, "%s", describe(ctx, err, t.Timeout))
	}
	defer conn.Close()
	r.Latency = time.Since(start)
	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		r.Status, r.Message = Down, "no certificate"
		return r
	}
	expiry := state.PeerCertificates[0].NotAfter
	r.CertExpiry = &expiry
	left := expiry.Sub(now)
	days := int(left.Hours() / 24)
	when := expiry.UTC().Format("2 Jan 2006")
	if left < time.Duration(t.WarnDays)*24*time.Hour {
		r.Status, r.Message = Degraded, fmt.Sprintf("certificate expires in %s (%s)", plural(days, "day"), when)
		return r
	}
	r.Status, r.Message = Up, fmt.Sprintf("certificate valid until %s (%s)", when, plural(days, "day"))
	return r
}

// Resolver looks up a host name (net.Resolver is one).
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DNS resolves a name.
type DNS struct {
	ID              string
	Host            string
	Timeout         time.Duration
	ExpectAddresses []string
	Resolver        Resolver
	Clock           clock.Clock
}

// Check resolves the name: down if it doesn't resolve, or to none of ExpectAddresses.
func (d *DNS) Check(ctx context.Context) Result {
	r := Result{TargetID: d.ID, At: d.Clock.Now()}
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	start := time.Now()
	addrs, err := d.Resolver.LookupHost(ctx, d.Host)
	r.Latency = time.Since(start)
	if err != nil {
		return down(r, start, "%s", describe(ctx, err, d.Timeout))
	}
	if len(d.ExpectAddresses) > 0 && !slices.ContainsFunc(addrs, func(a string) bool {
		return slices.ContainsFunc(d.ExpectAddresses, func(e string) bool { return sameIP(a, e) })
	}) {
		r.Status, r.Message = Down, fmt.Sprintf("resolves to %s, expected %s", strings.Join(addrs, ", "), strings.Join(d.ExpectAddresses, " or "))
		return r
	}
	r.Status, r.Message = Up, "resolves to "+strings.Join(addrs, ", ")
	return r
}

func sameIP(a, b string) bool {
	x, y := net.ParseIP(a), net.ParseIP(b)
	return x != nil && x.Equal(y)
}

func down(r Result, start time.Time, format string, args ...any) Result {
	if r.Latency == 0 {
		r.Latency = time.Since(start)
	}
	r.Status, r.Message = Down, fmt.Sprintf(format, args...)
	return r
}

// describe turns an error into a short message; a deadline says how long it waited.
func describe(ctx context.Context, err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("timed out after %s", timeout)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return fmt.Sprintf("%s: no such host", dnsErr.Name)
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "certificate: " + certErr.Err.Error()
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Op + ": " + opErr.Err.Error()
	}
	return err.Error()
}

func round(d time.Duration) time.Duration {
	if d > time.Second {
		return d.Round(10 * time.Millisecond)
	}
	return d.Round(time.Millisecond)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
