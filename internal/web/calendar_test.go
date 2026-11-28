package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/incident"
	"github.com/Gallileee/pulsewatch/internal/store"
)

// unfold joins folded lines back together (RFC 5545 3.1) and checks that none is too long.
func unfold(t *testing.T, ics string) []string {
	t.Helper()
	if !strings.HasSuffix(ics, "\r\n") || strings.Contains(strings.ReplaceAll(ics, "\r\n", ""), "\n") {
		t.Fatalf("lines must end with CRLF: %q", ics)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSuffix(ics, "\r\n"), "\r\n") {
		if len(l) > 75 {
			t.Errorf("line of %d octets: %q", len(l), l)
		}
		if strings.HasPrefix(l, " ") {
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

func TestCalendar(t *testing.T) {
	long := mw("Datacenter move; network, storage — and everything else in the racks", -3*time.Hour, -2*time.Hour)
	long.Note = "Expect short outages.\nWe'll post updates here."
	ics := string(Calendar("Acme status", append(windows, long), targets, now))
	lines := unfold(t, ics)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//Gallileee//pulsewatch//EN",
		"X-WR-CALNAME:Acme status maintenance",
		"REFRESH-INTERVAL;VALUE=DURATION:PT1H",
		// The second window in the fixture: a title with markup, only the API.
		"UID:20261121T113000Z-20261121T123000Z-database-upgrade@pulsewatch\nDTSTAMP:20261121T120000Z\n" +
			"DTSTART:20261121T113000Z\nDTEND:20261121T123000Z\nSUMMARY:Maintenance: Database <upgrade>\n" +
			"DESCRIPTION:Affects: API\nTRANSP:TRANSPARENT\nEND:VEVENT",
		`DESCRIPTION:Affects: Website <b>\, gone`,
		"DESCRIPTION:Affects: All services",
		`SUMMARY:Maintenance: Datacenter move\; network\, storage — and everything else in the racks`,
		`DESCRIPTION:Expect short outages.\nWe'll post updates here.\n\nAffects: All services`,
		"END:VCALENDAR",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// Every window, the ended ones too.
	if n := strings.Count(ics, "BEGIN:VEVENT"); n != len(windows)+1 {
		t.Errorf("%d events", n)
	}
}

func TestFoldKeepsCharactersWhole(t *testing.T) {
	line := "SUMMARY:" + strings.Repeat("ž", 100) // 2 octets each
	folded := fold(line)
	for _, part := range strings.Split(strings.TrimSuffix(folded, "\r\n"), "\r\n ") {
		if len(part) > 75 || !strings.HasPrefix(part, "SUMMARY") && len(part) > 74 {
			t.Errorf("part of %d octets", len(part))
		}
		if !utf8Valid(part) {
			t.Errorf("split inside a character: %q", part)
		}
	}
	if strings.ReplaceAll(strings.TrimSuffix(folded, "\r\n"), "\r\n ", "") != line {
		t.Error("unfolding doesn't give the line back")
	}
	ascii := fold("DESCRIPTION:" + strings.Repeat("a", 200))
	for _, l := range strings.Split(strings.TrimSuffix(ascii, "\r\n"), "\r\n") {
		if len(l) > 75 {
			t.Errorf("line of %d octets, with the leading space: %q", len(l), l)
		}
	}
	if fold("A:b") != "A:b\r\n" {
		t.Errorf("short line: %q", fold("A:b"))
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestCalendarRouteAndSite(t *testing.T) {
	s, _ := newServer(fixture())
	s.Maintenance = windows
	rec := get(t, s.Handler(), "/maintenance.ics")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/calendar; charset=utf-8" ||
		!strings.Contains(rec.Body.String(), "SUMMARY:Maintenance: Soon") {
		t.Errorf("%d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if !strings.Contains(get(t, s.Handler(), "/").Body.String(), `<a href="maintenance.ics">Maintenance calendar</a>`) {
		t.Error("no calendar link in the footer")
	}

	dir := t.TempDir()
	if err := s.WriteSite(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "maintenance.ics"))
	if err != nil || !strings.Contains(string(data), "SUMMARY:Maintenance: Later\\, all") {
		t.Errorf("%v %q", err, data)
	}
}

func TestPastMaintenanceInTheHistory(t *testing.T) {
	st := store.New()
	st.Record(check.Result{TargetID: "web", At: now, Status: check.Up})
	st.SaveIncident(incident.Incident{ID: "api-1", TargetID: "api", TargetName: "API", Started: now.Add(-5 * time.Hour),
		Resolved: ptr(now.Add(-4 * time.Hour)), Cause: "HTTP 500"})
	st.SaveIncident(incident.Incident{ID: "web-1", TargetID: "web", TargetName: "Website", Started: now.Add(-1 * time.Hour),
		Resolved: ptr(now.Add(-50 * time.Minute)), Cause: "timeout"})
	ws := []config.Maintenance{
		mw("Earlier", -30*time.Hour, -29*time.Hour),
		mw("Long ago", -100*24*time.Hour, -100*24*time.Hour+time.Hour),
		mw("Over", -3*time.Hour, -2*time.Hour, "api"),
		mw("Now", -time.Minute, time.Hour),
	}
	p := Build("Acme", targets, ws, st, now)
	var past []string
	for _, m := range p.PastMaintenance {
		past = append(past, m.Title)
	}
	if strings.Join(past, "|") != "Over|Earlier" {
		t.Errorf("past %q (older than 90 days left out, latest first)", past)
	}
	var order []string
	for _, h := range p.History {
		if h.Incident != nil {
			order = append(order, h.Incident.ID)
		} else {
			order = append(order, h.Maintenance.Title)
		}
	}
	if strings.Join(order, "|") != "web-1|Over|api-1|Earlier" {
		t.Errorf("history %q", order)
	}
	if len(p.Maintenance) != 1 || p.Maintenance[0].Title != "Now" {
		t.Errorf("current %+v", p.Maintenance)
	}
}

func ptr[T any](v T) *T { return &v }
