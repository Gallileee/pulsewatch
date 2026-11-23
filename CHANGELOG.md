# Changelog

## v0.4.0 — maintenance windows

- **`maintenance:` in the config** lists planned downtime: a title, a start and an end (RFC 3339
  with a zone, at most 7 days), the targets it covers (all if left out) and a note.
- **During a window, a failed check of a covered target is `maintenance`, not `down`:** it opens
  no incident, sends no alert and is left out of the uptime (the hourly totals count it
  separately). Good checks count as usual. A failure streak or an incident from before the
  window carries on afterwards.
- **The status page** lists the windows going on ("In progress") and announced ("Scheduled"),
  marks a covered target with the window and its end, says "Maintenance in progress" in the
  banner unless something is really down, and colours a day that had only planned failures.
  The badge of a target under maintenance is blue.
- **Metrics:** `pulsewatch_maintenance` per target, and `status="maintenance"` in
  `pulsewatch_checks_total`.
- The demo plans two windows: one going on for Payments, one announced for the next evening.

## v0.3.0 — status badges (2026-11-21)

- `/badge/<id>.json` (and `badge/<id>.json` in a static site) gives each target's status in
  the [Shields.io endpoint badge](https://shields.io/badges/endpoint-badge) format: the status
  and its 7-day uptime, for example "operational · 100 % (7 d)", coloured green, yellow, red,
  or grey before the first check. A README can show it as a badge that follows the target's
  status.

## v0.2.0 — free hosting: `-once` and a static status page (2026-11-21)

### Run once, publish a static page

- `pulsewatch -once` checks every target once (in parallel, within `max_concurrent`), waits
  for the alerts, saves the history and exits. Targets being down isn't an error; a run cut
  short leaves the history as it was.
- `-site DIR` (with `-once`) writes the status page as static files: `index.html`,
  `status.json` and `static/`. The static page fetches itself again every minute instead of
  listening to the event stream, and links to `status.json` instead of the API.
- **Incident streaks are saved with the history.** A run can open an incident that the
  previous run's failure started, and a restarted server carries on mid-streak too (it used
  to start counting again).
- The page uses relative links and has its Content-Security-Policy in a `<meta>` tag, so it
  works under a path such as `/pulsewatch/` and without the server's headers.

### Deployed

[.github/workflows/status.yml](.github/workflows/status.yml) runs it on GitHub Actions every
5 minutes and publishes https://gallileee.github.io/pulsewatch/. It watches Market Pulse, the
GitHub Pages certificate and DNS, and GitHub. The history is carried from run to run as an
artifact.

## v0.1.0 — first milestone (2026-11-21)

The first version: checks, incidents, alerts, history, a live status page and metrics.

- **Checks:** HTTP(S) (status codes, body text, slow means degraded), TLS (the certificate
  verified and expiring soon means degraded), DNS (optionally to expected addresses). Each
  target has its own interval and timeout; a limit on checks at once.
- **Incidents** open after N failed checks in a row and are resolved after N good ones; an
  incident open at a restart continues.
- **Alerts** to Discord, Slack or any webhook (JSON), retried after network errors, 429 and
  5xx, sent in the background; webhook URLs from environment variables.
- **History:** the last 60 checks and 90 days of hourly totals per target, saved atomically to
  a JSON file.
- **Status page** with uptime for 24 hours, 7 days and 90 days, daily bars, response times and
  incidents, updated live over server-sent events; works without JavaScript.
- **`/api/status`** (JSON), **`/metrics`** (Prometheus), **`/healthz`**.
- **`-demo`:** a local service that misbehaves on purpose.
- **Docker** image (distroless, non-root) and a Compose file with Prometheus.
