# Progress log

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

1. Deploy it: a small VM or Fly.io with the Docker image, watching the portfolio's demos, and
   link the live status page from the README.
2. Email alerts (SMTP with STARTTLS, settings from the environment) next to the webhooks.
3. A Grafana dashboard (JSON, provisioned in the Compose file) on the Prometheus metrics.
4. Maintenance windows: planned downtime that opens no incident and shows on the page.
5. Reload the config on SIGHUP without losing the history or the open incidents.
