// Package monitor runs the checks on schedule and passes each result on: to the store, the
// metrics, the incident trackers, the alerts and whoever is listening for live updates.
package monitor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/clock"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/incident"
	"github.com/Gallileee/pulsewatch/internal/metrics"
	"github.com/Gallileee/pulsewatch/internal/notify"
	"github.com/Gallileee/pulsewatch/internal/store"
)

// Target is a configured target with its checker.
type Target struct {
	Config  config.Target
	Checker check.Checker
	tracker *incident.Tracker
}

// Alerter sends alerts (notify.Dispatcher is one).
type Alerter interface {
	Send(a notify.Alert)
}

// Monitor runs the checks.
type Monitor struct {
	Targets    []*Target
	Store      *store.Store
	Metrics    *metrics.Metrics
	Broker     *Broker
	Alerter    Alerter
	Clock      clock.Clock
	Log        *slog.Logger
	PublicURL  string
	concurrent chan struct{}
}

// New makes a monitor; checkers holds one checker per target, in the config's order.
func New(cfg *config.Config, checkers []check.Checker, st *store.Store, m *metrics.Metrics, alerter Alerter, clk clock.Clock, log *slog.Logger) *Monitor {
	mon := &Monitor{
		Store: st, Metrics: m, Broker: NewBroker(), Alerter: alerter, Clock: clk, Log: log,
		PublicURL: cfg.PublicURL, concurrent: make(chan struct{}, cfg.MaxConcurrent),
	}
	for i, t := range cfg.Targets {
		tracker := incident.NewTracker(t.ID, t.Name, cfg.Incidents.FailuresToOpen, cfg.Incidents.SuccessesToResolve)
		if state, ok := st.Tracker(t.ID); ok {
			tracker.Restore(state) // carry on: the failure streak, the open incident
		} else if open, ok := st.OpenIncident(t.ID); ok {
			tracker.Resume(open) // it was down when we stopped: a recovery should close it
		}
		if tracker.Current() != nil {
			m.SetIncident(t.ID, true)
		}
		mon.Targets = append(mon.Targets, &Target{Config: t, Checker: checkers[i], tracker: tracker})
	}
	return mon
}

// Run checks every target at its interval until ctx is done. The targets start spread over
// their interval instead of all at once.
func (m *Monitor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i, t := range m.Targets {
		offset := time.Duration(t.Config.Interval) * time.Duration(i) / time.Duration(len(m.Targets))
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.loop(ctx, t, offset)
		}()
	}
	wg.Wait()
}

// RunOnce checks every target once (as many at a time as the limit allows) and returns when
// all are done: for running from a scheduler such as cron or GitHub Actions.
func (m *Monitor) RunOnce(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range m.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.CheckOnce(ctx, t)
		}()
	}
	wg.Wait()
}

func (m *Monitor) loop(ctx context.Context, t *Target, offset time.Duration) {
	if offset > 0 {
		select {
		case <-ctx.Done():
			return
		case <-m.Clock.After(offset):
		}
	}
	for {
		m.CheckOnce(ctx, t)
		select {
		case <-ctx.Done():
			return
		case <-m.Clock.After(time.Duration(t.Config.Interval)):
		}
	}
}

// CheckOnce checks one target now (waiting for a free slot) and passes the result on.
func (m *Monitor) CheckOnce(ctx context.Context, t *Target) {
	select {
	case m.concurrent <- struct{}{}:
	case <-ctx.Done():
		return
	}
	r := t.Checker.Check(ctx)
	<-m.concurrent
	if ctx.Err() != nil {
		return // stopped while checking: the result says nothing about the target
	}
	r.TargetID = t.Config.ID
	m.Store.Record(r)
	m.Metrics.Observe(r)
	if r.Status != check.Up && m.Log != nil {
		m.Log.Info("check", "target", t.Config.ID, "status", r.Status, "message", r.Message)
	}
	change := t.tracker.Observe(r)
	m.Store.SetTracker(t.Config.ID, t.tracker.State())
	switch {
	case change.Opened != nil:
		m.Store.SaveIncident(*change.Opened)
		m.Metrics.SetIncident(t.Config.ID, true)
		m.alert(notify.KindDown, *change.Opened)
	case change.Resolved != nil:
		m.Store.SaveIncident(*change.Resolved)
		m.Metrics.SetIncident(t.Config.ID, false)
		m.alert(notify.KindRecovered, *change.Resolved)
	case change.Updated != nil:
		m.Store.SaveIncident(*change.Updated)
	}
	m.Broker.Publish(Event{TargetID: t.Config.ID, Status: r.Status, At: r.At})
}

func (m *Monitor) alert(kind string, inc incident.Incident) {
	if m.Log != nil {
		m.Log.Warn("incident "+kind, "target", inc.TargetID, "incident", inc.ID, "cause", inc.Cause)
	}
	if m.Alerter != nil {
		m.Alerter.Send(notify.Alert{Kind: kind, Incident: inc, StatusPage: m.PublicURL})
	}
}

// Event says a target was just checked.
type Event struct {
	TargetID string       `json:"target"`
	Status   check.Status `json:"status"`
	At       time.Time    `json:"at"`
}

// Broker passes events to subscribers. A subscriber that doesn't keep up misses events rather
// than holding up the checks.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBroker makes a broker.
func NewBroker() *Broker { return &Broker{subs: map[chan Event]struct{}{}} }

// Subscribe returns a channel of events and a function that ends the subscription.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
		})
	}
}

// Publish sends an event to every subscriber with room for it.
func (b *Broker) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribers is how many are listening.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
