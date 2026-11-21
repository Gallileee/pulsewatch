package check

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/clock"
	"github.com/Gallileee/pulsewatch/internal/config"
)

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func httpChecker(url string) *HTTP {
	return &HTTP{ID: "web", URL: url, Timeout: 2 * time.Second, Client: &http.Client{}, Clock: clock.NewFake(now)}
}

func TestHTTPUp(t *testing.T) {
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.UserAgent()
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()
	r := httpChecker(srv.URL).Check(context.Background())
	if r.Status != Up || r.Code != 200 || r.Message != "HTTP 200" || r.TargetID != "web" || !r.At.Equal(now) {
		t.Errorf("%+v", r)
	}
	if r.Latency <= 0 {
		t.Error("no latency")
	}
	if !strings.HasPrefix(agent, "pulsewatch") {
		t.Errorf("user agent %q", agent)
	}
}

func TestHTTPStatusCodes(t *testing.T) {
	code := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	defer srv.Close()
	for _, c := range []struct {
		code   int
		expect []int
		want   Status
	}{
		{200, nil, Up},
		{301, nil, Up}, // redirects aren't followed without a Location; 3xx counts as up
		{399, nil, Up},
		{404, nil, Down},
		{503, nil, Down},
		{401, []int{401}, Up},
		{200, []int{204}, Down},
	} {
		code = c.code
		h := httpChecker(srv.URL)
		h.ExpectStatus = c.expect
		if r := h.Check(context.Background()); r.Status != c.want {
			t.Errorf("code %d, expect %v: %s (%s)", c.code, c.expect, r.Status, r.Message)
		}
	}
	code = 503
	if r := httpChecker(srv.URL).Check(context.Background()); r.Message != "HTTP 503 Service Unavailable" {
		t.Errorf("message %q", r.Message)
	}
}

func TestHTTPExpectBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	h := httpChecker(srv.URL)
	h.ExpectBody = `"status":"ok"`
	if r := h.Check(context.Background()); r.Status != Up {
		t.Errorf("%+v", r)
	}
	h.ExpectBody = "healthy"
	r := h.Check(context.Background())
	if r.Status != Down || !strings.Contains(r.Message, `doesn't contain "healthy"`) {
		t.Errorf("%+v", r)
	}
}

func TestHTTPSlowIsDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
	}))
	defer srv.Close()
	h := httpChecker(srv.URL)
	h.DegradedAfter = 20 * time.Millisecond
	r := h.Check(context.Background())
	if r.Status != Degraded || !strings.Contains(r.Message, "slow") {
		t.Errorf("%+v", r)
	}
	h.DegradedAfter = time.Second
	if r := h.Check(context.Background()); r.Status != Up {
		t.Errorf("%+v", r)
	}
}

func TestHTTPTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	h := httpChecker(srv.URL)
	h.Timeout = 50 * time.Millisecond
	r := h.Check(context.Background())
	if r.Status != Down || r.Message != "timed out after 50ms" {
		t.Errorf("%+v", r)
	}
}

func TestHTTPRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	r := httpChecker(url).Check(context.Background())
	if r.Status != Down || !strings.Contains(r.Message, "dial") {
		t.Errorf("%+v", r)
	}
}

func tlsChecker(t *testing.T, srv *httptest.Server, clk clock.Clock, warnDays int) *TLS {
	t.Helper()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	// httptest's certificate is for "example.com" and 127.0.0.1; ServerName must match the IP.
	return &TLS{ID: "cert", Host: host, Port: port, Timeout: 2 * time.Second, WarnDays: warnDays, RootCAs: pool, Clock: clk}
}

func TestTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	expiry := srv.Certificate().NotAfter
	clk := clock.NewFake(expiry.Add(-100 * 24 * time.Hour))
	c := tlsChecker(t, srv, clk, 14)

	r := c.Check(context.Background())
	if r.Status != Up || r.CertExpiry == nil || !r.CertExpiry.Equal(expiry) || !strings.Contains(r.Message, "valid until") ||
		!strings.Contains(r.Message, "100 days") {
		t.Errorf("valid: %+v", r)
	}
	clk.Set(expiry.Add(-5 * 24 * time.Hour))
	if r := c.Check(context.Background()); r.Status != Degraded || !strings.Contains(r.Message, "expires in 5 days") {
		t.Errorf("expiring: %+v", r)
	}
	clk.Set(expiry.Add(24 * time.Hour))
	if r := c.Check(context.Background()); r.Status != Down || !strings.Contains(r.Message, "certificate") {
		t.Errorf("expired: %+v", r)
	}
}

func TestTLSUntrusted(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	c := tlsChecker(t, srv, clock.NewFake(now), 14)
	c.RootCAs = nil // the system's roots don't know httptest's certificate
	r := c.Check(context.Background())
	if r.Status != Down || !strings.HasPrefix(r.Message, "certificate: ") {
		t.Errorf("%+v", r)
	}
}

func TestTLSNoServer(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	host, port, _ := net.SplitHostPort(addr)
	c := &TLS{ID: "x", Host: host, Port: port, Timeout: time.Second, Clock: clock.NewFake(now)}
	if r := c.Check(context.Background()); r.Status != Down {
		t.Errorf("%+v", r)
	}
}

type fakeResolver struct {
	addrs []string
	err   error
}

func (f fakeResolver) LookupHost(context.Context, string) ([]string, error) { return f.addrs, f.err }

func TestDNS(t *testing.T) {
	d := &DNS{ID: "dns", Host: "example.com", Timeout: time.Second, Clock: clock.NewFake(now),
		Resolver: fakeResolver{addrs: []string{"93.184.215.14", "2606:2800:21f:cb07:6820:80da:af6b:8b2c"}}}
	if r := d.Check(context.Background()); r.Status != Up || !strings.Contains(r.Message, "93.184.215.14") {
		t.Errorf("%+v", r)
	}
	d.ExpectAddresses = []string{"2606:2800:21f:cb07:6820:80da:AF6B:8b2c"} // same address, other spelling
	if r := d.Check(context.Background()); r.Status != Up {
		t.Errorf("%+v", r)
	}
	d.ExpectAddresses = []string{"10.0.0.1"}
	if r := d.Check(context.Background()); r.Status != Down || !strings.Contains(r.Message, "expected 10.0.0.1") {
		t.Errorf("%+v", r)
	}
	d.Resolver = fakeResolver{err: &net.DNSError{Name: "example.com", Err: "no such host", IsNotFound: true}}
	if r := d.Check(context.Background()); r.Status != Down || r.Message != "example.com: no such host" {
		t.Errorf("%+v", r)
	}
	d.Resolver = fakeResolver{err: errors.New("boom")}
	if r := d.Check(context.Background()); r.Status != Down || r.Message != "boom" {
		t.Errorf("%+v", r)
	}
}

func TestNew(t *testing.T) {
	for _, target := range []config.Target{
		{ID: "a", Type: config.TypeHTTP, URL: "http://a.test", Timeout: config.Duration(time.Second)},
		{ID: "b", Type: config.TypeTLS, Host: "b.test:8443", Timeout: config.Duration(time.Second)},
		{ID: "c", Type: config.TypeDNS, Host: "c.test", Timeout: config.Duration(time.Second)},
	} {
		c, err := New(target, Options{})
		if err != nil || c == nil {
			t.Errorf("%s: %v", target.Type, err)
		}
	}
	if tlsCheck, _ := New(config.Target{Type: config.TypeTLS, Host: "b.test:8443"}, Options{}); tlsCheck.(*TLS).Port != "8443" {
		t.Error("port")
	}
	if _, err := New(config.Target{Type: "ping"}, Options{}); err == nil {
		t.Error("unknown type: no error")
	}
}
