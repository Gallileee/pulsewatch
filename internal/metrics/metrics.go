// Package metrics counts the checks for Prometheus, written in its text format by hand (no
// client library needed for a handful of series).
package metrics

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Gallileee/pulsewatch/internal/check"
)

// Buckets are the latency histogram's upper bounds, in seconds.
var Buckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Target describes a target for the labels.
type Target struct {
	ID, Name, Type string
}

type series struct {
	target      Target
	up          float64
	degraded    float64
	checks      map[check.Status]uint64
	buckets     []uint64 // per bound, not cumulative
	sum         float64
	count       uint64
	open        float64
	maintenance float64
	certExpiry  float64
}

// Metrics holds the counters of every target.
type Metrics struct {
	mu      sync.Mutex
	version string
	targets map[string]*series
	order   []string
}

// New registers the targets (in the order the output lists them).
func New(version string, targets []Target) *Metrics {
	m := &Metrics{version: version, targets: map[string]*series{}}
	for _, t := range targets {
		m.targets[t.ID] = &series{target: t, checks: map[check.Status]uint64{}, buckets: make([]uint64, len(Buckets))}
		m.order = append(m.order, t.ID)
	}
	return m
}

// Observe counts a check.
func (m *Metrics) Observe(r check.Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.targets[r.TargetID]
	if s == nil {
		return
	}
	s.up = boolValue(r.Status == check.Up || r.Status == check.Degraded)
	s.degraded = boolValue(r.Status == check.Degraded)
	s.checks[r.Status]++
	seconds := r.Latency.Seconds()
	if i := slices.IndexFunc(Buckets, func(b float64) bool { return seconds <= b }); i >= 0 {
		s.buckets[i]++
	}
	s.sum += seconds
	s.count++
	if r.CertExpiry != nil {
		s.certExpiry = float64(r.CertExpiry.Unix())
	}
}

// SetMaintenance records whether the target is in a maintenance window.
func (m *Metrics) SetMaintenance(id string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.targets[id]; s != nil {
		s.maintenance = boolValue(on)
	}
}

// SetIncident records whether the target has an open incident.
func (m *Metrics) SetIncident(id string, open bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.targets[id]; s != nil {
		s.open = boolValue(open)
	}
}

// WriteTo writes every metric in the Prometheus text format (version 0.0.4).
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	header := func(name, kind, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	}
	header("pulsewatch_build_info", "gauge", "The running version.")
	fmt.Fprintf(&b, "pulsewatch_build_info{version=%s} 1\n", quote(m.version))

	header("pulsewatch_up", "gauge", "1 if the latest check was up or degraded, 0 if it failed (also during maintenance).")
	m.each(func(s *series, l string) { fmt.Fprintf(&b, "pulsewatch_up{%s} %s\n", l, num(s.up)) }, true)

	header("pulsewatch_degraded", "gauge", "1 if the latest check was degraded (slow, or a certificate about to expire).")
	m.each(func(s *series, l string) { fmt.Fprintf(&b, "pulsewatch_degraded{%s} %s\n", l, num(s.degraded)) }, true)

	header("pulsewatch_checks_total", "counter", "Checks since the start, by status.")
	m.each(func(s *series, l string) {
		for _, status := range []check.Status{check.Up, check.Degraded, check.Down, check.Maintenance} {
			fmt.Fprintf(&b, "pulsewatch_checks_total{%s,status=%q} %d\n", l, status, s.checks[status])
		}
	}, false)

	header("pulsewatch_check_duration_seconds", "histogram", "How long the checks took.")
	m.each(func(s *series, l string) {
		var cumulative uint64
		for i, bound := range Buckets {
			cumulative += s.buckets[i]
			fmt.Fprintf(&b, "pulsewatch_check_duration_seconds_bucket{%s,le=%q} %d\n", l, num(bound), cumulative)
		}
		fmt.Fprintf(&b, "pulsewatch_check_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", l, s.count)
		fmt.Fprintf(&b, "pulsewatch_check_duration_seconds_sum{%s} %s\n", l, num(s.sum))
		fmt.Fprintf(&b, "pulsewatch_check_duration_seconds_count{%s} %d\n", l, s.count)
	}, false)

	header("pulsewatch_maintenance", "gauge", "1 while the target is in a planned maintenance window (as of its latest check).")
	m.each(func(s *series, l string) { fmt.Fprintf(&b, "pulsewatch_maintenance{%s} %s\n", l, num(s.maintenance)) }, false)

	header("pulsewatch_incident_open", "gauge", "1 while the target has an open incident.")
	m.each(func(s *series, l string) { fmt.Fprintf(&b, "pulsewatch_incident_open{%s} %s\n", l, num(s.open)) }, false)

	header("pulsewatch_tls_cert_expiry_timestamp_seconds", "gauge", "When the certificate expires (tls targets).")
	m.each(func(s *series, l string) {
		if s.certExpiry != 0 {
			fmt.Fprintf(&b, "pulsewatch_tls_cert_expiry_timestamp_seconds{%s} %s\n", l, num(s.certExpiry))
		}
	}, false)
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// each calls f for every target that has been checked, with its labels (all of them when
// full, else only the ID).
func (m *Metrics) each(f func(s *series, labels string), full bool) {
	for _, id := range m.order {
		s := m.targets[id]
		if s.count == 0 {
			continue
		}
		labels := "target=" + quote(id)
		if full {
			labels += ",name=" + quote(s.target.Name) + ",type=" + quote(s.target.Type)
		}
		f(s, labels)
	}
}

// quote writes a label value: in double quotes, with \, " and newlines escaped.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
