# Progress log

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

1. After the merge: set Pages to "GitHub Actions", run the workflow by hand, check the live
   page and that the second run picks up the first run's history.
2. Email alerts (SMTP with STARTTLS, settings from the environment) next to the webhooks, and
   a Discord alert from the Actions run (a repository secret).
3. A Grafana dashboard (JSON, provisioned in the Compose file) on the Prometheus metrics.
4. Maintenance windows: planned downtime that opens no incident and shows on the page.
5. Reload the config on SIGHUP without losing the history or the open incidents.
