package config

import (
	"strings"
	"testing"
	"time"
)

const twoTargets = `
targets:
  - name: Website
    url: https://example.com
  - name: Search API
    url: https://example.com/search
`

func TestParseMaintenance(t *testing.T) {
	cfg, err := Parse([]byte(twoTargets + `
maintenance:
  - title: Database upgrade
    start: 2026-11-24T22:00:00Z
    end: "2026-11-25T00:30:00+01:00"
    targets: [Search API]
    note: Search may be unavailable.
  - title: Everything
    start: 2026-11-26T08:00:00Z
    end: 2026-11-26T09:00:00Z
`))
	if err != nil {
		t.Fatal(err)
	}
	db, all := cfg.Maintenance[0], cfg.Maintenance[1]
	if !db.Start.Equal(time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC)) ||
		!db.End.Equal(time.Date(2026, 11, 24, 23, 30, 0, 0, time.UTC)) {
		t.Errorf("times %v %v", db.Start, db.End)
	}
	if len(db.TargetIDs) != 1 || db.TargetIDs[0] != "search-api" || db.Note != "Search may be unavailable." {
		t.Errorf("db %+v", db)
	}
	if !db.Covers("search-api") || db.Covers("website") || !all.Covers("website") {
		t.Error("covers")
	}
}

func TestMaintenanceActive(t *testing.T) {
	start := time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC)
	w := Maintenance{Start: Time{start}, End: Time{start.Add(time.Hour)}, TargetIDs: []string{"a"}}
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{start.Add(-time.Nanosecond), false},
		{start, true},
		{start.Add(59 * time.Minute), true},
		{start.Add(time.Hour), false}, // the end isn't part of it
	} {
		if got := w.Active(c.at); got != c.want {
			t.Errorf("Active(%v) = %v", c.at, got)
		}
	}
	windows := []Maintenance{w, {Title: "all", Start: Time{start.Add(2 * time.Hour)}, End: Time{start.Add(3 * time.Hour)}}}
	if _, ok := InMaintenance(windows, "b", start.Add(time.Minute)); ok {
		t.Error("b isn't covered by the first window")
	}
	if got, ok := InMaintenance(windows, "b", start.Add(150*time.Minute)); !ok || got.Title != "all" {
		t.Errorf("b in the second window: %+v %v", got, ok)
	}
}

func TestMaintenanceValidation(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"  - title: x\n    start: 2026-11-24T22:00:00\n    end: 2026-11-24T23:00:00Z\n", "RFC 3339, with a zone"},
		{"  - title: x\n    start: tomorrow\n    end: 2026-11-24T23:00:00Z\n", `"tomorrow" isn't a time`},
		{"  - start: 2026-11-24T22:00:00Z\n    end: 2026-11-24T23:00:00Z\n", "needs a title"},
		{"  - title: x\n    start: 2026-11-24T22:00:00Z\n", "needs a start and an end"},
		{"  - title: x\n    start: 2026-11-24T22:00:00Z\n    end: 2026-11-24T22:00:00Z\n", "end must be after start"},
		{"  - title: x\n    start: 2026-11-01T00:00:00Z\n    end: 2026-11-09T00:00:00Z\n", "longer than 7 days"},
		{"  - title: x\n    start: 2026-11-24T22:00:00Z\n    end: 2026-11-24T23:00:00Z\n    targets: [Website, Nope]\n", `no target named "Nope"`},
	} {
		_, err := Parse([]byte(twoTargets + "maintenance:\n" + c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.yaml, err, c.want)
		}
	}
}

func TestMaintenanceExpandsVariables(t *testing.T) {
	t.Setenv("WHO", "the platform team")
	cfg, err := Parse([]byte(twoTargets + `
maintenance:
  - title: Upgrade
    start: 2026-11-24T22:00:00Z
    end: 2026-11-24T23:00:00Z
    note: Ask ${WHO}.
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Maintenance[0].Note != "Ask the platform team." {
		t.Errorf("note %q", cfg.Maintenance[0].Note)
	}
}
