# gateway-server

Go edge proxy of the Horizon Gateway product. Terminates HTTP, validates incoming requests, enforces auth and rate limits, and forwards traffic to NestJS-side handlers over Core NATS request/reply. The handler routing table is rebuilt atomically from the NATS-KV `handler_registry` bucket — new SDK handlers appear in the table within milliseconds of the KV update with no server restart.

The companion TypeScript SDK lives at [`libs/gateway-sdk`](../../libs/gateway-sdk). Neither component is independently deployable in production.

## Status: bootstrap

This package currently ships only a Go module skeleton, a `cmd/gateway` entry point that prints a single banner line, and the Nx + golangci-lint plumbing required to build, test, and lint it. No runtime wiring is in place yet.

Subsequent ports add `internal/<package>/` directories in dependency order: `config`, `observability`, `errors`, `codec`, `lifecycle`, `transport/nats`, `transport/http`, `registry`, `routing`, `auth`, `ratelimit`, `proxy`, `trustedproxy`. Each one will be reflected in `cmd/gateway/main.go` and ship its own unit / integration / e2e tests as applicable.

## Build, test, lint

```bash
pnpm exec nx build gateway-server
pnpm exec nx test gateway-server
pnpm exec nx lint gateway-server
```

`build` produces `dist/apps/gateway-server/gateway`. `test` runs `go test -race ./...`. `lint` runs `golangci-lint run ./...` against the workspace-private `.golangci.yml`.

Additional Nx targets — `serve`, `test-integration`, `e2e`, `e2e-up`, `e2e-down`, `bench`, `tidy` — are wired in `project.json` and become useful as the ports below them land.

## Requirements

- Go 1.25+
- [`golangci-lint`](https://golangci-lint.run/) v2 for the `lint` target
