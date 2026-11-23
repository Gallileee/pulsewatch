package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
)

func TestMaintenanceChecksDontCountForUptime(t *testing.T) {
	s := New()
	s.Record(res(t0, check.Up, 100))
	s.Record(res(t0.Add(time.Minute), check.Maintenance, 5000))
	s.Record(res(t0.Add(2*time.Minute), check.Maintenance, 5000))
	s.Record(res(t0.Add(3*time.Minute), check.Up, 100))

	h := s.series["web"].Hours[0]
	if h.Checks != 2 || h.Good() != 2 || h.Maintenance != 2 || h.LatencyMs != 200 {
		t.Errorf("hour %+v", h)
	}
	if up, checks := s.Uptime("web", time.Hour, t0.Add(time.Hour)); up != 1 || checks != 2 {
		t.Errorf("uptime %v over %d", up, checks)
	}
	days := s.Days("web", 2, t0)
	if today := days[1]; today.Checks != 2 || today.Maintenance != 2 || today.Uptime() != 1 {
		t.Errorf("today %+v", today)
	}
	if recent := s.Recent("web"); len(recent) != 4 || recent[1].Status != check.Maintenance {
		t.Errorf("recent %+v", recent)
	}
	if last, _ := s.Last("web"); last.Status != check.Up {
		t.Errorf("last %+v", last)
	}

	path := filepath.Join(t.TempDir(), "history.json")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.series["web"].Hours[0].Maintenance != 2 {
		t.Errorf("loaded %+v %v", loaded.series["web"], err)
	}
}
