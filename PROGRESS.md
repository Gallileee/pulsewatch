# Progress log

## 2026-11-28 (scheduled run): email alerts

Why: next step 1. Chat webhooks need a team chat; email is what a solo maintainer already has,
and it's what every hosted monitor offers first. The SMTP part needs no mail account to build
and test (a fake server in the tests, Mailpit for a try-out); only the Discord alert from the
Actions run needs a repository secret, so that half stays for the owner.

Acceptance criteria:
- `format: email` with `smtp` (host:port), `security` (`starttls` default, `tls`, `none`),
  optional `username`/`password`, `from`, `to`; `${VARIABLES}` in all of them; every problem
  reported at once, never the password.
- With `starttls`, nothing but EHLO and STARTTLS before TLS; a server without STARTTLS gets no
  email. `none` with a password is a config error.
- The outage and recovery emails are one thread; subjects stay one line.
- Retries like the webhooks: 4xx and network errors again, 5xx and bad certificates not.

Done:
- `config.Alert` email fields, `FormatEmail`, `Security*`, `emailProblems`; webhook URL checks
  only for webhook formats.
- `notify.Email` (net/smtp with a context: the connection closes when it ends), `Message`
  (quoted-printable text, RFC 2047 subject folded under 78 characters a line, Message-ID from
  the incident, `In-Reply-To`/`References` on the recovery, `Auto-Submitted`), `smtpError` /
  `tlsError` decide what is retried. The webhook retry loop became `retry` for both.
  `FromConfig` returns a `Notifier`.
- 10 new tests (8 against a fake SMTP server: STARTTLS + login, no STARTTLS, implicit TLS,
  plain, untrusted certificate, 4xx retried / 5xx not, a server that never answers, the
  message; 2 config). One existing case changed on purpose: "email" was the example of an
  unknown format.
- README (features, config, Mailpit try-out, limits), CHANGELOG v0.5.0 (v0.4.0 heading dated),
  example config.

Verified locally:
- `go vet ./...`, `gofmt -l`: clean; `go test ./...`: all pass (103 test functions).
- Hand-made mutants, 7 of 7 caught: no STARTTLS check, 5xx retried, no `In-Reply-To`, bad
  certificate retried, line breaks kept in the subject, `none` allowed with a password,
  `none` as the default.
- The real binary: two `-once` runs against a refused port with an email alert to a local
  fake SMTP server (`security: none`): the second run opened the incident and the email
  arrived with the folded subject, the error, "Failed checks: 2 in a row".

Not verified: delivery through a real provider (Gmail, Fastmail...) over the internet.

## 2026-11-23 (scheduled run): maintenance windows

Why: next step 3, and the one with the most to learn: it touches every layer (config, monitor,
store, metrics, page) and has real decisions in it. Hosted monitors agree on the behaviour:
alerts held, planned downtime left out of uptime, the window shown on the status page
(Checkly, Uptime.com, incident.io docs). Email alerts (step 1) need a mail account and a
repository secret, which a scheduled run shouldn't set up alone.

Acceptance criteria:
- `maintenance:` entries with a title, RFC 3339 start/end with a zone, optional targets and a
  note; validated (zone, end after start, at most 7 days, known targets), all errors at once.
- A failed check of a covered target inside a window is `maintenance`: no incident, no alert,
  not in the uptime; the tracker doesn't see it. Good checks count as usual.
- The page lists active and announced windows, marks covered targets and the daily bars,
  shows "Maintenance in progress"; a blue badge; `pulsewatch_maintenance` metric.
- The demo shows it.

Done:
- `config.Maintenance`, `config.Time` (strict RFC 3339), `InMaintenance`; `check.Maintenance`;
  `Monitor.Maintenance`; `Hour.Maintenance`/`Day.Maintenance` in the store (saved with the
  history, older files read fine); `Metrics.SetMaintenance`; `web.MaintenanceView`, `span`,
  the template section and target note, CSS tokens `--maint`/`--maint-soft` for both themes.
- A target whose last check was `maintenance` but whose window has ended shows as down until
  the next check.
