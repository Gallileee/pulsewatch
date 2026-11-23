// Package store keeps the check history: the latest checks of each target, hourly totals for
// 90 days (uptime and latency), and the incidents. It lives in memory and is saved to a JSON
// file, so a restart keeps the history.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/incident"
)

// Limits.
const (
	RecentChecks = 60                  // checks kept one by one per target
	Retention    = 90 * 24 * time.Hour // how long the hourly totals are kept
	MaxIncidents = 200
	fileVersion  = 1
)

// Point is one recent check, for the latency chart.
type Point struct {
	At        time.Time    `json:"at"`
	Status    check.Status `json:"status"`
	LatencyMs float64      `json:"latency_ms"`
}

// Hour is the totals of one target's checks in one hour (UTC).
type Hour struct {
	Start     int64   `json:"start"` // Unix seconds, a whole hour
	Checks    int     `json:"checks"`
	Up        int     `json:"up"`
	Degraded  int     `json:"degraded"`
	LatencyMs float64 `json:"latency_ms"` // the sum
	// Maintenance counts the failed checks during planned maintenance; they aren't in Checks,
	// so they don't count against the uptime.
	Maintenance int `json:"maintenance,omitempty"`
}

// Good is the number of checks that weren't down.
func (h Hour) Good() int { return h.Up + h.Degraded }

// Series is one target's history.
type Series struct {
	Last   *check.Result `json:"last,omitempty"`
	Recent []Point       `json:"recent"`
	Hours  []Hour        `json:"hours"` // oldest first
}

// Store holds every target's history.
type Store struct {
	mu        sync.RWMutex
	series    map[string]*Series
	incidents []incident.Incident // oldest first
	trackers  map[string]incident.State
}

// New returns an empty store.
func New() *Store {
	return &Store{series: map[string]*Series{}, trackers: map[string]incident.State{}}
}

// SetTracker keeps a target's incident tracker state.
func (s *Store) SetTracker(id string, state incident.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trackers[id] = state
}

// Tracker is a target's saved incident tracker state.
func (s *Store) Tracker(id string) (incident.State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.trackers[id]
	return state, ok
}

// Record adds a check.
func (s *Store) Record(r check.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sr := s.series[r.TargetID]
	if sr == nil {
		sr = &Series{}
		s.series[r.TargetID] = sr
	}
	if sr.Last != nil && r.At.Before(sr.Last.At) {
		return // older than what we have: a clock jump; ignore it
	}
	last := r
	sr.Last = &last
	ms := float64(r.Latency) / float64(time.Millisecond)
	sr.Recent = append(sr.Recent, Point{At: r.At, Status: r.Status, LatencyMs: ms})
	if len(sr.Recent) > RecentChecks {
		sr.Recent = slices.Clone(sr.Recent[len(sr.Recent)-RecentChecks:])
	}
	start := r.At.UTC().Truncate(time.Hour).Unix()
	if n := len(sr.Hours); n == 0 || sr.Hours[n-1].Start != start {
		sr.Hours = append(sr.Hours, Hour{Start: start})
	}
	h := &sr.Hours[len(sr.Hours)-1]
	switch r.Status {
	case check.Maintenance:
		h.Maintenance++
	case check.Up:
		h.Up++
	case check.Degraded:
		h.Degraded++
	}
	if r.Status != check.Maintenance {
		h.Checks++
		h.LatencyMs += ms
	}
	cutoff := r.At.Add(-Retention).Unix()
	drop := 0
	for drop < len(sr.Hours) && sr.Hours[drop].Start+3600 <= cutoff {
		drop++
	}
	if drop > 0 {
		sr.Hours = slices.Clone(sr.Hours[drop:])
	}
}

// Last is the target's latest check.
func (s *Store) Last(id string) (check.Result, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sr := s.series[id]; sr != nil && sr.Last != nil {
		return *sr.Last, true
	}
	return check.Result{}, false
}

// Recent is the target's latest checks, oldest first.
func (s *Store) Recent(id string) []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sr := s.series[id]; sr != nil {
		return slices.Clone(sr.Recent)
	}
	return nil
}

