package incident

import (
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func result(minute int, status check.Status, message string) check.Result {
	return check.Result{TargetID: "web", At: t0.Add(time.Duration(minute) * time.Minute), Status: status, Message: message}
}

func TestOpensAfterEnoughFailuresAndStartsAtTheFirst(t *testing.T) {
	tr := NewTracker("web", "Website", 3, 2)
	if c := tr.Observe(result(0, check.Down, "HTTP 502")); c.Opened != nil {
		t.Fatal("opened after one failure")
	}
	if c := tr.Observe(result(1, check.Down, "HTTP 503")); c.Opened != nil {
		t.Fatal("opened after two failures")
	}
	c := tr.Observe(result(2, check.Down, "timed out"))
	if c.Opened == nil {
		t.Fatal("not opened after three failures")
	}
	inc := *c.Opened
	if !inc.Started.Equal(t0) || inc.Cause != "HTTP 502" || inc.LastMessage != "timed out" || inc.Checks != 3 ||
		inc.TargetName != "Website" || !inc.Open() || inc.ID != "web-1795262400" {
		t.Errorf("%+v", inc)
	}
	c = tr.Observe(result(3, check.Down, "refused"))
	if c.Updated == nil || c.Updated.Checks != 4 || c.Updated.LastMessage != "refused" || c.Opened != nil {
		t.Errorf("%+v", c)
	}
}

func TestAFailureStreakIsBrokenByAGoodCheck(t *testing.T) {
	tr := NewTracker("web", "Website", 2, 1)
	tr.Observe(result(0, check.Down, "a"))
	tr.Observe(result(1, check.Up, ""))
	if c := tr.Observe(result(2, check.Down, "b")); c.Opened != nil {
		t.Fatal("opened from a broken streak")
	}
	c := tr.Observe(result(3, check.Down, "c"))
	if c.Opened == nil || c.Opened.Cause != "b" || !c.Opened.Started.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("%+v", c.Opened)
	}
}

func TestResolvesAfterEnoughGoodChecksFromTheFirst(t *testing.T) {
	tr := NewTracker("web", "Website", 1, 2)
	tr.Observe(result(0, check.Down, "x"))
	if c := tr.Observe(result(1, check.Degraded, "slow")); c.Resolved != nil {
		t.Fatal("resolved after one good check")
	}
	// A failure in between starts the count again.
	tr.Observe(result(2, check.Down, "x"))
	tr.Observe(result(3, check.Up, ""))
	c := tr.Observe(result(4, check.Up, ""))
	if c.Resolved == nil {
		t.Fatal("not resolved")
	}
	if !c.Resolved.Resolved.Equal(t0.Add(3*time.Minute)) || c.Resolved.Open() {
		t.Errorf("resolved at %v", c.Resolved.Resolved)
	}
	if got := c.Resolved.Duration(t0.Add(time.Hour)); got != 3*time.Minute {
		t.Errorf("duration %v", got)
	}
	if tr.Current() != nil {
		t.Error("still open")
	}
	if c := tr.Observe(result(5, check.Up, "")); c != (Change{}) {
		t.Errorf("%+v", c)
	}
}

func TestResume(t *testing.T) {
	tr := NewTracker("web", "Website", 2, 1)
	open := Incident{ID: "web-1", TargetID: "web", Started: t0, Cause: "x"}
	tr.Resume(open)
	if c := tr.Observe(result(1, check.Down, "y")); c.Updated == nil || c.Updated.ID != "web-1" {
		t.Errorf("%+v", c)
	}
	if c := tr.Observe(result(2, check.Up, "")); c.Resolved == nil || c.Resolved.ID != "web-1" {
		t.Errorf("%+v", c)
	}
	// Another target's or a closed incident isn't resumed.
	other := NewTracker("api", "API", 2, 1)
	other.Resume(open)
	resolvedAt := t0
	other.Resume(Incident{ID: "api-1", TargetID: "api", Resolved: &resolvedAt})
	if other.Current() != nil {
		t.Error("resumed")
	}
}

func TestCurrentIsACopy(t *testing.T) {
	tr := NewTracker("web", "Website", 1, 1)
	tr.Observe(result(0, check.Down, "x"))
	cur := tr.Current()
	cur.Cause = "changed"
	if tr.Current().Cause != "x" {
		t.Error("the tracker's incident changed")
	}
}
