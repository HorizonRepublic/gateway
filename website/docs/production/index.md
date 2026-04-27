---
sidebar_position: 1
sidebar_label: "Production"
title: "Production — Deployment, health, performance, and graceful shutdown"
description: "Operating Horizon Gateway in production: deployment topologies, health probes, graceful shutdown semantics, and the performance envelope."
schema:
  type: Article
  headline: "Production — Deployment, health, performance, and graceful shutdown"
  description: "Operating Horizon Gateway in production: deployment topologies, health probes, graceful shutdown semantics, and the performance envelope."
  datePublished: "2026-04-27"
  dateModified: "2026-04-27"
---

# Production

The defaults are designed to work for production without tuning. This section documents what the defaults guarantee, where the failure modes are, and what to change when the workload changes.

:::info Coming soon
Per-topic guides land alongside production hardening work. Planned pages:

- **Deployment** — single-region and multi-region topologies, Kubernetes manifests, NATS placement.
- **Health checks** — liveness vs readiness, what each probe answers, drain semantics.
- **Graceful shutdown** — in-flight request budget, NATS subscription drain, signal handling.
- **Performance** — latency and allocation envelope, hot-path discipline, benchmarks at 100k RPS.
:::

## Where to next

- [Server configuration](/docs/server/configuration) — knobs that affect production behaviour.
- [Reference: metrics](/docs/reference/metrics) — what to watch in Grafana / Prometheus.
