// Package config reads and checks the YAML file that lists what Pulsewatch watches.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Check types.
const (
	TypeHTTP = "http"
	TypeTLS  = "tls"
	TypeDNS  = "dns"
)

// Alert formats.
const (
	FormatWebhook = "webhook"
	FormatDiscord = "discord"
	FormatSlack   = "slack"
)

// MinInterval is the shortest interval between two checks of one target.
const MinInterval = 5 * time.Second

// MaxMaintenance is the longest a maintenance window may last: a window that never ends would
// hide real outages for good.
const MaxMaintenance = 7 * 24 * time.Hour

// Time is a moment written in RFC 3339 with a time zone, such as 2026-11-24T22:00:00Z or
// 2026-11-24T23:00:00+01:00.
type Time struct{ time.Time }

// UnmarshalYAML reads an RFC 3339 time; one without a zone is an error, not a guess.
func (t *Time) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.Parse(time.RFC3339, node.Value)
	if node.Kind != yaml.ScalarNode || err != nil {
		return fmt.Errorf("line %d: %q isn't a time like 2026-11-24T22:00:00Z (RFC 3339, with a zone)", node.Line, node.Value)
	}
	t.Time = parsed
	return nil
}

// Duration is a time.Duration written as "30s", "5m" or "1h30m" in YAML.
type Duration time.Duration

