package web

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/incident"
	"github.com/Gallileee/pulsewatch/internal/notify"
	"github.com/Gallileee/pulsewatch/internal/store"
)

// DaysShown is how many days the uptime bars cover.
const DaysShown = 90

// Page is what the status page shows.
type Page struct {
	Title   string       `json:"title"`
	Overall Overall      `json:"overall"`
	Targets []TargetView `json:"targets"`
	// Maintenance is the planned downtime going on now and announced for later.
	Maintenance []MaintenanceView `json:"maintenance"`
	// PastMaintenance is the windows that ended in the last 90 days, the latest first.
	PastMaintenance []MaintenanceView `json:"past_maintenance"`
	Incidents       []IncidentView    `json:"incidents"`
	// History is the incidents and the past maintenance together, the latest start first.
	History   []HistoryEntry `json:"-"`
	Generated time.Time      `json:"generated"`
	// Static: written as files (no server for live updates, the JSON or the metrics).
	Static bool `json:"-"`
}

// Overall is the banner at the top.
type Overall struct {
	Status string `json:"status"` // up, degraded, maintenance, partial, major, unknown
	Text   string `json:"text"`
}

// TargetView is one target's card.
type TargetView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Status     string `json:"status"` // up, degraded, maintenance, down, unknown
	StatusText string `json:"status_text"`
	Message    string `json:"message,omitempty"`
	// Maintenance is the title of the maintenance going on for it now, MaintenanceEnd its end.
	Maintenance    string    `json:"maintenance,omitempty"`
	MaintenanceEnd time.Time `json:"maintenance_end,omitzero"`
	LastChecked    time.Time `json:"last_checked,omitzero"`
	LatencyMs      float64   `json:"latency_ms"`
	Uptime24h      float64   `json:"uptime_24h"` // a share; -1 with no data
	Uptime7d       float64   `json:"uptime_7d"`
	Uptime90d      float64   `json:"uptime_90d"`
	Days           []DayView `json:"days"`
	// Spark is the SVG path of the recent latencies, Spark max their highest (ms).
	Spark    string        `json:"-"`
	SparkMax float64       `json:"-"`
	Recent   []store.Point `json:"recent"`
}

// DayView is one bar.
type DayView struct {
	Date   string  `json:"date"`
	Uptime float64 `json:"uptime"` // -1 with no data
	Checks int     `json:"checks"`
	// Maintenance is the failed checks during planned maintenance (not in Checks or Uptime).
	Maintenance int    `json:"maintenance,omitempty"`
	Class       string `json:"-"` // ok, minor, partial, major, maintenance, none
	Label       string `json:"-"`
}

