---
sidebar_position: 1
sidebar_label: "Development"
title: "Development — Testing, contributing, benchmarking"
description: "Contributor docs for Horizon Gateway: how to run the test suites, what the test tier policy requires, how to contribute, how the benchmarks are structured."
schema:
  type: Article
  headline: "Development — Testing, contributing, benchmarking"
  description: "Contributor docs for Horizon Gateway: how to run the test suites, what the test tier policy requires, how to contribute, how the benchmarks are structured."
  datePublished: "2026-04-27"
  dateModified: "2026-04-27"
---

# Development

If you are reading this section, you are touching the code, not just the API. Tests are first-class citizens: every behaviour change ships with the test that proves it, and the three-tier mandate (unit, integration, e2e) governs which tier covers which kind of change.

:::info Coming soon
Per-topic guides land alongside the test suites and CI pipeline. Planned pages:

- **Testing** — three-tier mandate, runner setup (Vitest for SDK, `go test` for server), Testcontainers for NATS, naming conventions.
- **Contributing** — pull request flow, microfeature ceiling, how to file an issue.
- **Benchmarking** — autocannon for HTTP, `go test -bench` for hot paths, baseline numbers.
:::

## Where to next

- [Architecture overview](/docs/architecture/overview) — the system you are about to change.
