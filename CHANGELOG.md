# Changelog

## v0.1.0 — first milestone

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
