---
sidebar_position: 1
sidebar_label: "SDK"
title: "SDK — TypeScript guides for gateway-sdk"
description: "How to declare routes, validate requests, handle exceptions, and consume the type bridge from a NestJS service using the gateway-sdk package."
schema:
  type: Article
  headline: "SDK — TypeScript guides for gateway-sdk"
  description: "How to declare routes, validate requests, handle exceptions, and consume the type bridge from a NestJS service using the gateway-sdk package."
  datePublished: "2026-04-27"
  dateModified: "2026-04-27"
---

# SDK

The `gateway-sdk` package is the NestJS-side surface of Horizon Gateway. It declares routes, exposes decorators, ships an exception filter and an interceptor, and bridges the server's wire contracts into your codebase as types.

:::info Coming soon
Per-topic guides land with the SDK port. Planned pages:

- **Declaring routes** — `@Route()` decorator, registry registration, scope conventions.
- **Request validation** — `typia` integration, runtime + compile-time invariants.
- **Exception filter** — error envelope, status code mapping, custom error types.
- **Type bridge** — keeping TypeScript types in sync with Go structs on the wire.
:::

## Where to next

- [Architecture overview](/docs/architecture/overview) — where the SDK fits in the system.
- [Wire protocol](/docs/architecture/wire-protocol) — the contract the SDK enforces on the consumer side.
