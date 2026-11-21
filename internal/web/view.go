package web

import (
	"fmt"
	"math"
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
	Title     string         `json:"title"`
	Overall   Overall        `json:"overall"`
	Targets   []TargetView   `json:"targets"`
	Incidents []IncidentView `json:"incidents"`
	Generated time.Time      `json:"generated"`
}

// Overall is the banner at the top.
type Overall struct {
	Status string `json:"status"` // up, degraded, partial, major, unknown
	Text   string `json:"text"`
}

// TargetView is one target's card.
type TargetView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Status      string    `json:"status"` // up, degraded, down, unknown
	StatusText  string    `json:"status_text"`
	Message     string    `json:"message,omitempty"`
	LastChecked time.Time `json:"last_checked,omitzero"`
	LatencyMs   float64   `json:"latency_ms"`
	Uptime24h   float64   `json:"uptime_24h"` // a share; -1 with no data
	Uptime7d    float64   `json:"uptime_7d"`
	Uptime90d   float64   `json:"uptime_90d"`
	Days        []DayView `json:"days"`
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
	Class  string  `json:"-"` // ok, minor, partial, major, none
	Label  string  `json:"-"`
}

// IncidentView is one line of the incident history.
type IncidentView struct {
	incident.Incident
	DurationText string `json:"duration"`
}

// Build makes the page from the targets' history.
func Build(title string, targets []config.Target, st *store.Store, now time.Time) Page {
	p := Page{Title: title, Generated: now}
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
	}
	return "Unknown"
}

func overall(counts map[string]int, total int) Overall {
	switch {
	case counts["unknown"] == total:
		return Overall{"unknown", "Waiting for the first checks"}
	case counts["down"] == total:
		return Overall{"major", "Major outage"}
	case counts["down"] > 0:
		return Overall{"partial", "Partial outage"}
	case counts["degraded"] > 0:
		return Overall{"degraded", "Degraded performance"}
	case counts["up"] == total:
		return Overall{"up", "All systems operational"}
	}
	return Overall{"up", "All checked systems operational"}
}

func dayView(d store.Day) DayView {
	v := DayView{Date: d.Date.Format("2006-01-02"), Uptime: d.Uptime(), Checks: d.Checks}
	date := d.Date.Format("2 Jan 2006")
	switch u := d.Uptime(); {
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
		v.Label = fmt.Sprintf("%s: %s up (%d checks)", date, Percent(u), d.Checks)
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
// oldest. Down checks are left out (their latency is the timeout).
func sparkline(points []store.Point, width, height float64) (string, float64) {
	var values []float64
	for _, p := range points {
		if p.Status != check.Down {
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
