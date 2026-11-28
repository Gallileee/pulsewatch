package web

import (
	"cmp"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Gallileee/pulsewatch/internal/config"
)

// Calendar writes the maintenance windows as an iCalendar feed (RFC 5545), so people can
// subscribe to maintenance.ics and see planned downtime in their own calendar. Every window
// in the config is in it, the ended ones too, so a calendar keeps them after they're over.
func Calendar(title string, windows []config.Maintenance, targets []config.Target, now time.Time) []byte {
	names := map[string]string{}
	for _, t := range targets {
		names[t.ID] = t.Name
	}
	var b strings.Builder
	line := func(name, value string) { b.WriteString(fold(name + ":" + value)) }
	stamp := func(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

	line("BEGIN", "VCALENDAR")
	line("VERSION", "2.0")
	line("PRODID", "-//Gallileee//pulsewatch//EN")
	line("CALSCALE", "GREGORIAN")
	line("METHOD", "PUBLISH")
	line("X-WR-CALNAME", escapeText(title+" maintenance"))
	// RFC 7986, and the older name Outlook reads: check for new windows every hour.
	line("REFRESH-INTERVAL;VALUE=DURATION", "PT1H")
	line("X-PUBLISHED-TTL", "PT1H")
	for _, w := range windows {
		covered := "All services"
		if len(w.TargetIDs) > 0 {
			list := make([]string, len(w.TargetIDs))
			for i, id := range w.TargetIDs {
				list[i] = cmp.Or(names[id], id)
			}
			covered = strings.Join(list, ", ")
		}
		description := "Affects: " + covered
		if w.Note != "" {
			description = w.Note + "\n\n" + description
		}
		line("BEGIN", "VEVENT")
		// The same window keeps its UID across runs, so calendars update it instead of adding
		// a copy; a changed start or end is a new window.
		line("UID", fmt.Sprintf("%s-%s-%s@pulsewatch", stamp(w.Start.Time), stamp(w.End.Time), config.Slug(w.Title)))
		line("DTSTAMP", stamp(now))
		line("DTSTART", stamp(w.Start.Time))
		line("DTEND", stamp(w.End.Time))
		line("SUMMARY", escapeText("Maintenance: "+w.Title))
		line("DESCRIPTION", escapeText(description))
		// Planned downtime of a service doesn't make the subscriber busy.
		line("TRANSP", "TRANSPARENT")
		line("END", "VEVENT")
	}
	line("END", "VCALENDAR")
	return []byte(b.String())
}

// escapeText escapes a TEXT value (RFC 5545 3.3.11).
func escapeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`, "\r", `\n`).Replace(s)
}

// fold ends a content line with CRLF and splits it into lines of at most 75 octets, each
// continuation starting with a space (RFC 5545 3.1), never inside a UTF-8 character.
func fold(s string) string {
	var b strings.Builder
	limit := 75
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		b.WriteString(s[:cut])
		b.WriteString("\r\n ")
		s = s[cut:]
		limit = 74 // the leading space counts
	}
	b.WriteString(s)
	b.WriteString("\r\n")
	return b.String()
}
