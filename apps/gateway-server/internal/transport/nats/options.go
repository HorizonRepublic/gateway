// Package nats wraps the nats.go client with gateway-specific options,
// a connection-pool Requester implementing proxy.NatsRequester, and a
// small set of lifecycle hooks that surface connection state on the
// gateway's structured logger.
//
// This package is the ONLY place in the gateway codebase that imports
// github.com/nats-io/nats.go. Every other consumer goes through
// proxy.NatsRequester so the NATS concrete type never leaks into the
// request-path code that the proxy handler owns.
package nats

import (
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
)

// Tuning constants for the nats.go client. Extracted so the rationale
// behind each value is visible at the top of the file instead of
// buried in the middle of a long option list. Benchmarks will
// validate whether these values are sized correctly for the
// gateway's target RPS; until then they reflect the nats.go
// recommendations for high-throughput request/reply workloads.
const (
	// clientName is the connection name advertised to the NATS server
	// so operators can identify gateway connections in `nats server
	// list` output.
	clientName = "horizon-gateway-server"

	// pingInterval is how often the client sends PING frames to keep
	// the connection alive and detect half-open sockets.
	pingInterval = 30 * time.Second

	// maxPingsOutstanding is the number of unanswered PINGs tolerated
	// before the client declares the connection dead and triggers
	// reconnection.
	maxPingsOutstanding = 3

	// connectTimeout bounds the TCP + TLS + NATS handshake. Five
	// seconds is deliberately tight so startup fails loud on
	// misconfigured NATS URLs instead of hanging a pod.
	connectTimeout = 5 * time.Second

	// flusherTimeout bounds how long a blocked flush may stall before
	// the client drops the outgoing frame and surfaces the error.
	flusherTimeout = 2 * time.Second

	// syncQueueLen is the per-subscription synchronous message queue
	// length. The request/reply path uses an inbox subscription whose
	// queue must be deep enough to absorb concurrent replies without
	// slowing the publisher.
	syncQueueLen = 8192
)

// BuildOptions constructs the nats.Option slice from cfg and wires
// connection lifecycle callbacks into the supplied logger.
//
// The option ordering intentionally puts tuning before auth so that
// a missing credentials file fails the connect after all tuning has
// been applied — this makes log entries easier to interpret when a
// misconfigured pod starts looping on reconnect.
func BuildOptions(cfg *config.Config, logger zerolog.Logger) []natsgo.Option {
	opts := []natsgo.Option{
		natsgo.Name(clientName),
		natsgo.MaxReconnects(cfg.NATSMaxReconnects),
		natsgo.ReconnectWait(cfg.NATSReconnectWait),
		natsgo.ReconnectBufSize(cfg.NATSReconnectBufSize),
		natsgo.PingInterval(pingInterval),
		natsgo.MaxPingsOutstanding(maxPingsOutstanding),
		natsgo.Timeout(connectTimeout),
		natsgo.FlusherTimeout(flusherTimeout),
		natsgo.SyncQueueLen(syncQueueLen),
		natsgo.NoEcho(),
		natsgo.ConnectHandler(func(c *natsgo.Conn) {
			logger.Info().
				Str("connected_url", c.ConnectedUrl()).
				Strs("discovered_servers", c.DiscoveredServers()).
				Msg("nats connected")
		}),
		natsgo.ReconnectHandler(func(c *natsgo.Conn) {
			logger.Warn().
				Str("connected_url", c.ConnectedUrl()).
				Msg("nats reconnected")
		}),
		natsgo.DisconnectErrHandler(buildDisconnectErrHandler(logger)),
		natsgo.ClosedHandler(func(_ *natsgo.Conn) {
			logger.Error().Msg("nats connection permanently closed")
		}),
	}

	if !cfg.NATSRandomizeUrls {
		opts = append(opts, natsgo.DontRandomize())
	}

	if cfg.NATSCredsFile != "" {
		opts = append(opts, natsgo.UserCredentials(cfg.NATSCredsFile))
	} else if cfg.NATSUser != "" {
		opts = append(opts, natsgo.UserInfo(cfg.NATSUser, cfg.NATSPassword))
	}

	return opts
}

// buildDisconnectErrHandler returns the callback nats.go invokes
// every time the client transitions out of CONNECTED.
//
// nats.go calls DisconnectErrHandler with a nil error on graceful
// disconnects (Drain, Close on a healthy socket) and a non-nil error
// on transport faults. Logging both at ERROR floods alerting
// pipelines on every clean restart, so the handler distinguishes the
// two cases: graceful disconnects log at INFO ("nats disconnected
// gracefully"), genuine errors log at ERROR with the cause attached.
//
// Extracted from BuildOptions so unit tests can assert the level
// switch without spinning up a real NATS connection.
func buildDisconnectErrHandler(logger zerolog.Logger) natsgo.ConnErrHandler {
	return func(_ *natsgo.Conn, err error) {
		if err != nil {
			logger.Error().Err(err).Msg("nats disconnected with error")

			return
		}

		logger.Info().Msg("nats disconnected gracefully")
	}
}
