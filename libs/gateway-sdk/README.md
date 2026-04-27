# @horizon-republic/gateway-sdk

NestJS / Fastify SDK for Horizon Gateway. Declares routes, ships the exception filter and interceptor, mirrors the gateway-server's wire contracts.

## Status

Bootstrap. The package compiles and the test harness runs; runtime types and decorators land in subsequent ports. The published surface is intentionally empty until the first contract feature ships.

## Install

```bash
pnpm add @horizon-republic/gateway-sdk
```

Peer dependencies (must exist in the consuming app):

- `@nestjs/common >= 12.0.0-alpha`
- `@nestjs/core >= 12.0.0-alpha`
- `@nestjs/microservices >= 12.0.0-alpha`
- `reflect-metadata >= 0.2.0`
- `rxjs >= 7.8.0`

## Repository

Source lives at `libs/gateway-sdk/` in [`HorizonRepublic/gateway`](https://github.com/HorizonRepublic/gateway). The lib is part of the Horizon Gateway product and ships in tandem with `apps/gateway-server/` (Go).