- 15 new tests (92 in total). Hand-made mutants: 8 of 9 caught, the other is equivalent (the
  `maintenance` case in `overall` can't be reached when something is down).
- README, CHANGELOG v0.4.0 (v0.3.0 heading dated), example config.

Verified locally:
- `go vet`, `gofmt -l`: clean; `go test ./...`: all pass.
- `pulsewatch -demo` in the browser: "Scheduled maintenance" with "In progress" and
  "Scheduled"; Payments failed twice during its window, recorded as `maintenance`, no
  incident (only the Reports outage opened one); the Payments note with the end time;
  `/metrics` had `pulsewatch_maintenance{target="payments"} 1`; dark desktop and light 375 px
  with no sideways scrolling; no console errors.

Not verified: the GitHub Pages deployment with a window in `deploy/status.yaml` (none planned).

## 2026-11-21 (interactive, user picked "Odkaz z ostatních README"): status badges

Why: linking the status page from the projects it watches. Shields.io's dynamic JSON badge
can't filter `status.json` by target ID ("query not supported"), and an index would break when
the targets change, so Pulsewatch writes a badge file per target.

Done:
- `web.BadgeFor` and `/badge/{id}.json` (CORS open, cached a minute); `WriteSite` writes
  `badge/<id>.json` for every target.
- Tests: the badge for each status, the route (unknown ID, no `.json` → 404), the files in a
  static site.
- README (badge example), CHANGELOG v0.3.0, v0.2.0 heading dated.
- Then: the Market Pulse README links its status badge and the status page (a PR there).

Verified locally: `go vet`, `gofmt`, `go test ./...` clean and passing.

## 2026-11-21 (interactive, user picked "Nasazení Pulsewatch", then free hosting on GitHub Actions + Pages)

Why: next step 1. Always-on free hosts sleep (Render, Koyeb) or need a card (Fly.io now has
only a short trial; Oracle's free VMs). The user chose GitHub Actions + Pages: no server, no
cost, no live stream.

Acceptance criteria:
- `-once`: every target checked once, alerts waited for, history saved, exit 0 even with
  targets down; nothing saved if interrupted.
- `-site DIR`: a static page (relative links, CSP in a meta tag, reloads itself every minute)
  plus `status.json` and the assets.
- Incident streaks survive between runs (and restarts): saved in the history file.
- A scheduled workflow that carries the history between runs and deploys to Pages; checked
  by workflow-guard.

Done:
- `incident.State` with `State()`/`Restore()`; the store saves it per target; the monitor
  restores it (falling back to the open incident for older files) and saves it after every
  check; `Monitor.RunOnce`.
- `web.Server.WriteSite`; the template's static mode; `app.js` polls the page itself in
  static mode; relative URLs everywhere.
- `deploy/status.yaml` and `.github/workflows/status.yml` (history as an artifact from the
  newest earlier run that has one, kept 3 days; `upload-pages-artifact` + `deploy-pages`).
- Tests: tracker state across trackers, trackers saved/forgotten, `RunOnce`, a streak across
  two runs, the static site's files and links; 77 tests in total.
- README (free hosting section, live link), CHANGELOG v0.2.0, v0.1.0 heading dated.

Verified locally:
- `go vet`, `gofmt`, `go test ./...`: clean, all pass.
- `pulsewatch -once` twice against a dead local port: the second run opened the incident
  (`failures: 2`, "Ongoing" on the page); the static site has `data-mode="static"` and links
  `status.json`.
- `deploy/status.yaml` run once: all 4 targets up (certificate valid 38 more days).
- workflow-guard on both workflows: 0 findings.

Not verified locally: the workflow itself (artifacts, Pages deploy) runs only on GitHub. Pages
has to be switched to "GitHub Actions" in the repository settings before the first deploy.

## 2026-11-21 (interactive, user picked "Pulsewatch"): milestone 1

Why: the portfolio had a React frontend (fitness-workout-log, market-pulse), a Java backend
(LendLoop), a Python DevSecOps CLI (workflow-guard), algorithms (transit-raptor) and LLM apps
(recipe-suggester, ask-your-pdfs). It had no Go, no long-running service with metrics and
alerting, and nothing pushed to browsers live. Pulsewatch covers those, and it watches the
other projects' demos.

Acceptance criteria:
- HTTP, TLS and DNS checks with timeouts; up, degraded and down, each with a message.
- A scheduler with an interval per target, start offsets and a limit on concurrent checks;
  tested with a fake clock, without sleeping.
- Incidents from failure and success streaks; resumed after a restart.
- Alerts to webhook, Discord or Slack, with retries; secrets from the environment, never
  logged.
- 90 days of history, saved atomically; a broken file kept aside.
- A status page that works without JavaScript and updates over SSE; JSON API; Prometheus
  metrics.
- A Docker image and Compose file; CI on Linux (race detector) and Windows; workflow-guard.

Done:
- 10 packages, one dependency (yaml.v3).
- Tests in every package: config validation, every check against `httptest` servers (TLS
  expiry by moving the clock), the incident state machine, store totals and save/load,
  webhook formats and retries, the metrics format, the scheduler and concurrency limit, the
  page, the JSON, SSE and the demo.
- README with the design decisions, CHANGELOG v0.1.0, an example config, a Prometheus config.

Verified locally:
- `go vet ./...`, `gofmt -l .`: clean. `go test ./...`: all packages pass. The race detector
  needs cgo, which isn't set up on this Windows machine; CI runs it on Linux.
- `pulsewatch -demo` in the browser:
  - the overall banner went from partial outage to all operational as the demo's outage
    ended;
  - the slow endpoint showed as degraded, and the outage endpoint opened an incident after 2
    failures;
  - "Live" was on, and the page updated by itself;
  - light and dark themes, and a 375 px width with no sideways scrolling;
  - `/metrics` and `/api/status` matched the page;
  - no console errors.
- workflow-guard on `.github/workflows/ci.yml`: 0 findings.
- Docker isn't installed on this machine: the image and the demo smoke test run in CI.

## Next steps (in order)

1. A Discord (or email) alert from the Actions run: needs a repository secret, so the owner
   adds `DISCORD_WEBHOOK_URL` and the alert goes into `deploy/status.yaml`. Then send one
   real email through a provider to check delivery.
2. A Grafana dashboard (JSON, provisioned in the Compose file) on the Prometheus metrics.
3. Reload the config on SIGHUP without losing the history or the open incidents (useful now
   that maintenance windows live in the config).
4. Past maintenance in the history list (Checkly shows completed windows next to incidents),
   and an iCalendar feed (`maintenance.ics`) of announced windows.
