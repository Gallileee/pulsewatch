package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/clock"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/incident"
	"github.com/Gallileee/pulsewatch/internal/metrics"
	"github.com/Gallileee/pulsewatch/internal/monitor"
	"github.com/Gallileee/pulsewatch/internal/store"
)

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

var targets = []config.Target{
	{ID: "web", Name: "Website <b>", Type: config.TypeHTTP},
	{ID: "api", Name: "API", Type: config.TypeHTTP},
	{ID: "new", Name: "New one", Type: config.TypeDNS},
}

func fixture() *store.Store {
	st := store.New()
	for i := range 10 {
		st.Record(check.Result{TargetID: "web", At: now.Add(time.Duration(i-10) * time.Minute), Status: check.Up,
			Latency: time.Duration(100+i*10) * time.Millisecond, Message: "HTTP 200"})
	}
	st.Record(check.Result{TargetID: "api", At: now.Add(-2 * time.Minute), Status: check.Down, Message: "HTTP 503 <oops>"})
	st.Record(check.Result{TargetID: "api", At: now.Add(-time.Minute), Status: check.Down, Message: "HTTP 503 <oops>"})
	st.SaveIncident(incident.Incident{ID: "api-1", TargetID: "api", TargetName: "API", Started: now.Add(-2 * time.Minute),
		Cause: "HTTP 503 <oops>", Checks: 2})
	resolved := now.Add(-47 * time.Hour)
	st.SaveIncident(incident.Incident{ID: "web-1", TargetID: "web", TargetName: "Website <b>", Started: now.Add(-48 * time.Hour),
		Resolved: &resolved, Cause: "timed out", Checks: 5})
	return st
}

func newServer(st *store.Store) (*Server, *monitor.Broker) {
	broker := monitor.NewBroker()
	m := metrics.New("test", []metrics.Target{{ID: "web", Name: "Website", Type: "http"}})
	m.Observe(check.Result{TargetID: "web", Status: check.Up, Latency: time.Millisecond})
	return &Server{Title: "Acme status", Targets: targets, Store: st, Metrics: m, Broker: broker, Clock: clock.NewFake(now)}, broker
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestBuild(t *testing.T) {
	p := Build("Acme", targets, fixture(), now)
	if p.Overall.Status != "partial" || p.Overall.Text != "Partial outage" {
		t.Errorf("overall %+v", p.Overall)
	}
	web, api, fresh := p.Targets[0], p.Targets[1], p.Targets[2]
	if web.Status != "up" || web.StatusText != "Operational" || web.Uptime24h != 1 || web.LatencyMs != 190 {
		t.Errorf("web %+v", web)
	}
	if web.Spark == "" || !strings.HasPrefix(web.Spark, "M0.0 ") || web.SparkMax != 190 {
		t.Errorf("spark %q %v", web.Spark, web.SparkMax)
	}
	if len(web.Days) != DaysShown || web.Days[DaysShown-1].Class != "ok" || web.Days[0].Class != "none" {
		t.Errorf("days: last %+v, first %+v", web.Days[DaysShown-1], web.Days[0])
	}
	if api.Status != "down" || api.Uptime24h != 0 || api.Spark != "" {
		t.Errorf("api %+v", api)
	}
	if fresh.Status != "unknown" || fresh.Uptime90d != -1 {
		t.Errorf("new %+v", fresh)
	}
	if len(p.Incidents) != 2 || p.Incidents[0].ID != "web-1" || p.Incidents[0].DurationText != "1h 0m" {
		t.Errorf("incidents %+v", p.Incidents)
	}
}

func TestOverall(t *testing.T) {
	for _, c := range []struct {
		counts map[string]int
		want   string
	}{
		{map[string]int{"unknown": 2}, "unknown"},
		{map[string]int{"down": 2}, "major"},
		{map[string]int{"down": 1, "up": 1}, "partial"},
		{map[string]int{"degraded": 1, "up": 1}, "degraded"},
		{map[string]int{"up": 2}, "up"},
		{map[string]int{"up": 1, "unknown": 1}, "up"},
	} {
		if got := overall(c.counts, 2); got.Status != c.want {
			t.Errorf("%v: %s", c.counts, got.Status)
		}
	}
}

func TestPercent(t *testing.T) {
	for share, want := range map[float64]string{
		1: "100 %", 0.9995: "99.95 %", 0.99999: "99.99 %", 0.873: "87.3 %", 0.5: "50 %", 0: "0 %", -1: "—",
	} {
		if got := Percent(share); got != want {
			t.Errorf("%v: %q, want %q", share, got, want)
		}
	}
}

func TestDayClasses(t *testing.T) {
	for _, c := range []struct {
		checks, good int
		class        string
	}{{0, 0, "none"}, {1000, 1000, "ok"}, {1000, 995, "minor"}, {1000, 960, "partial"}, {1000, 900, "major"}} {
		v := dayView(store.Day{Date: now, Checks: c.checks, Good: c.good})
		if v.Class != c.class {
			t.Errorf("%d/%d: %s", c.good, c.checks, v.Class)
		}
	}
	if v := dayView(store.Day{Date: now, Checks: 1000, Good: 995}); v.Label != "21 Nov 2026: 99.5 % up (1000 checks)" {
		t.Errorf("label %q", v.Label)
	}
}

func TestPage(t *testing.T) {
	s, _ := newServer(fixture())
	rec := get(t, s.Handler(), "/")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>Acme status</title>", "Partial outage", "Website &lt;b&gt;", "HTTP 503 &lt;oops&gt;",
		`class="pill pill-down"`, "Ongoing", "Resolved", `class="bar bar-ok"`, "<path d=\"M0.0 ", "Not checked yet",
		`<script src="static/app.js" defer>`, `data-mode="live"`, `href="api/status"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "<b>") || strings.Contains(body, "<oops>") {
		t.Error("unescaped text")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("csp %q", csp)
	}
}

func TestPartial(t *testing.T) {
	s, _ := newServer(fixture())
	rec := get(t, s.Handler(), "/partials/live")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Partial outage") || strings.Contains(body, "<html") {
		t.Errorf("%d %.200s", rec.Code, body)
	}
}

func TestStatusJSON(t *testing.T) {
	s, _ := newServer(fixture())
	rec := get(t, s.Handler(), "/api/status")
	var p struct {
		Overall Overall `json:"overall"`
		Targets []struct {
			ID        string  `json:"id"`
			Status    string  `json:"status"`
			Uptime24h float64 `json:"uptime_24h"`
			Days      []struct {
				Date string `json:"date"`
			} `json:"days"`
		} `json:"targets"`
		Incidents []map[string]any `json:"incidents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Overall.Status != "partial" || len(p.Targets) != 3 || p.Targets[1].Status != "down" || len(p.Targets[0].Days) != DaysShown {
		t.Errorf("%+v", p)
	}
	if p.Targets[0].Days[DaysShown-1].Date != "2026-11-21" {
		t.Errorf("last day %s", p.Targets[0].Days[DaysShown-1].Date)
	}
	if len(p.Incidents) != 2 || p.Incidents[0]["duration"] != "1h 0m" || p.Incidents[0]["cause"] != "timed out" {
		t.Errorf("incidents %+v", p.Incidents)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("no CORS header")
	}
}

