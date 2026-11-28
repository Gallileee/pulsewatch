package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseFillsDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
targets:
  - name: Main site (EU)
    url: https://example.com
  - name: Cert
    type: tls
    host: example.com
  - name: Name
    type: dns
    host: example.com
    interval: 5m
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Title != "Status" || cfg.Listen != ":8080" || cfg.MaxConcurrent != 10 {
		t.Errorf("defaults: %+v", cfg)
	}
	if cfg.Incidents.FailuresToOpen != 2 || cfg.Incidents.SuccessesToResolve != 2 {
		t.Errorf("incident defaults: %+v", cfg.Incidents)
	}
	web := cfg.Targets[0]
	if web.ID != "main-site-eu" || web.Type != TypeHTTP {
		t.Errorf("id/type: %q %q", web.ID, web.Type)
	}
	if time.Duration(web.Interval) != time.Minute || time.Duration(web.Timeout) != 10*time.Second {
		t.Errorf("interval/timeout: %v %v", web.Interval, web.Timeout)
	}
	if cfg.Targets[1].WarnDays != 14 {
		t.Errorf("warn days: %d", cfg.Targets[1].WarnDays)
	}
	if time.Duration(cfg.Targets[2].Interval) != 5*time.Minute {
		t.Errorf("interval: %v", cfg.Targets[2].Interval)
	}
}

func TestShortIntervalGetsHalfAsTimeout(t *testing.T) {
	cfg, err := Parse([]byte("targets:\n  - name: a\n    url: http://a.test\n    interval: 6s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(cfg.Targets[0].Timeout); got != 3*time.Second {
		t.Errorf("timeout %v", got)
	}
}

func TestEnvironmentVariables(t *testing.T) {
	t.Setenv("HOOK", "https://hooks.example.com/secret")
	cfg, err := Parse([]byte(`
alerts:
  - format: discord
    url: ${HOOK}
targets:
  - name: a
    url: http://a.test
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alerts[0].URL != "https://hooks.example.com/secret" {
		t.Errorf("url %q", cfg.Alerts[0].URL)
	}
	_, err = Parse([]byte("alerts:\n  - url: ${NOT_SET_ANYWHERE}\ntargets:\n  - name: a\n    url: http://a.test\n"))
	if err == nil || !strings.Contains(err.Error(), "NOT_SET_ANYWHERE") {
		t.Errorf("missing variable: %v", err)
	}
}

func TestValidationListsEveryProblem(t *testing.T) {
	t.Setenv("SECRET_HOOK", "not a url with secret-token")
	_, err := Parse([]byte(`
max_concurrent: -1
alerts:
  - name: team
    format: pager
  - name: hook
    format: slack
    url: ${SECRET_HOOK}
targets:
  - name: Web
    url: ftp://example.com
    interval: 1s
  - name: web
    url: https://example.com
  - name: Cert
    type: tls
    host: https://example.com/
  - name: Name
    type: dns
    host: example.com
    expect_addresses: [not-an-ip]
    expect_body: hi
  - name: "!!!"
    type: ping
    url: http://x.test
    expect_status: [42]
`))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"max_concurrent",
		`alert "team": format "pager" isn't webhook, discord, slack or email`,
		`alert "hook": url isn't an http(s) URL`,
		`target "Web": url "ftp://example.com"`,
		`target "Web": interval must be at least 5s`,
		`target "web": another target has the same name`,
		`target "Cert": host "https://example.com/"`,
		`expect_addresses: "not-an-ip"`,
		`target "Name": url, expect_status, expect_body and degraded_after are for http targets`,
		`target "!!!": needs a name`,
		`type "ping"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Error("the error shows an alert URL")
	}
}

func TestTimeoutMustBeShorterThanInterval(t *testing.T) {
	_, err := Parse([]byte("targets:\n  - name: a\n    url: http://a.test\n    interval: 10s\n    timeout: 10s\n"))
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err %v", err)
	}
}

func TestUnknownFieldsAndBadDurations(t *testing.T) {
	for _, text := range []string{
		"targets:\n  - name: a\n    url: http://a.test\n    intervall: 10s\n",
		"targets:\n  - name: a\n    url: http://a.test\n    interval: ten seconds\n",
		"targets: []\n",
	} {
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("no error for %q", text)
		}
	}
}

func TestSplitHostPort(t *testing.T) {
	for _, c := range []struct{ in, host, port string }{
		{"example.com", "example.com", "443"},
		{"example.com:8443", "example.com", "8443"},
		{"[::1]:9443", "::1", "9443"},
		{"::1", "::1", "443"},
	} {
		host, port, err := SplitHostPort(c.in, "443")
		if err != nil || host != c.host || port != c.port {
			t.Errorf("%q: %q %q %v", c.in, host, port, err)
		}
	}
	for _, bad := range []string{"", "https://x/", ":443"} {
		if _, _, err := SplitHostPort(bad, "443"); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"API (EU)": "api-eu", "  Web  ": "web", "a--b": "a-b", "***": ""} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExampleConfig(t *testing.T) {
	cfg, err := Load("../../examples/pulsewatch.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Targets) == 0 || cfg.DataFile == "" {
		t.Errorf("%+v", cfg)
	}
}

func TestVariablesInCommentsAreIgnored(t *testing.T) {
	if _, err := Parse([]byte("# url: ${NOT_SET_ANYWHERE}\ntargets:\n  - name: a\n    url: http://a.test\n")); err != nil {
		t.Error(err)
	}
}

func TestEmptyFile(t *testing.T) {
	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), "nothing to watch") {
		t.Errorf("%v", err)
	}
}

func TestEmailAlert(t *testing.T) {
	t.Setenv("SMTP_PASSWORD", "s3cret")
	cfg, err := Parse([]byte(`
alerts:
  - name: on-call
    format: email
    smtp: smtp.example.com:587
    username: alerts@example.com
    password: ${SMTP_PASSWORD}
    from: pulsewatch <alerts@example.com>
    to: [ops@example.com, "Dana <dana@example.com>"]
targets:
  - name: a
    url: http://a.test
`))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Alerts[0]
	if a.Format != FormatEmail || a.Security != SecurityStartTLS || a.Password != "s3cret" || len(a.To) != 2 {
		t.Errorf("%+v", a)
	}
}

func TestEmailAlertProblems(t *testing.T) {
	t.Setenv("SMTP_PASSWORD", "hunter2-secret")
	_, err := Parse([]byte(`
alerts:
  - name: mail
    format: email
    url: https://hooks.test/x
    smtp: smtp.example.com
    security: ssl
    username: me
    from: not an address
    to: [ok@example.com, "@nope"]
  - name: plain
    format: email
    smtp: localhost:1025
    security: none
    username: me
    password: ${SMTP_PASSWORD}
    from: a@example.com
targets:
  - name: a
    url: http://a.test
`))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		`alert "mail": url is for webhooks`,
		`alert "mail": smtp "smtp.example.com" isn't host:port`,
		`alert "mail": security "ssl" isn't starttls, tls or none`,
		`alert "mail": username and password go together`,
		`alert "mail": from "not an address" isn't an email address`,
		`alert "mail": to "@nope" isn't an email address`,
		`alert "plain": security none would send the password unencrypted`,
		`alert "plain": to: nobody to send to`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Error("the error shows the password")
	}
	if strings.Contains(err.Error(), `alert "plain": username`) {
		t.Errorf("username and password are both set for plain:\n%v", err)
	}
}
