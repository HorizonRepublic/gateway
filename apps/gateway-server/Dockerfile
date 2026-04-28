# syntax=docker/dockerfile:1.7

# Stage 1 - build the static Go binary.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod go mod download
COPY . .
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# Stage 2 - distroless runtime.
FROM gcr.io/distroless/static-debian12 AS runtime
COPY --from=build /out/gateway /gateway
EXPOSE 8080
ENTRYPOINT ["/gateway"]