func TestMetricsHealthAndStatic(t *testing.T) {
	s, _ := newServer(fixture())
	h := s.Handler()
	if rec := get(t, h, "/metrics"); !strings.Contains(rec.Body.String(), `pulsewatch_up{target="web"`) ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Errorf("metrics %d %.200s", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, "/healthz"); rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Errorf("healthz %d", rec.Code)
	}
	for _, path := range []string{"/static/app.js", "/static/style.css", "/static/favicon.svg"} {
		if rec := get(t, h, path); rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	if rec := get(t, h, "/nope"); rec.Code != 404 {
		t.Errorf("unknown path: %d", rec.Code)
	}
}

func TestEvents(t *testing.T) {
	s, broker := newServer(fixture())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type %q", resp.Header.Get("Content-Type"))
	}
	lines := bufio.NewReader(resp.Body)
	read := func() string {
		line, err := lines.ReadString('\n')
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		return strings.TrimRight(line, "\n")
	}
	if got := read(); got != "retry: 5000" {
		t.Fatalf("first line %q", got)
	}
	read() // ": connected"
	read()
	broker.Publish(monitor.Event{TargetID: "web", Status: check.Down, At: now})
	if got := read(); got != "event: check" {
		t.Fatalf("%q", got)
	}
	if got := read(); got != `data: {"target":"web","status":"down","at":"2026-11-21T12:00:00Z"}` {
		t.Errorf("%q", got)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	old := Heartbeat
	Heartbeat = 10 * time.Millisecond
	defer func() { Heartbeat = old }()
	s, _ := newServer(fixture())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewReader(resp.Body)
	for range 3 {
		_, _ = lines.ReadString('\n')
	}
	if line, _ := lines.ReadString('\n'); line != ": ping\n" {
		t.Errorf("%q", line)
	}
}

func TestMilliseconds(t *testing.T) {
	for ms, want := range map[float64]string{0.02: "< 0.1 ms", 0.42: "0.4 ms", 12.4: "12 ms", 999: "999 ms", 1360: "1.36 s"} {
		if got := Milliseconds(ms); got != want {
			t.Errorf("%v: %q", ms, got)
		}
	}
}

func TestWriteSite(t *testing.T) {
	s, _ := newServer(fixture())
	dir := t.TempDir()
	if err := s.WriteSite(dir); err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-mode="static"`, `href="status.json"`, "Checked every few minutes", "Partial outage",
		`<meta http-equiv="Content-Security-Policy"`} {
		if !strings.Contains(string(html), want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(string(html), `href="metrics"`) || strings.Contains(string(html), `href="/`) {
		t.Error("links to the server or to absolute paths")
	}
	var p Page
	data, _ := os.ReadFile(filepath.Join(dir, "status.json"))
	if err := json.Unmarshal(data, &p); err != nil || len(p.Targets) != 3 || p.Overall.Status != "partial" {
		t.Errorf("status.json: %v %+v", err, p.Overall)
	}
	for _, asset := range []string{"app.js", "style.css", "favicon.svg"} {
		if info, err := os.Stat(filepath.Join(dir, "static", asset)); err != nil || info.Size() == 0 {
			t.Errorf("%s: %v", asset, err)
		}
	}
}
