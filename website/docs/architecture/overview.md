---
sidebar_position: 1
sidebar_label: "Architecture"
title: "Architecture Overview — How gateway-server, gateway-sdk, and NATS fit together"
description: "Deployment topology and request flow for Horizon Gateway: HTTP termination in Go, route registry in NATS-KV, upstream handlers via NestJS SDK."
schema:
  type: Article
  headline: "Architecture Overview — How gateway-server, gateway-sdk, and NATS fit together"
  description: "Deployment topology and request flow for Horizon Gateway: HTTP termination in Go, route registry in NATS-KV, upstream handlers via NestJS SDK."
  datePublished: "2026-04-27"
  dateModified: "2026-04-27"
---

# Architecture Overview

:::info Coming soon
The deployment topology, component responsibilities, and a request walkthrough land with the first end-to-end port. Expected content: a topology diagram (clients → gateway-server → NATS → SDK consumers), the role of the NATS-KV registry, ownership boundaries, and what the SDK is and is not allowed to do.
:::

## Where to next

- [Request lifecycle](/docs/architecture/request-lifecycle) — what happens between socket-accept and response-flush.
- [Route registry](/docs/architecture/route-registry) — how routes are declared, stored, and discovered.
- [Wire protocol](/docs/architecture/wire-protocol) — the HTTP↔NATS envelope contract.