// Uptime is the share of good checks in the hours that overlap the window ending at now, and
// the number of checks; with no checks the share is -1. It counts whole hours, so a window of
// 24h may include up to one more hour.
func (s *Store) Uptime(id string, window time.Duration, now time.Time) (float64, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sr := s.series[id]
	if sr == nil {
		return -1, 0
	}
	from := now.Add(-window).UTC().Truncate(time.Hour).Unix()
	checks, good := 0, 0
	for _, h := range sr.Hours {
		if h.Start >= from && h.Start <= now.Unix() {
			checks += h.Checks
			good += h.Good()
		}
	}
	if checks == 0 {
		return -1, 0
	}
	return float64(good) / float64(checks), checks
}

// Day is one day's totals (UTC).
type Day struct {
	Date   time.Time // midnight UTC
	Checks int
	Good   int
	// Maintenance is the failed checks during planned maintenance (not in Checks).
	Maintenance int
}

// Uptime is the share of good checks, or -1 with none.
func (d Day) Uptime() float64 {
	if d.Checks == 0 {
		return -1
	}
	return float64(d.Good) / float64(d.Checks)
}

// Days is the totals of the last n days (UTC), oldest first, the last one being today.
func (s *Store) Days(id string, n int, now time.Time) []Day {
	s.mu.RLock()
	defer s.mu.RUnlock()
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	days := make([]Day, n)
	for i := range days {
		days[i].Date = today.AddDate(0, 0, i-n+1)
	}
	sr := s.series[id]
	if sr == nil {
		return days
	}
	first := days[0].Date.Unix()
	for _, h := range sr.Hours {
		if h.Start < first {
			continue
		}
		i := int((h.Start - first) / 86400)
		if i >= n {
			continue
		}
		days[i].Checks += h.Checks
		days[i].Good += h.Good()
		days[i].Maintenance += h.Maintenance
	}
	return days
}

// SaveIncident adds an incident, or replaces the one with its ID.
func (s *Store) SaveIncident(inc incident.Incident) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.incidents, func(x incident.Incident) bool { return x.ID == inc.ID }); i >= 0 {
		s.incidents[i] = inc
		return
	}
	s.incidents = append(s.incidents, inc)
	if len(s.incidents) > MaxIncidents {
		s.incidents = slices.Clone(s.incidents[len(s.incidents)-MaxIncidents:])
	}
}

// Incidents is every incident, newest first.
func (s *Store) Incidents() []incident.Incident {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Clone(s.incidents)
	slices.Reverse(out)
	return out
}

// OpenIncident is the target's incident that is still going on, if any.
func (s *Store) OpenIncident(id string) (incident.Incident, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.incidents) - 1; i >= 0; i-- {
		if inc := s.incidents[i]; inc.TargetID == id && inc.Open() {
			return inc, true
		}
	}
	return incident.Incident{}, false
}

// Forget drops the history of targets that aren't in keep (removed from the config); their
// incidents stay.
func (s *Store) Forget(keep []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.series {
		if !slices.Contains(keep, id) {
			delete(s.series, id)
			delete(s.trackers, id)
		}
	}
}

type file struct {
	Version   int                       `json:"version"`
	Series    map[string]*Series        `json:"series"`
	Incidents []incident.Incident       `json:"incidents"`
	Trackers  map[string]incident.State `json:"trackers,omitempty"`
}

// Save writes the store to path: to a temporary file first, then renamed, so a crash while
// writing leaves the old file.
func (s *Store) Save(path string) error {
	s.mu.RLock()
	data, err := json.Marshal(file{Version: fileVersion, Series: s.series, Incidents: s.incidents, Trackers: s.trackers})
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // after the rename there is nothing to remove
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ErrNoFile means there was nothing to load: a first start.
var ErrNoFile = errors.New("no saved history")

// Load reads a store saved by Save.
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), ErrNoFile
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("%s: version %d, this build reads version %d", path, f.Version, fileVersion)
	}
	s := New()
	for id, sr := range f.Series {
		if sr != nil {
			s.series[id] = sr
		}
	}
	s.incidents = f.Incidents
	for id, state := range f.Trackers {
		s.trackers[id] = state
	}
	return s, nil
}
