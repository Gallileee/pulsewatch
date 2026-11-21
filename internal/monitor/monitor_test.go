package monitor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/clock"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/incident"
	"github.com/Gallileee/pulsewatch/internal/metrics"
	"github.com/Gallileee/pulsewatch/internal/notify"
	"github.com/Gallileee/pulsewatch/internal/store"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// scripted answers with the next status of its list each time (the last one repeats).
type scripted struct {
	mu       sync.Mutex
	clk      clock.Clock
	statuses []check.Status
	calls    int
}

func (s *scripted) Check(context.Context) check.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.statuses[min(s.calls, len(s.statuses)-1)]
	s.calls++
	return check.Result{At: s.clk.Now(), Status: st, Message: "status " + string(st), Latency: time.Millisecond}
}

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type alerts struct {
	mu   sync.Mutex
	sent []notify.Alert
}

func (a *alerts) Send(al notify.Alert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, al)
}

func setup(t *testing.T, checkers ...check.Checker) (*Monitor, *alerts, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(t0)
	cfg := &config.Config{MaxConcurrent: 10, PublicURL: "https://status.test",
		Incidents: config.Incident{FailuresToOpen: 2, SuccessesToResolve: 1}}
	var described []metrics.Target
	for i := range checkers {
		id := string(rune('a' + i))
		cfg.Targets = append(cfg.Targets, config.Target{ID: id, Name: "Target " + id, Interval: config.Duration(time.Minute)})
		described = append(described, metrics.Target{ID: id})
	}
	sent := &alerts{}
	return New(cfg, checkers, store.New(), metrics.New("test", described), sent, clk, nil), sent, clk
}

func TestCheckOncePassesTheResultOn(t *testing.T) {
	clk := clock.NewFake(t0)
	checker := &scripted{clk: clk, statuses: []check.Status{check.Down, check.Down, check.Down, check.Up}}
	mon, sent, _ := setup(t, checker)
	mon.Clock = clk
	events, cancel := mon.Broker.Subscribe()
	defer cancel()
	target := mon.Targets[0]

	mon.CheckOnce(context.Background(), target)
	if last, ok := mon.Store.Last("a"); !ok || last.TargetID != "a" || last.Status != check.Down {
		t.Errorf("stored %+v", last)
	}
	if e := <-events; e.TargetID != "a" || e.Status != check.Down {
		t.Errorf("event %+v", e)
	}
	if len(sent.sent) != 0 || len(mon.Store.Incidents()) != 0 {
		t.Fatal("an incident after one failure")
	}

	mon.CheckOnce(context.Background(), target)
	if len(sent.sent) != 1 || sent.sent[0].Kind != notify.KindDown || sent.sent[0].StatusPage != "https://status.test" {
		t.Fatalf("alerts %+v", sent.sent)
	}
	if open, ok := mon.Store.OpenIncident("a"); !ok || open.Checks != 2 {
		t.Errorf("incident %+v", open)
	}

	mon.CheckOnce(context.Background(), target) // still down: the incident is updated, no alert
	if open, _ := mon.Store.OpenIncident("a"); open.Checks != 3 || len(sent.sent) != 1 {
		t.Errorf("incident %+v, %d alerts", open, len(sent.sent))
	}

	mon.CheckOnce(context.Background(), target)
	if len(sent.sent) != 2 || sent.sent[1].Kind != notify.KindRecovered {
		t.Fatalf("alerts %+v", sent.sent)
	}
	if _, ok := mon.Store.OpenIncident("a"); ok {
		t.Error("still open")
	}
}

func TestResumesAnOpenIncident(t *testing.T) {
	clk := clock.NewFake(t0)
	st := store.New()
	st.SaveIncident(incident.Incident{ID: "a-1", TargetID: "a", Started: t0.Add(-time.Hour)})
	cfg := &config.Config{MaxConcurrent: 1, Incidents: config.Incident{FailuresToOpen: 2, SuccessesToResolve: 1},
		Targets: []config.Target{{ID: "a", Name: "A", Interval: config.Duration(time.Minute)}}}
	sent := &alerts{}
	mon := New(cfg, []check.Checker{&scripted{clk: clk, statuses: []check.Status{check.Up}}}, st,
		metrics.New("test", []metrics.Target{{ID: "a"}}), sent, clk, nil)
	mon.CheckOnce(context.Background(), mon.Targets[0])
	if len(sent.sent) != 1 || sent.sent[0].Kind != notify.KindRecovered || sent.sent[0].Incident.ID != "a-1" {
		t.Errorf("alerts %+v", sent.sent)
	}
}

func TestRunChecksAtEachInterval(t *testing.T) {
	clk := clock.NewFake(t0)
	a := &scripted{clk: clk, statuses: []check.Status{check.Up}}
	b := &scripted{clk: clk, statuses: []check.Status{check.Up}}
	mon, _, _ := setup(t, a, b)
	mon.Clock = clk
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mon.Run(ctx)
		close(done)
	}()
	// a runs at once and waits a minute; b waits for its offset (half the interval) first.
	if !clk.BlockUntil(2, time.Second) {
		t.Fatal("the loops didn't start")
	}
	if a.count() != 1 || b.count() != 0 {
		t.Fatalf("at start: a %d, b %d", a.count(), b.count())
	}
	clk.Advance(30 * time.Second)
	waitFor(t, func() bool { return b.count() == 1 })
	if !clk.BlockUntil(2, time.Second) {
		t.Fatal("b didn't wait again")
	}
	clk.Advance(30 * time.Second)
	waitFor(t, func() bool { return a.count() == 2 })
	cancel()
	<-done
}

func TestConcurrencyIsLimited(t *testing.T) {
	release := make(chan struct{})
	var running, peak atomic.Int32
	var checkers []check.Checker
	for range 5 {
		checkers = append(checkers, checkFunc(func() check.Result {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			running.Add(-1)
			return check.Result{Status: check.Up}
		}))
	}
	mon, _, _ := setup(t, checkers...)
	mon.concurrent = make(chan struct{}, 2)
	var wg sync.WaitGroup
	for _, target := range mon.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mon.CheckOnce(context.Background(), target)
		}()
	}
	waitFor(t, func() bool { return running.Load() == 2 })
	time.Sleep(20 * time.Millisecond) // give the others a chance to wrongly start
	if peak.Load() != 2 {
		t.Errorf("peak %d", peak.Load())
	}
	close(release)
	wg.Wait()
	if peak.Load() != 2 {
		t.Errorf("peak %d", peak.Load())
	}
}

func TestAStoppedCheckIsDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mon, _, _ := setup(t, checkFunc(func() check.Result {
		cancel()
		return check.Result{Status: check.Down}
	}))
	mon.CheckOnce(ctx, mon.Targets[0])
	if _, ok := mon.Store.Last("a"); ok {
		t.Error("a check cut short by stopping was recorded")
	}
}

func TestBroker(t *testing.T) {
	b := NewBroker()
	ch, cancel := b.Subscribe()
	if b.Subscribers() != 1 {
		t.Fatal("not subscribed")
	}
	for range 20 { // more than the buffer: the rest are dropped, nothing blocks
		b.Publish(Event{TargetID: "a"})
	}
	if len(ch) != 16 {
		t.Errorf("%d buffered", len(ch))
	}
	cancel()
	cancel()
	if b.Subscribers() != 0 {
		t.Error("still subscribed")
	}
	b.Publish(Event{})
}

type checkFunc func() check.Result

func (f checkFunc) Check(context.Context) check.Result { return f() }

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}
