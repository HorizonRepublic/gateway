// Package observability provides the gateway's logging, request-identifier,
// tracing, and metrics primitives.
//
// Every upstream layer in the gateway-server depends on this package for
// structured logging and request correlation. Keeping these foundations
// in a dedicated package — separate from config, transport, and business
// logic — prevents the cross-cutting concerns of "how do we log?" and
// "how do we trace?" from leaking into every other package's import
// graph, and it gives us a single place to evolve the telemetry stack
// (swap zerolog for slog, add OTEL exporters, ...) without touching
// consumers.
package observability

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// init forces zerolog to stamp every event timestamp in UTC.
//
// zerolog defaults TimestampFunc to time.Now, which renders in the
// local timezone of whichever pod hosts the gateway. Multi-region
// deployments would emit interleaved logs whose timestamps cannot be
// compared without timezone arithmetic. UTC is the only reliable
// shared frame of reference and matches the godoc claim made on
// NewLogger about UTC-stamped output.
func init() {
	zerolog.TimestampFunc = func() time.Time {
		return time.Now().UTC()
	}
}

// serviceName is stamped on every log entry as the "service" field so
// downstream log aggregators (Loki, Elasticsearch, Datadog) can filter
// gateway logs out of a multi-service stream without parsing.
const serviceName = "horizon-gateway-server"

// NewLogger constructs the gateway's root zerolog logger from the
// configured level and format strings.
//
// Level is any of trace, debug, info, warn, error, fatal, panic, or
// disabled (case-insensitive — the raw string is lowercased before
// parsing). Format is either "json" (the production default, one JSON
// object per line on stdout) or "console" (a human-friendly coloured
// writer useful for local development). An empty format string is
// treated as "json" so callers can zero-value their config without
// surprise.
//
// The returned logger is stamped with a UTC timestamp and the
// "service" field; child loggers derived via With() inherit both. If
// the level is not a valid zerolog level name, or the format is
// neither json nor console, an error is returned alongside a no-op
// logger so the caller can safely unwrap without worrying about a nil
// receiver.
func NewLogger(level, format string) (zerolog.Logger, error) {
	parsedLevel, err := zerolog.ParseLevel(strings.ToLower(level))
	if err != nil {
		return zerolog.Nop(), fmt.Errorf("parse log level %q: %w", level, err)
	}

	var base zerolog.Logger
	switch strings.ToLower(format) {
	case "console":
		base = zerolog.New(zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "15:04:05.000",
		})
	case "json", "":
		base = zerolog.New(os.Stdout)
	default:
		return zerolog.Nop(), fmt.Errorf(
			"unsupported log format %q (expected 'json' or 'console')",
			format,
		)
	}

	return base.
		Level(parsedLevel).
		With().
		Timestamp().
		Str("service", serviceName).
		Logger(), nil
}
