---
sidebar_position: 1
sidebar_label: "Reference"
title: "Reference — Wire contracts, headers, errors, metrics, env vars"
description: "Formal contracts the gateway-server and gateway-sdk both honour: HTTP↔NATS envelopes, header conventions, error catalog, metric names, environment variables."
schema:
  type: Article
  headline: "Reference — Wire contracts, headers, errors, metrics, env vars"
  description: "Formal contracts the gateway-server and gateway-sdk both honour: HTTP↔NATS envelopes, header conventions, error catalog, metric names, environment variables."
  datePublished: "2026-04-27"
  dateModified: "2026-04-27"
---

# Reference

Stable contracts. Names and shapes change only on major version bumps; minor releases extend, never break. External clients (Go, Python, Rust services that talk to the gateway over NATS directly) need only honour these pages to interoperate.

:::info Coming soon
Reference pages land alongside the components they document. Planned pages:

- **Wire contracts** — request and response envelope schemas exchanged over NATS.
- **Header contract** — every header the server reads and writes, including W3C Trace Context propagation.
- **Error catalog** — sentinel errors, their wire codes, and how clients discriminate them.
- **Metrics** — every metric the server emits, with cardinality bounds.
- **Environment variables** — the full env var surface with defaults.
:::

## Where to next

- [Architecture: wire protocol](/docs/architecture/wire-protocol) — the conceptual model behind the contracts here.
