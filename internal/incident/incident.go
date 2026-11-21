// Package incident decides when a target's failures become an outage and when it's over.
//
// One failed check isn't an outage: networks hiccup. An incident opens after FailuresToOpen
// failed checks in a row (it starts at the first of them) and is resolved after
// SuccessesToResolve good checks in a row. A degraded check counts as good.
package incident

import (
	"fmt"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
)

// Incident is one outage of one target.
type Incident struct {
	ID         string     `json:"id"`
	TargetID   string     `json:"target"`
	TargetName string     `json:"target_name"`
	Started    time.Time  `json:"started"`
	Resolved   *time.Time `json:"resolved,omitempty"`
	// Cause is the message of the first failed check; LastMessage the latest one.
	Cause       string `json:"cause"`
	LastMessage string `json:"last_message"`
	Checks      int    `json:"failed_checks"`
}

// Open reports whether the incident is still going on.
func (i Incident) Open() bool { return i.Resolved == nil }

// Duration is how long it lasted, or has lasted until now.
func (i Incident) Duration(now time.Time) time.Duration {
	if i.Resolved != nil {
		return i.Resolved.Sub(i.Started)
	}
	return now.Sub(i.Started)
}

// Change is what one check did to the target's incident.
type Change struct {
	Opened   *Incident // an incident just opened
	Resolved *Incident // an incident just ended
	Updated  *Incident // the open incident got another failed check
}

// Tracker follows one target's checks.
type Tracker struct {
	targetID, targetName string
	failuresToOpen       int
	successesToResolve   int

	failures, successes int
	firstFailure        check.Result // the first failed check of the current streak
	firstSuccess        time.Time    // the first good check of the current streak
	current             *Incident
}

// NewTracker makes a tracker for one target.
func NewTracker(targetID, targetName string, failuresToOpen, successesToResolve int) *Tracker {
	return &Tracker{
		targetID: targetID, targetName: targetName,
		failuresToOpen: max(1, failuresToOpen), successesToResolve: max(1, successesToResolve),
	}
}

// State is what a tracker remembers between checks, saved so a restart (or the next run of
// pulsewatch -once) carries on where it stopped.
type State struct {
	Failures     int           `json:"failures"`
	Successes    int           `json:"successes"`
	FirstFailure *check.Result `json:"first_failure,omitempty"`
	FirstSuccess time.Time     `json:"first_success,omitzero"`
	Current      *Incident     `json:"current,omitempty"`
}

// State returns the tracker's state.
func (t *Tracker) State() State {
	s := State{Failures: t.failures, Successes: t.successes, FirstSuccess: t.firstSuccess, Current: t.Current()}
	if t.failures > 0 {
		first := t.firstFailure
		s.FirstFailure = &first
	}
	return s
}

// Restore continues from a saved state.
func (t *Tracker) Restore(s State) {
	t.failures, t.successes, t.firstSuccess = s.Failures, s.Successes, s.FirstSuccess
	t.firstFailure = check.Result{}
	if s.FirstFailure != nil {
		t.firstFailure = *s.FirstFailure
	}
	t.current = nil
	if s.Current != nil && s.Current.Open() && s.Current.TargetID == t.targetID {
		current := *s.Current
		t.current = &current
	}
}

// Resume continues an incident that was open before a restart, so a recovery closes it.
func (t *Tracker) Resume(open Incident) {
	if open.Open() && open.TargetID == t.targetID {
		t.current = &open
		t.failures = t.failuresToOpen
	}
}

// Current is the open incident, if any.
func (t *Tracker) Current() *Incident {
	if t.current == nil {
		return nil
	}
	copied := *t.current
	return &copied
}

// Observe takes the next check of the target.
func (t *Tracker) Observe(r check.Result) Change {
	if r.Status == check.Down {
		t.successes = 0
		t.failures++
		if t.failures == 1 {
			t.firstFailure = r
		}
		if t.current != nil {
			t.current.LastMessage = r.Message
			t.current.Checks++
			return Change{Updated: t.Current()}
		}
		if t.failures >= t.failuresToOpen {
			first := t.firstFailure
			t.current = &Incident{
				ID:          fmt.Sprintf("%s-%d", t.targetID, first.At.Unix()),
				TargetID:    t.targetID,
				TargetName:  t.targetName,
				Started:     first.At,
				Cause:       first.Message,
				LastMessage: r.Message,
				Checks:      t.failures,
			}
			return Change{Opened: t.Current()}
		}
		return Change{}
	}
	t.failures = 0
	t.successes++
	if t.successes == 1 {
		t.firstSuccess = r.At
	}
	if t.current != nil && t.successes >= t.successesToResolve {
		at := t.firstSuccess // it was back from the first good check
		t.current.Resolved = &at
		resolved := t.Current()
		t.current = nil
		return Change{Resolved: resolved}
	}
	return Change{}
}
