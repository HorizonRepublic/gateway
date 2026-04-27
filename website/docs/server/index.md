---
sidebar_position: 1
sidebar_label: "Server"
title: "Server — Operating the Go gateway-server"
description: "Configuration, auth verification, rate limiting, CORS, and observability for the Go edge proxy that fronts NATS-backed upstream services."
schema:
  type: Article
  headline: "Server — Operating the Go gateway-server"
  description: "Configuration, auth verification, rate limiting, CORS, and observability for the Go edge proxy that fronts NATS-backed upstream services."
  datePublished: "2026-04-27"
  dateModified: "2026-04-27"
---

# Server

The `gateway-server` binary terminates HTTP, validates requests, enforces auth and rate limits, and forwards traffic to NATS. Operator-facing surfaces (probes, admin, metrics, debug) live on a separate listener from public client traffic.

:::info Coming soon
Per-topic guides land with the gateway-server port. Planned pages:

- **Configuration** — env vars, config file shape, defaults that work in production unchanged.
- **Auth** — the verifier sub-request, JWT and mTLS modes, sharing the request deadline.
- **Rate limiting** — bucket schema, NATS-KV-CAS counters, per-route overrides.
- **CORS** — preflight handling without NATS round-trip.
- **Observability** — Prometheus metrics, structured logs (zerolog), distributed traces.
:::

## Where to next

- [Production deployment](/docs/production/deployment) — running the server at scale.
- [Reference: environment variables](/docs/reference/environment-variables) — the full env var surface.
- [Reference: metrics](/docs/reference/metrics) — every metric the server emits.
