package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/clock"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/notify"
)

func window(from, to time.Duration, ids ...string) config.Maintenance {
	return config.Maintenance{Title: "Upgrade", Start: config.Time{Time: t0.Add(from)}, End: config.Time{Time: t0.Add(to)}, TargetIDs: ids}
}

func TestFailuresDuringMaintenanceOpenNoIncident(t *testing.T) {
	clk := clock.NewFake(t0)
	checker := &scripted{clk: clk, statuses: []check.Status{check.Down}}
	mon, sent, _ := setup(t, checker)
	mon.Clock = clk
	mon.Maintenance = []config.Maintenance{window(0, 10*time.Minute, "a")}
	events, cancel := mon.Broker.Subscribe()
	defer cancel()

	for range 3 {
		mon.CheckOnce(context.Background(), mon.Targets[0])
		clk.Advance(time.Minute)
	}
	if len(sent.sent) != 0 || len(mon.Store.Incidents()) != 0 {
		t.Fatalf("alerts %+v, incidents %+v", sent.sent, mon.Store.Incidents())
	}
	if last, _ := mon.Store.Last("a"); last.Status != check.Maintenance {
		t.Errorf("stored %+v", last)
	}
	if e := <-events; e.Status != check.Maintenance {
		t.Errorf("event %+v", e)
	}
	if state, _ := mon.Store.Tracker("a"); state.Failures != 0 {
		t.Errorf("the streak grew during maintenance: %+v", state)
	}
	if up, checks := mon.Store.Uptime("a", time.Hour, clk.Now()); up != -1 || checks != 0 {
		t.Errorf("uptime %v over %d checks; maintenance shouldn't count", up, checks)
	}
	var metrics strings.Builder
	_, _ = mon.Metrics.WriteTo(&metrics)
	for _, want := range []string{`pulsewatch_maintenance{target="a"} 1`, `pulsewatch_up{target="a",name="",type=""} 0`, `pulsewatch_checks_total{target="a",status="maintenance"} 3`} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// After the window, failures count again: two in a row open an incident.
	clk.Set(t0.Add(10 * time.Minute))
	mon.CheckOnce(context.Background(), mon.Targets[0])
	mon.CheckOnce(context.Background(), mon.Targets[0])
	if len(sent.sent) != 1 || sent.sent[0].Kind != notify.KindDown {
		t.Fatalf("alerts after the window: %+v", sent.sent)
	}
	metrics.Reset()
	_, _ = mon.Metrics.WriteTo(&metrics)
	if !strings.Contains(metrics.String(), `pulsewatch_maintenance{target="a"} 0`) {
		t.Error("maintenance gauge still on after the window")
	}
}

func TestMaintenanceCoversOnlyItsTargets(t *testing.T) {
	clk := clock.NewFake(t0)
	a := &scripted{clk: clk, statuses: []check.Status{check.Down}}
	b := &scripted{clk: clk, statuses: []check.Status{check.Down}}
	mon, sent, _ := setup(t, a, b)
	mon.Maintenance = []config.Maintenance{window(0, time.Hour, "a")}
	for range 2 {
		mon.CheckOnce(context.Background(), mon.Targets[0])
		mon.CheckOnce(context.Background(), mon.Targets[1])
	}
	if len(sent.sent) != 1 || sent.sent[0].Incident.TargetID != "b" {
		t.Errorf("alerts %+v", sent.sent)
	}
}

func TestGoodChecksDuringMaintenanceCount(t *testing.T) {
	clk := clock.NewFake(t0)
	checker := &scripted{clk: clk, statuses: []check.Status{check.Down, check.Down, check.Up}}
	mon, sent, _ := setup(t, checker)
	mon.Clock = clk
	// An incident opens before the window...
	mon.CheckOnce(context.Background(), mon.Targets[0])
	mon.CheckOnce(context.Background(), mon.Targets[0])
	if len(sent.sent) != 1 {
		t.Fatalf("alerts %+v", sent.sent)
	}
	// ...and a good check during the window resolves it (one success resolves in setup).
	mon.Maintenance = []config.Maintenance{window(0, time.Hour)}
	mon.CheckOnce(context.Background(), mon.Targets[0])
	if last, _ := mon.Store.Last("a"); last.Status != check.Up {
		t.Errorf("a good check became %q", last.Status)
	}
	if len(sent.sent) != 2 || sent.sent[1].Kind != notify.KindRecovered {
		t.Errorf("alerts %+v", sent.sent)
	}
}

func TestOpenIncidentStaysOpenThroughMaintenance(t *testing.T) {
	clk := clock.NewFake(t0)
	checker := &scripted{clk: clk, statuses: []check.Status{check.Down}}
	mon, sent, _ := setup(t, checker)
	mon.Clock = clk
	mon.CheckOnce(context.Background(), mon.Targets[0])
	mon.CheckOnce(context.Background(), mon.Targets[0])
	mon.Maintenance = []config.Maintenance{window(0, time.Hour, "a")}
	mon.CheckOnce(context.Background(), mon.Targets[0])
	open, ok := mon.Store.OpenIncident("a")
	if !ok || open.Checks != 2 || len(sent.sent) != 1 {
		t.Errorf("incident %+v (open %v), alerts %d", open, ok, len(sent.sent))
	}
}
