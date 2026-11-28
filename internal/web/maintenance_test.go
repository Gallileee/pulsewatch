package web

import (
	"strings"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/store"
)

func mw(title string, from, to time.Duration, ids ...string) config.Maintenance {
	return config.Maintenance{Title: title, Start: config.Time{Time: now.Add(from)}, End: config.Time{Time: now.Add(to)}, TargetIDs: ids}
}

// maintenanceFixture: the API failed during a window that is still going on.
func maintenanceFixture() *store.Store {
	st := fixture()
	st.Record(check.Result{TargetID: "api", At: now, Status: check.Maintenance, Message: "HTTP 503 <oops>"})
	return st
}

var windows = []config.Maintenance{
	mw("Later, all", 48*time.Hour, 49*time.Hour),
	mw("Database <upgrade>", -30*time.Minute, 30*time.Minute, "api"),
	mw("Soon", 24*time.Hour, 26*time.Hour, "web", "gone"),
	mw("Over", -3*time.Hour, -2*time.Hour, "api"),
}

func TestBuildWithMaintenance(t *testing.T) {
	p := Build("Acme", targets, windows, maintenanceFixture(), now)
	if p.Overall.Status != "maintenance" || p.Overall.Text != "Maintenance in progress" {
		t.Errorf("overall %+v", p.Overall)
	}
	api := p.Targets[1]
	if api.Status != "maintenance" || api.StatusText != "Under maintenance" || api.Maintenance != "Database <upgrade>" ||
		!api.MaintenanceEnd.Equal(now.Add(30*time.Minute)) {
		t.Errorf("api %+v", api)
	}
	if p.Targets[0].Maintenance != "" {
		t.Errorf("web isn't in maintenance: %+v", p.Targets[0])
	}
	var titles []string
	for _, m := range p.Maintenance {
		titles = append(titles, m.Title)
	}
	if strings.Join(titles, "|") != "Database <upgrade>|Soon|Later, all" {
		t.Errorf("listed %q", titles)
	}
	active, soon, all := p.Maintenance[0], p.Maintenance[1], p.Maintenance[2]
	if !active.Active || soon.Active || active.When != "21 Nov 2026, 11:30–12:30 UTC" {
		t.Errorf("active %+v, soon %+v", active, soon)
	}
	if strings.Join(soon.Targets, ",") != "Website <b>,gone" || len(all.Targets) != 0 {
		t.Errorf("targets %q %q", soon.Targets, all.Targets)
	}
	if soon.When != "22 Nov 2026, 12:00–14:00 UTC" {
		t.Errorf("when %q", soon.When)
	}
}

func TestMaintenanceStatusAfterTheWindow(t *testing.T) {
	// The window is over and no check has come since: the last failure shows as down.
	p := Build("Acme", targets, windows[3:], maintenanceFixture(), now)
	if api := p.Targets[1]; api.Status != "down" || api.StatusText != "Down" || api.Maintenance != "" {
		t.Errorf("api %+v", api)
	}
	if p.Overall.Status != "partial" || len(p.Maintenance) != 0 {
		t.Errorf("overall %+v, maintenance %+v", p.Overall, p.Maintenance)
	}
}

func TestSpan(t *testing.T) {
	start := time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC)
	if got := span(start, start.Add(5*time.Hour)); got != "24 Nov 2026, 22:00 UTC – 25 Nov 2026, 03:00 UTC" {
		t.Errorf("across midnight: %q", got)
	}
	prague := time.FixedZone("CET", 3600)
	if got := span(start.In(prague), start.Add(time.Hour).In(prague)); got != "24 Nov 2026, 22:00–23:00 UTC" {
		t.Errorf("zone: %q", got)
	}
}

func TestMaintenanceDayAndOverall(t *testing.T) {
	if v := dayView(store.Day{Date: now, Maintenance: 3}); v.Class != "maintenance" || v.Label != "21 Nov 2026: maintenance (3 failed checks)" {
		t.Errorf("%+v", v)
	}
	if v := dayView(store.Day{Date: now, Checks: 10, Good: 10, Maintenance: 2}); v.Class != "ok" ||
		v.Label != "21 Nov 2026: 100 % up (10 checks, 2 failed during maintenance)" {
		t.Errorf("%+v", v)
	}
	if got := overall(map[string]int{"maintenance": 1, "down": 1}, 2); got.Status != "partial" {
		t.Errorf("a real outage wins: %+v", got)
	}
	if got := overall(map[string]int{"maintenance": 1, "degraded": 1}, 2); got.Status != "maintenance" {
		t.Errorf("%+v", got)
	}
	if b := BadgeFor(TargetView{Status: "maintenance", StatusText: "Under maintenance", Uptime7d: 1}); b.Color != "blue" ||
		b.Message != "under maintenance · 100 % (7 d)" {
		t.Errorf("badge %+v", b)
	}
}

func TestPageShowsMaintenance(t *testing.T) {
	s, _ := newServer(maintenanceFixture())
	s.Maintenance = windows
	body := get(t, s.Handler(), "/").Body.String()
	for _, want := range []string{
		"Scheduled maintenance", "Database &lt;upgrade&gt;", `class="pill pill-maintenance">In progress`,
		`class="pill">Scheduled`, "All services", "Website &lt;b&gt;, gone", "Maintenance in progress",
		"Planned maintenance: Database &lt;upgrade&gt;, until", `class="banner banner-maintenance"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	scheduled, history, _ := strings.Cut(body, `id="incidents-title"`)
	if strings.Contains(scheduled, ">Over<") || strings.Contains(body, "<upgrade>") {
		t.Error("an ended window in the scheduled list, or unescaped text")
	}
	// It's in the history instead, next to the incidents.
	for _, want := range []string{">Incidents and maintenance<", "<strong>Over</strong>", "Maintenance completed",
		"21 Nov 2026, 09:00–10:00 UTC"} {
		if !strings.Contains(history, want) {
			t.Errorf("history: missing %q", want)
		}
	}
	if !strings.Contains(scheduled, `<a href="maintenance.ics" class="muted">Add to calendar</a>`) {
		t.Error("no calendar link")
	}

	s.Maintenance = nil
	if body := get(t, s.Handler(), "/").Body.String(); strings.Contains(body, "Scheduled maintenance") {
		t.Error("an empty maintenance section")
	}
}
