package nats

import (
	"errors"
	"fmt"
	"strings"

	natsgo "github.com/nats-io/nats.go"
	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
)

// ErrNoNATSURLs is returned by Connect when cfg.NATSUrls is empty
// (or carries only blank entries). nats.go silently falls back to
// localhost when given an empty URL string, which is a startup-time
// misconfiguration trap: a pod with NATS_URLS unset would happily
// connect to a non-existent localhost server and surface the
// failure later as obscure publish errors. Failing loud at Connect
// keeps the bootstrap honest.
var ErrNoNATSURLs = errors.New("nats: NATS_URLS required, got empty")

// Connect establishes a NATS connection using cfg and returns a live
// *nats.Conn. The comma-separated URL list supports clustered, single-
// node, and DNS-based discovery transparently — nats.go resolves all
// three formats from a comma-joined URL string.
//
// Returns ErrNoNATSURLs when cfg.NATSUrls contains no non-blank
// entries — the env layer should already enforce this via the
// "required" tag, but the defense-in-depth check guards against a
// future config refactor that drops the tag.
//
// On failure the returned error wraps the underlying nats.go error so
// callers see the original cause when logging with zerolog's Err.
func Connect(cfg *config.Config, logger zerolog.Logger) (*natsgo.Conn, error) {
	urls, err := joinNATSURLs(cfg.NATSUrls)
	if err != nil {
		return nil, err
	}

	opts := BuildOptions(cfg, logger)

	nc, err := natsgo.Connect(urls, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect %q: %w", urls, err)
	}
	return nc, nil
}

// joinNATSURLs validates the operator-supplied URL list and returns
// the comma-joined form nats.go expects. Blank entries are dropped
// before joining so a stray empty value does not surface to nats.go
// as a malformed URL. An empty result is treated as a fatal config
// error and returns ErrNoNATSURLs.
func joinNATSURLs(urls []string) (string, error) {
	cleaned := make([]string, 0, len(urls))
	for _, u := range urls {
		if trimmed := strings.TrimSpace(u); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}

	if len(cleaned) == 0 {
		return "", ErrNoNATSURLs
	}

	return strings.Join(cleaned, ","), nil
}