// UnmarshalYAML reads a Go duration string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var text string
	if err := node.Decode(&text); err != nil {
		return fmt.Errorf("line %d: a duration like 30s or 5m: %w", node.Line, err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("line %d: %q isn't a duration like 30s or 5m", node.Line, text)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the whole file.
type Config struct {
	// Title of the status page.
	Title string `yaml:"title"`
	// Listen is the address of the web server, ":8080" by default.
	Listen string `yaml:"listen"`
	// DataFile keeps the history across restarts; empty keeps it in memory only.
	DataFile string `yaml:"data_file"`
	// MaxConcurrent limits how many checks run at the same time (10 by default).
	MaxConcurrent int `yaml:"max_concurrent"`
	// PublicURL is where the status page is reachable, linked from alerts.
	PublicURL string   `yaml:"public_url"`
	Incidents Incident `yaml:"incidents"`
	Alerts    []Alert  `yaml:"alerts"`
	Targets   []Target `yaml:"targets"`
	// Maintenance lists planned downtime.
	Maintenance []Maintenance `yaml:"maintenance"`
}

// Maintenance is planned downtime. While it lasts, a failed check of a target it covers opens
// no incident, sends no alert and doesn't count against the uptime; the status page shows it
// from when it's announced (in the config) until it ends.
type Maintenance struct {
	Title string `yaml:"title"`
	Start Time   `yaml:"start"`
	End   Time   `yaml:"end"`
	// Targets are the names of the targets it covers; empty covers all of them.
	Targets []string `yaml:"targets"`
	// Note says more on the status page (what changes, whom to ask).
	Note string `yaml:"note"`
	// TargetIDs are the covered targets' IDs, filled in from Targets.
	TargetIDs []string `yaml:"-"`
}

// Active reports whether the window is going on at t: from Start up to, not including, End.
func (w Maintenance) Active(t time.Time) bool {
	return !t.Before(w.Start.Time) && t.Before(w.End.Time)
}

// Covers reports whether the window includes the target.
func (w Maintenance) Covers(id string) bool {
	return len(w.TargetIDs) == 0 || slices.Contains(w.TargetIDs, id)
}

// InMaintenance is the first window that covers the target at t, if any.
func InMaintenance(windows []Maintenance, id string, t time.Time) (Maintenance, bool) {
	for _, w := range windows {
		if w.Active(t) && w.Covers(id) {
			return w, true
		}
	}
	return Maintenance{}, false
}

// Incident sets when an outage starts and ends.
type Incident struct {
	// FailuresToOpen is how many failed checks in a row open an incident (2 by default).
	FailuresToOpen int `yaml:"failures_to_open"`
	// SuccessesToResolve is how many good checks in a row close it (2 by default).
	SuccessesToResolve int `yaml:"successes_to_resolve"`
}

// Alert is where notifications go.
type Alert struct {
	Name string `yaml:"name"`
	// Format is webhook (the incident as JSON), discord or slack.
	Format string `yaml:"format"`
	// URL may use ${VARIABLE}s, so a webhook's secret stays out of the file.
	URL string `yaml:"url"`
}

// Target is one thing to watch.
type Target struct {
	// ID is made from the name: lower case, dashes.
	ID   string `yaml:"-"`
	Name string `yaml:"name"`
	// Type is http (the default), tls or dns.
	Type string `yaml:"type"`
	// URL to request (http).
	URL string `yaml:"url"`
	// Host to connect to (tls, host or host:port, 443 by default) or to resolve (dns).
	Host     string   `yaml:"host"`
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
	// ExpectStatus lists the good HTTP status codes; empty means 200-399.
	ExpectStatus []int `yaml:"expect_status"`
	// ExpectBody must appear in the response body.
	ExpectBody string `yaml:"expect_body"`
	// DegradedAfter: a slower answer counts as degraded (http).
	DegradedAfter Duration `yaml:"degraded_after"`
	// WarnDays: a certificate that expires sooner is degraded (tls, 14 by default).
	WarnDays int `yaml:"warn_days"`
	// ExpectAddresses: the name must resolve to at least one of them (dns).
	ExpectAddresses []string `yaml:"expect_addresses"`
}

var envVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads the file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse reads a config from YAML, fills in the defaults and checks it. ${VARIABLE}s in values
// (not in comments) are replaced from the environment; one that isn't set is an error.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading the config: %w", err)
	}
	if err := cfg.expand(); err != nil {
		return nil, err
	}
	cfg.defaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expand replaces the ${VARIABLE}s in the text fields from the environment.
func (c *Config) expand() error {
	var missing []string
	replace := func(s *string) {
		*s = envVar.ReplaceAllStringFunc(*s, func(m string) string {
			name := envVar.FindStringSubmatch(m)[1]
			value, ok := os.LookupEnv(name)
			if !ok && !slices.Contains(missing, name) {
				missing = append(missing, name)
			}
			return value
		})
	}
	for _, s := range []*string{&c.Title, &c.Listen, &c.DataFile, &c.PublicURL} {
		replace(s)
	}
	for i := range c.Alerts {
		replace(&c.Alerts[i].Name)
		replace(&c.Alerts[i].URL)
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		for _, s := range []*string{&t.Name, &t.URL, &t.Host, &t.ExpectBody} {
			replace(s)
		}
	}
	for i := range c.Maintenance {
		replace(&c.Maintenance[i].Title)
		replace(&c.Maintenance[i].Note)
	}
	if len(missing) > 0 {
		return fmt.Errorf("environment variables not set: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) defaults() {
	if c.Title == "" {
		c.Title = "Status"
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 10
	}
	if c.Incidents.FailuresToOpen == 0 {
		c.Incidents.FailuresToOpen = 2
	}
	if c.Incidents.SuccessesToResolve == 0 {
		c.Incidents.SuccessesToResolve = 2
	}
	for i := range c.Alerts {
		if c.Alerts[i].Format == "" {
			c.Alerts[i].Format = FormatWebhook
		}
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		t.ID = Slug(t.Name)
		if t.Type == "" {
			t.Type = TypeHTTP
		}
		if t.Interval == 0 {
			t.Interval = Duration(time.Minute)
		}
		if t.Timeout == 0 {
			t.Timeout = Duration(min(10*time.Second, time.Duration(t.Interval)/2))
		}
		if t.Type == TypeTLS && t.WarnDays == 0 {
			t.WarnDays = 14
		}
	}
	for i := range c.Maintenance {
		w := &c.Maintenance[i]
		w.TargetIDs = nil
		for _, name := range w.Targets {
			w.TargetIDs = append(w.TargetIDs, Slug(name))
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.MaxConcurrent < 1 {
		add("max_concurrent must be at least 1")
	}
	if c.Incidents.FailuresToOpen < 1 || c.Incidents.SuccessesToResolve < 1 {
		add("incidents: failures_to_open and successes_to_resolve must be at least 1")
	}
	if c.PublicURL != "" && !isHTTPURL(c.PublicURL) {
		add("public_url %q isn't an http(s) URL", c.PublicURL)
	}
	for i, a := range c.Alerts {
		where := fmt.Sprintf("alerts[%d]", i)
		if a.Name != "" {
			where = fmt.Sprintf("alert %q", a.Name)
		}
		switch a.Format {
		case FormatWebhook, FormatDiscord, FormatSlack:
		default:
			add("%s: format %q isn't webhook, discord or slack", where, a.Format)
		}
		if !isHTTPURL(a.URL) {
			add("%s: url isn't an http(s) URL", where) // not shown: it may hold a secret
		}
	}
	if len(c.Targets) == 0 {
		add("targets: nothing to watch")
	}
	seen := map[string]bool{}
	for i, t := range c.Targets {
		where := fmt.Sprintf("targets[%d]", i)
		if t.Name != "" {
			where = fmt.Sprintf("target %q", t.Name)
		}
		if t.ID == "" {
			add("%s: needs a name with a letter or digit", where)
		} else if seen[t.ID] {
			add("%s: another target has the same name", where)
		}
		seen[t.ID] = true
		if time.Duration(t.Interval) < MinInterval {
			add("%s: interval must be at least %s", where, MinInterval)
		}
		if t.Timeout <= 0 || t.Timeout >= t.Interval {
			add("%s: timeout must be more than 0 and less than the interval", where)
		}
		switch t.Type {
		case TypeHTTP:
			if !isHTTPURL(t.URL) {
				add("%s: url %q isn't an http(s) URL", where, t.URL)
			}
			for _, code := range t.ExpectStatus {
				if code < 100 || code > 599 {
					add("%s: expect_status %d isn't an HTTP status", where, code)
				}
			}
			if t.Host != "" || t.WarnDays != 0 || len(t.ExpectAddresses) > 0 {
				add("%s: host, warn_days and expect_addresses are for tls and dns targets", where)
			}
		case TypeTLS:
			if _, _, err := SplitHostPort(t.Host, "443"); err != nil {
				add("%s: host %q: %v", where, t.Host, err)
			}
			if t.WarnDays < 0 {
				add("%s: warn_days can't be negative", where)
			}
		case TypeDNS:
			if t.Host == "" || strings.ContainsAny(t.Host, ":/ ") {
				add("%s: host %q isn't a name to resolve", where, t.Host)
			}
			for _, a := range t.ExpectAddresses {
				if net.ParseIP(a) == nil {
					add("%s: expect_addresses: %q isn't an IP address", where, a)
				}
			}
		default:
			add("%s: type %q isn't http, tls or dns", where, t.Type)
		}
		if t.Type != TypeHTTP && (t.URL != "" || len(t.ExpectStatus) > 0 || t.ExpectBody != "" || t.DegradedAfter != 0) {
			add("%s: url, expect_status, expect_body and degraded_after are for http targets", where)
		}
	}
	for i, w := range c.Maintenance {
		where := fmt.Sprintf("maintenance[%d]", i)
		if w.Title != "" {
			where = fmt.Sprintf("maintenance %q", w.Title)
		} else {
			add("%s: needs a title (it's shown on the status page)", where)
		}
		switch {
		case w.Start.IsZero() || w.End.IsZero():
			add("%s: needs a start and an end", where)
		case !w.End.After(w.Start.Time):
			add("%s: end must be after start", where)
		case w.End.Sub(w.Start.Time) > MaxMaintenance:
			add("%s: lasts longer than %s; split it, or fix what needs that long", where, "7 days")
		}
		for j, id := range w.TargetIDs {
			if !seen[id] {
				add("%s: no target named %q", where, w.Targets[j])
			}
		}
	}
	return errors.Join(errs...)
}

// SplitHostPort splits "host" or "host:port", using port when there is none.
func SplitHostPort(hostport, port string) (string, string, error) {
	if hostport == "" {
		return "", "", errors.New("empty")
	}
	if strings.Contains(hostport, "/") {
		return "", "", errors.New("a host, not a URL")
	}
	if net.ParseIP(hostport) != nil {
		return hostport, port, nil // an IPv6 address without brackets has colons but no port
	}
	host, p, err := net.SplitHostPort(hostport)
	if err != nil {
		if strings.Contains(err.Error(), "missing port") {
			return strings.Trim(hostport, "[]"), port, nil
		}
		return "", "", err
	}
	if host == "" {
		return "", "", errors.New("no host")
	}
	return host, p, nil
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a name into an ID: "API (EU)" becomes "api-eu".
func Slug(name string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
}
