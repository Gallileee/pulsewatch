package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/incident"
)

var t0 = time.Date(2026, 11, 21, 12, 30, 0, 0, time.UTC)

func res(at time.Time, status check.Status, ms int) check.Result {
	return check.Result{TargetID: "web", At: at, Status: status, Latency: time.Duration(ms) * time.Millisecond}
}

func TestRecordKeepsLastRecentAndHours(t *testing.T) {
	s := New()
	s.Record(res(t0, check.Up, 100))
	s.Record(res(t0.Add(10*time.Minute), check.Degraded, 300))
	s.Record(res(t0.Add(40*time.Minute), check.Down, 0)) // the next hour
	last, ok := s.Last("web")
	if !ok || last.Status != check.Down {
		t.Errorf("last %+v", last)
	}
	if _, ok := s.Last("other"); ok {
		t.Error("other has a last check")
	}
	recent := s.Recent("web")
	if len(recent) != 3 || recent[1].LatencyMs != 300 || recent[1].Status != check.Degraded {
		t.Errorf("recent %+v", recent)
	}
	hours := s.series["web"].Hours
	if len(hours) != 2 || hours[0].Checks != 2 || hours[0].Up != 1 || hours[0].Degraded != 1 || hours[0].Good() != 2 ||
		hours[0].LatencyMs != 400 || hours[1].Checks != 1 || hours[1].Good() != 0 {
		t.Errorf("hours %+v", hours)
	}
	if hours[0].Start != t0.Truncate(time.Hour).Unix() {
		t.Errorf("start %d", hours[0].Start)
	}
}

func TestRecentIsCapped(t *testing.T) {
	s := New()
	for i := range RecentChecks + 5 {
		s.Record(res(t0.Add(time.Duration(i)*time.Second), check.Up, i))
	}
	recent := s.Recent("web")
	if len(recent) != RecentChecks || recent[0].LatencyMs != 5 {
		t.Errorf("%d points, first %v", len(recent), recent[0].LatencyMs)
	}
}

func TestOlderResultsAreIgnored(t *testing.T) {
	s := New()
	s.Record(res(t0, check.Up, 1))
	s.Record(res(t0.Add(-time.Hour), check.Down, 1))
	if last, _ := s.Last("web"); last.Status != check.Up || len(s.Recent("web")) != 1 {
		t.Error("an older result was recorded")
	}
}

func TestOldHoursArePruned(t *testing.T) {
	s := New()
	s.Record(res(t0.Add(-Retention-2*time.Hour), check.Up, 1))
	s.Record(res(t0.Add(-Retention+2*time.Hour), check.Up, 1))
	s.Record(res(t0, check.Up, 1))
	if n := len(s.series["web"].Hours); n != 2 {
		t.Errorf("%d hours kept", n)
	}
}

func TestUptime(t *testing.T) {
	s := New()
	if u, n := s.Uptime("web", time.Hour, t0); u != -1 || n != 0 {
		t.Errorf("no data: %v %d", u, n)
	}
	// Two days ago: down. Today: 3 up, 1 degraded, 1 down.
	s.Record(res(t0.Add(-48*time.Hour), check.Down, 1))
	for i, st := range []check.Status{check.Up, check.Up, check.Degraded, check.Down, check.Up} {
		s.Record(res(t0.Add(time.Duration(i)*time.Minute), st, 1))
	}
	if u, n := s.Uptime("web", 24*time.Hour, t0.Add(10*time.Minute)); u != 0.8 || n != 5 {
		t.Errorf("24h: %v %d", u, n)
	}
	if u, n := s.Uptime("web", 7*24*time.Hour, t0.Add(10*time.Minute)); n != 6 || u != 4.0/6 {
		t.Errorf("7d: %v %d", u, n)
	}
	if _, n := s.Uptime("other", time.Hour, t0); n != 0 {
		t.Error("other has data")
	}
}

func TestDays(t *testing.T) {
	s := New()
	s.Record(res(t0.Add(-49*time.Hour), check.Down, 1)) // 19 Nov
	s.Record(res(t0.Add(-24*time.Hour), check.Up, 1))   // 20 Nov
	s.Record(res(t0, check.Up, 1))                      // 21 Nov
	s.Record(res(t0.Add(time.Minute), check.Down, 1))
	days := s.Days("web", 3, t0.Add(time.Hour))
	if len(days) != 3 {
		t.Fatalf("%d days", len(days))
	}
	if days[0].Date.Format("2006-01-02") != "2026-11-19" || days[2].Date.Format("2006-01-02") != "2026-11-21" {
		t.Errorf("dates %v %v", days[0].Date, days[2].Date)
	}
	if days[0].Uptime() != 0 || days[1].Uptime() != 1 || days[2].Uptime() != 0.5 || days[2].Checks != 2 {
		t.Errorf("%+v", days)
	}
	if empty := s.Days("other", 2, t0); len(empty) != 2 || empty[0].Uptime() != -1 {
		t.Errorf("%+v", empty)
	}
}

func TestIncidents(t *testing.T) {
	s := New()
	s.SaveIncident(incident.Incident{ID: "a", TargetID: "web", Cause: "x"})
	s.SaveIncident(incident.Incident{ID: "b", TargetID: "api"})
	s.SaveIncident(incident.Incident{ID: "a", TargetID: "web", Cause: "y"}) // replaces
	all := s.Incidents()
	if len(all) != 2 || all[0].ID != "b" || all[1].Cause != "y" {
		t.Errorf("%+v", all)
	}
	if open, ok := s.OpenIncident("web"); !ok || open.ID != "a" {
		t.Errorf("open %+v", open)
	}
	resolved := t0
	s.SaveIncident(incident.Incident{ID: "a", TargetID: "web", Resolved: &resolved})
	if _, ok := s.OpenIncident("web"); ok {
		t.Error("a resolved incident is open")
	}
	for i := range MaxIncidents + 3 {
		s.SaveIncident(incident.Incident{ID: string(rune('a'+i%26)) + time.Duration(i).String()})
	}
	if n := len(s.Incidents()); n != MaxIncidents {
		t.Errorf("%d incidents kept", n)
	}
}

func TestForget(t *testing.T) {
	s := New()
	s.Record(res(t0, check.Up, 1))
	s.Record(check.Result{TargetID: "gone", At: t0, Status: check.Up})
	s.Forget([]string{"web"})
	if _, ok := s.Last("gone"); ok {
		t.Error("gone is kept")
	}
	if _, ok := s.Last("web"); !ok {
		t.Error("web is forgotten")
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	if _, err := Load(path); !errors.Is(err, ErrNoFile) {
		t.Errorf("first start: %v", err)
	}
	s := New()
	expiry := t0.Add(30 * 24 * time.Hour)
	r := res(t0, check.Degraded, 250)
	r.CertExpiry = &expiry
	s.Record(r)
	s.SaveIncident(incident.Incident{ID: "web-1", TargetID: "web", Started: t0, Cause: "x"})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	last, _ := loaded.Last("web")
	if last.Status != check.Degraded || last.Latency != 250*time.Millisecond || !last.CertExpiry.Equal(expiry) {
		t.Errorf("last %+v", last)
	}
	if u, n := loaded.Uptime("web", time.Hour, t0); u != 1 || n != 1 {
		t.Errorf("uptime %v %d", u, n)
	}
	if open, ok := loaded.OpenIncident("web"); !ok || open.Cause != "x" {
		t.Errorf("incident %+v", open)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("left %d files behind", len(entries))
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"broken.json":  "{not json",
		"version.json": `{"version": 99}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || errors.Is(err, ErrNoFile) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
