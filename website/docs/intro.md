---
slug: /
sidebar_position: 1
sidebar_label: "Introduction"
title: "Horizon Gateway — Edge HTTP-to-NATS Proxy with Type-Bridged SDK"
description: "An edge HTTP gateway that terminates client traffic in Go and forwards it to NATS-backed upstream services, paired with a NestJS SDK that declares routes and mirrors the wire contracts."
schema:
  type: Article
  headline: "Horizon Gateway — Edge HTTP-to-NATS Proxy with Type-Bridged SDK"
  description: "An edge HTTP gateway that terminates client traffic in Go and forwards it to NATS-backed upstream services, paired with a NestJS SDK that declares routes and mirrors the wire contracts."
  datePublished: "2026-04-27"
  dateModified: "2026-04-27"
---

# Introduction

Horizon Gateway is one product made of two components that operate exclusively in tandem:

- **`gateway-server`** — Go edge proxy. Terminates HTTP, validates requests, enforces auth and rate limits, and forwards traffic to NATS-backed upstream services.
- **`gateway-sdk`** — TypeScript NestJS / Fastify SDK. Declares routes via decorators, ships an exception filter and an interceptor, and bridges the server's wire contracts into your codebase as types.

Neither component is independently deployable. They share a NATS message bus and a NATS-KV registry: SDK consumers declare routes, the server enforces them.

The product targets healthcare-grade traffic at ~100k RPS. The bias is always **harder to build, impossible to misuse** over easy to ship, easy to misconfigure.

## Where to start

Pick an entry point based on your goal:

- **New here?** — [Why Horizon Gateway?](/docs/getting-started/why-horizon-gateway) → [Installation](/docs/getting-started/installation) → [Quick Start](/docs/getting-started/quick-start)
- **Understanding the system?** — [Architecture overview](/docs/architecture/overview), [Request lifecycle](/docs/architecture/request-lifecycle), [Wire protocol](/docs/architecture/wire-protocol)
- **Writing handlers in NestJS?** — [SDK guides](/docs/sdk/declaring-routes)
- **Operating the server?** — [Server configuration](/docs/server/configuration), [Production deployment](/docs/production/deployment)
- **Looking for a contract?** — [Reference](/docs/reference/wire-contracts)

The full table of contents lives in the sidebar on the left.

:::info Project status
Horizon Gateway is in Phase 0 — the workspace and tooling are in place; component code is being ported from a working precedent. Pages marked _Coming soon_ land as their respective port progresses.
:::