// MaintenanceView is one maintenance window on the page.
type MaintenanceView struct {
	Title string    `json:"title"`
	Note  string    `json:"note,omitempty"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Targets are the names of the covered targets; empty means all of them.
	Targets []string `json:"targets"`
	Active  bool     `json:"active"`
	When    string   `json:"-"`
}

// IncidentView is one line of the incident history.
type IncidentView struct {
	incident.Incident
	DurationText string `json:"duration"`
}

// HistoryEntry is one line of the history: an incident or a maintenance window that has ended.
type HistoryEntry struct {
	Incident    *IncidentView
	Maintenance *MaintenanceView
}

func (h HistoryEntry) start() time.Time {
	if h.Incident != nil {
		return h.Incident.Started
	}
	return h.Maintenance.Start
}

// Build makes the page from the targets' history and the maintenance windows.
func Build(title string, targets []config.Target, windows []config.Maintenance, st *store.Store, now time.Time) Page {
	p := Page{Title: title, Generated: now}
	p.Maintenance, p.PastMaintenance = maintenanceViews(windows, targets, now)
	counts := map[string]int{}
	for _, t := range targets {
		v := TargetView{ID: t.ID, Name: t.Name, Type: t.Type, Status: "unknown", StatusText: "Not checked yet"}
		if last, ok := st.Last(t.ID); ok {
			v.Status = string(last.Status)
			v.StatusText = statusText(last.Status)
			v.Message = last.Message
			v.LastChecked = last.At
			v.LatencyMs = round(float64(last.Latency)/float64(time.Millisecond), 1)
		}
		if w, ok := config.InMaintenance(windows, t.ID, now); ok {
			v.Maintenance, v.MaintenanceEnd = w.Title, w.End.Time
		} else if v.Status == string(check.Maintenance) {
			// It failed during a window that has ended since: down until a check says otherwise.
			v.Status, v.StatusText = string(check.Down), statusText(check.Down)
		}
		counts[v.Status]++
		v.Uptime24h, _ = st.Uptime(t.ID, 24*time.Hour, now)
		v.Uptime7d, _ = st.Uptime(t.ID, 7*24*time.Hour, now)
		v.Uptime90d, _ = st.Uptime(t.ID, DaysShown*24*time.Hour, now)
		for _, d := range st.Days(t.ID, DaysShown, now) {
			v.Days = append(v.Days, dayView(d))
		}
		v.Recent = st.Recent(t.ID)
		v.Spark, v.SparkMax = sparkline(v.Recent, 240, 40)
		p.Targets = append(p.Targets, v)
	}
	p.Overall = overall(counts, len(targets))
	for _, inc := range st.Incidents() {
		p.Incidents = append(p.Incidents, IncidentView{Incident: inc, DurationText: notify.Duration(inc.Duration(now))})
	}
	for i := range p.Incidents {
		p.History = append(p.History, HistoryEntry{Incident: &p.Incidents[i]})
	}
	for i := range p.PastMaintenance {
		p.History = append(p.History, HistoryEntry{Maintenance: &p.PastMaintenance[i]})
	}
	slices.SortStableFunc(p.History, func(a, b HistoryEntry) int { return b.start().Compare(a.start()) })
	return p
}

func statusText(s check.Status) string {
	switch s {
	case check.Up:
		return "Operational"
	case check.Degraded:
		return "Degraded"
	case check.Down:
		return "Down"
	case check.Maintenance:
		return "Under maintenance"
	}
	return "Unknown"
}

// maintenanceViews lists the windows going on at now (first) and those still to come, by start;
// and those that ended in the last DaysShown days, the latest first.
func maintenanceViews(windows []config.Maintenance, targets []config.Target, now time.Time) (current, past []MaintenanceView) {
	names := map[string]string{}
	for _, t := range targets {
		names[t.ID] = t.Name
	}
	out := []MaintenanceView{}
	past = []MaintenanceView{}
	for _, w := range windows {
		v := MaintenanceView{Title: w.Title, Note: w.Note, Start: w.Start.Time, End: w.End.Time,
			Targets: []string{}, Active: w.Active(now), When: span(w.Start.Time, w.End.Time)}
		for _, id := range w.TargetIDs {
			v.Targets = append(v.Targets, cmp.Or(names[id], id))
		}
		switch {
		case now.Before(w.End.Time):
			out = append(out, v)
		case now.Sub(w.End.Time) <= DaysShown*24*time.Hour:
			past = append(past, v)
		}
	}
	slices.SortStableFunc(past, func(a, b MaintenanceView) int { return b.End.Compare(a.End) })
	slices.SortStableFunc(out, func(a, b MaintenanceView) int {
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		return a.Start.Compare(b.Start)
	})
	return out, past
}

// span writes a time range in UTC: "24 Nov 2026, 22:00–23:30 UTC" within a day, otherwise
// "24 Nov 2026, 22:00 UTC – 25 Nov 2026, 02:00 UTC".
func span(start, end time.Time) string {
	start, end = start.UTC(), end.UTC()
	if y, m, d := start.Date(); end.Year() == y && end.Month() == m && end.Day() == d {
		return start.Format("2 Jan 2006, 15:04") + "–" + end.Format("15:04") + " UTC"
	}
	return start.Format("2 Jan 2006, 15:04 UTC") + " – " + end.Format("2 Jan 2006, 15:04 UTC")
}

func overall(counts map[string]int, total int) Overall {
	switch {
	case counts["unknown"] == total:
		return Overall{"unknown", "Waiting for the first checks"}
	case counts["down"] == total:
		return Overall{"major", "Major outage"}
	case counts["down"] > 0:
		return Overall{"partial", "Partial outage"}
	case counts["maintenance"] > 0:
		return Overall{"maintenance", "Maintenance in progress"}
	case counts["degraded"] > 0:
		return Overall{"degraded", "Degraded performance"}
	case counts["up"] == total:
		return Overall{"up", "All systems operational"}
	}
	return Overall{"up", "All checked systems operational"}
}

func dayView(d store.Day) DayView {
	v := DayView{Date: d.Date.Format("2006-01-02"), Uptime: d.Uptime(), Checks: d.Checks, Maintenance: d.Maintenance}
	date := d.Date.Format("2 Jan 2006")
	planned := ""
	if d.Maintenance > 0 {
		planned = fmt.Sprintf(", %d failed during maintenance", d.Maintenance)
	}
	switch u := d.Uptime(); {
	case u < 0 && d.Maintenance > 0:
		v.Class, v.Label = "maintenance", fmt.Sprintf("%s: maintenance (%d failed checks)", date, d.Maintenance)
	case u < 0:
		v.Class, v.Label = "none", date+": no data"
	default:
		switch {
		case u >= 0.999:
			v.Class = "ok"
		case u >= 0.99:
			v.Class = "minor"
		case u >= 0.95:
			v.Class = "partial"
		default:
			v.Class = "major"
		}
		v.Label = fmt.Sprintf("%s: %s up (%d checks%s)", date, Percent(u), d.Checks, planned)
	}
	return v
}

// Percent writes a share as a percentage with up to two decimals, rounded down so an outage
// never shows as 100 %: "100 %", "99.95 %", "87.3 %". A negative share is "—".
func Percent(share float64) string {
	if share < 0 {
		return "—"
	}
	if share >= 1 {
		return "100 %"
	}
	p := math.Floor(share*10000+1e-6) / 100 // the epsilon: 0.9995*10000 is 9994.999…
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", p), "0"), ".") + " %"
}

// sparkline draws the latencies as an SVG path in a width × height box, from the left as the
// oldest. Failed checks are left out (their latency is the timeout).
func sparkline(points []store.Point, width, height float64) (string, float64) {
	var values []float64
	for _, p := range points {
		if p.Status != check.Down && p.Status != check.Maintenance {
			values = append(values, p.LatencyMs)
		}
	}
	if len(values) < 2 {
		return "", 0
	}
	peak := 0.0
	for _, v := range values {
		peak = max(peak, v)
	}
	if peak == 0 {
		peak = 1
	}
	var b strings.Builder
	step := width / float64(len(values)-1)
	for i, v := range values {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		y := height - 2 - (v/peak)*(height-4)
		fmt.Fprintf(&b, "%s%.1f %.1f ", cmd, float64(i)*step, y)
	}
	return strings.TrimSpace(b.String()), round(peak, 1)
}

func round(f float64, decimals int) float64 {
	p := math.Pow10(decimals)
	return math.Round(f*p) / p
}
