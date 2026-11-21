package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
)

func TestWriteTo(t *testing.T) {
	m := New("v0.1.0", []Target{
		{ID: "web", Name: `Web "main"`, Type: "http"},
		{ID: "cert", Name: "Cert", Type: "tls"},
		{ID: "idle", Name: "Never checked", Type: "dns"},
	})
	m.Observe(check.Result{TargetID: "web", Status: check.Up, Latency: 80 * time.Millisecond})
	m.Observe(check.Result{TargetID: "web", Status: check.Down, Latency: 3 * time.Second})
	m.Observe(check.Result{TargetID: "web", Status: check.Degraded, Latency: 20 * time.Second})
	expiry := time.Unix(1800000000, 0)
	m.Observe(check.Result{TargetID: "cert", Status: check.Up, Latency: 10 * time.Millisecond, CertExpiry: &expiry})
	m.Observe(check.Result{TargetID: "unknown", Status: check.Up})
	m.SetIncident("web", true)
	m.SetIncident("unknown", true)

	var b strings.Builder
	if _, err := m.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`pulsewatch_build_info{version="v0.1.0"} 1`,
		`# TYPE pulsewatch_up gauge`,
		`pulsewatch_up{target="web",name="Web \"main\"",type="http"} 1`,
		`pulsewatch_degraded{target="web",name="Web \"main\"",type="http"} 1`,
		`pulsewatch_checks_total{target="web",status="up"} 1`,
		`pulsewatch_checks_total{target="web",status="down"} 1`,
		`pulsewatch_checks_total{target="web",status="degraded"} 1`,
		`# TYPE pulsewatch_check_duration_seconds histogram`,
		`pulsewatch_check_duration_seconds_bucket{target="web",le="0.05"} 0`,
		`pulsewatch_check_duration_seconds_bucket{target="web",le="0.1"} 1`,
		`pulsewatch_check_duration_seconds_bucket{target="web",le="5"} 2`,
		`pulsewatch_check_duration_seconds_bucket{target="web",le="10"} 2`,
		`pulsewatch_check_duration_seconds_bucket{target="web",le="+Inf"} 3`,
		`pulsewatch_check_duration_seconds_sum{target="web"} 23.08`,
		`pulsewatch_check_duration_seconds_count{target="web"} 3`,
		`pulsewatch_incident_open{target="web"} 1`,
		`pulsewatch_incident_open{target="cert"} 0`,
		`pulsewatch_tls_cert_expiry_timestamp_seconds{target="cert"} 1.8e+09`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	for _, unwanted := range []string{`target="idle"`, `target="unknown"`, `tls_cert_expiry_timestamp_seconds{target="web"}`} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unexpected %s", unwanted)
		}
	}
}

func TestQuote(t *testing.T) {
	if got := quote("a\\b\"c\nd"); got != `"a\\b\"c\nd"` {
		t.Errorf("%s", got)
	}
}
