package clock

import (
	"testing"
	"time"
)

func TestFake(t *testing.T) {
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)
	if !f.Now().Equal(start) {
		t.Fatal("now")
	}
	soon, later := f.After(time.Second), f.After(time.Minute)
	if !f.BlockUntil(2, time.Second) {
		t.Fatal("waiters not seen")
	}
	f.Advance(time.Second)
	select {
	case at := <-soon:
		if !at.Equal(start.Add(time.Second)) {
			t.Errorf("woke at %v", at)
		}
	default:
		t.Fatal("not woken")
	}
	select {
	case <-later:
		t.Fatal("woken too early")
	default:
	}
	f.Advance(time.Hour)
	<-later
	if now := <-f.After(0); !now.Equal(start.Add(time.Hour + time.Second)) {
		t.Errorf("After(0) gave %v", now)
	}
	if f.BlockUntil(1, 10*time.Millisecond) {
		t.Error("a waiter is left")
	}
	f.Set(start)
	if !f.Now().Equal(start) {
		t.Error("set")
	}
}

func TestReal(t *testing.T) {
	var c Clock = Real{}
	if time.Since(c.Now()) > time.Second {
		t.Error("now")
	}
	<-c.After(time.Millisecond)
}
