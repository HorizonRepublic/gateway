package nats

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
)

// applyOptions reduces a slice of nats.Option functions onto a fresh
// Options struct so the test can inspect what each option ultimately
// configured. nats.go documents this as the canonical inspection
// pattern — every Option is a `func(*nats.Options) error`.
func applyOptions(t *testing.T, opts []natsgo.Option) natsgo.Options {
	t.Helper()

	base := natsgo.GetDefaultOptions()
	for _, opt := range opts {
		require.NoError(t, opt(&base))
	}

	return base
}

// writeFakeCredsFile materialises a creds-file shaped test fixture so
// nats.UserCredentials option installation succeeds. The file content
// is never parsed at option-install time (only on connect), so the
// minimal armored-block layout is sufficient — the JWT and seed are
// not validated cryptographically.
func writeFakeCredsFile(t *testing.T) string {
	t.Helper()

	const fixture = `-----BEGIN NATS USER JWT-----
fake-jwt-payload
------END NATS USER JWT------

-----BEGIN USER NKEY SEED-----
SUAFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE
------END USER NKEY SEED------
`

	dir := t.TempDir()
	path := dir + "/test.creds"
	require.NoError(t, os.WriteFile(path, []byte(fixture), 0o600))

	return path
}

// TestBuildDisconnectErrHandler_NilErrLogsAtInfo pins the level
// switch on a graceful disconnect: nats.go invokes the handler with
// a nil error during clean shutdown (Drain, Close on a healthy
// socket). Logging that at ERROR floods alert pipelines on every
// pod restart.
func TestBuildDisconnectErrHandler_NilErrLogsAtInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	handler := buildDisconnectErrHandler(logger)
	handler(nil, nil)

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))

	assert.Equal(t, "info", record["level"], "graceful disconnect must log at info")
	assert.Equal(t, "nats disconnected gracefully", record["message"])
	assert.NotContains(t, record, zerolog.ErrorFieldName)
}

// TestBuildOptions_BaselineProducesNoEchoAndConnectionName pins the
// connection-level invariants every gateway pod relies on:
//
//   - NoEcho is set so a connection that both publishes and subscribes
//     does not receive its own messages — the same-connection
//     subscribe/request topology would otherwise see request frames
//     echo back into the inbox subscription and corrupt the reply path.
//   - Name is the gateway client name so operators can identify
//     gateway connections in `nats server list` output.
//   - DontRandomize flips with NATSRandomizeUrls — the env layer
//     defaults to randomize=true, but a deterministic-ordering
//     deployment (testing, sticky-replica routing) MUST be able to
//     opt out and see NoRandomize=true on the resolved options.
func TestBuildOptions_BaselineProducesNoEchoAndConnectionName(t *testing.T) {
	cfg := &config.Config{
		NATSUrls:             []string{"nats://localhost:4222"},
		NATSRandomizeUrls:    true,
		NATSMaxReconnects:    -1,
		NATSReconnectWait:    1 * time.Second,
		NATSReconnectBufSize: 1 << 20,
	}

	got := applyOptions(t, BuildOptions(cfg, zerolog.Nop()))

	assert.True(t, got.NoEcho,
		"NoEcho must be set so same-connection sub/pub topologies do not loop back")
	assert.Equal(t, clientName, got.Name,
		"connection name must surface in `nats server list`")
	assert.False(t, got.NoRandomize,
		"NATSRandomizeUrls=true must leave NoRandomize=false (default randomized order)")
	assert.Equal(t, -1, got.MaxReconnect,
		"NATSMaxReconnects=-1 maps to retry-forever for gateway resilience")
	assert.Equal(t, cfg.NATSReconnectWait, got.ReconnectWait)
	assert.Equal(t, pingInterval, got.PingInterval)
	assert.Equal(t, maxPingsOutstanding, got.MaxPingsOut)
}

// TestBuildOptions_DontRandomizeWhenConfigDisablesIt covers the
// branch where NATSRandomizeUrls=false flips DontRandomize on. Without
// this, a deterministic-routing test or sticky-replica deployment
// could not pin server selection.
func TestBuildOptions_DontRandomizeWhenConfigDisablesIt(t *testing.T) {
	cfg := &config.Config{
		NATSUrls:             []string{"nats://localhost:4222"},
		NATSRandomizeUrls:    false,
		NATSMaxReconnects:    1,
		NATSReconnectWait:    1 * time.Second,
		NATSReconnectBufSize: 1 << 20,
	}

	got := applyOptions(t, BuildOptions(cfg, zerolog.Nop()))

	assert.True(t, got.NoRandomize,
		"NATSRandomizeUrls=false must surface as NoRandomize=true on the resolved options")
}

// TestBuildOptions_CredentialsFileWiresUserJWT pins the auth wiring
// for NGS / decentralised JWT deployments. When NATSCredsFile is set,
// the option list MUST install the credential callbacks (UserJWT and
// SignatureCB) so the connect handshake can authenticate. The exact
// callback implementations are nats.go internals; presence of both is
// the wire-level contract the gateway exposes.
func TestBuildOptions_CredentialsFileWiresUserJWT(t *testing.T) {
	credsPath := writeFakeCredsFile(t)
	cfg := &config.Config{
		NATSUrls:             []string{"nats://localhost:4222"},
		NATSRandomizeUrls:    true,
		NATSMaxReconnects:    -1,
		NATSReconnectWait:    1 * time.Second,
		NATSReconnectBufSize: 1 << 20,
		NATSCredsFile:        credsPath,
	}

	got := applyOptions(t, BuildOptions(cfg, zerolog.Nop()))

	assert.NotNil(t, got.UserJWT,
		"NATSCredsFile must install the UserJWT callback")
	assert.NotNil(t, got.SignatureCB,
		"NATSCredsFile must install the signature callback")
}

// TestBuildOptions_UserPasswordWiresUserInfo pins the password-auth
// branch. When NATSUser is set (and NATSCredsFile is not), UserInfo
// MUST be applied so the connect handshake carries the credentials.
// The else-branch precedence is load-bearing: a deployment using
// creds-file MUST NOT have UserInfo accidentally applied on top.
func TestBuildOptions_UserPasswordWiresUserInfo(t *testing.T) {
	cfg := &config.Config{
		NATSUrls:             []string{"nats://localhost:4222"},
		NATSRandomizeUrls:    true,
		NATSMaxReconnects:    -1,
		NATSReconnectWait:    1 * time.Second,
		NATSReconnectBufSize: 1 << 20,
		NATSUser:             "gateway",
		NATSPassword:         "s3cret",
	}

	got := applyOptions(t, BuildOptions(cfg, zerolog.Nop()))

	assert.Equal(t, "gateway", got.User,
		"NATSUser must surface as Options.User")
	assert.Equal(t, "s3cret", got.Password,
		"NATSPassword must surface as Options.Password")
	assert.Nil(t, got.UserJWT,
		"creds-file branch must not run when only user/password are set")
}

// TestBuildOptions_CredsFilePrecedesUserPassword pins the precedence
// order: credentials file beats user/password if both are set. A
// deployment migrating from user/password to NGS MUST be able to
// leave the legacy credentials in env without accidentally double-
// authenticating or sending a stale password to the server.
func TestBuildOptions_CredsFilePrecedesUserPassword(t *testing.T) {
	credsPath := writeFakeCredsFile(t)
	cfg := &config.Config{
		NATSUrls:             []string{"nats://localhost:4222"},
		NATSRandomizeUrls:    true,
		NATSMaxReconnects:    -1,
		NATSReconnectWait:    1 * time.Second,
		NATSReconnectBufSize: 1 << 20,
		NATSCredsFile:        credsPath,
		NATSUser:             "gateway",
		NATSPassword:         "s3cret",
	}

	got := applyOptions(t, BuildOptions(cfg, zerolog.Nop()))

	assert.NotNil(t, got.UserJWT,
		"creds-file branch must take precedence and install UserJWT")
	assert.Empty(t, got.User,
		"creds-file precedence must leave the password-auth fields empty")
	assert.Empty(t, got.Password,
		"creds-file precedence must leave the password-auth fields empty")
}

// TestConnect_InvalidURLPropagatesWrappedError exercises the only
// non-trivial branch in Connect: a syntactically-valid but
// unreachable URL surfaces as an error from natsgo.Connect, which
// Connect MUST wrap with the URL string for triage. Using
// MaxReconnects=0 ensures the dial fails fast instead of looping.
func TestConnect_InvalidURLPropagatesWrappedError(t *testing.T) {
	cfg := &config.Config{
		// Port 1 reliably refuses connections without external setup.
		NATSUrls:             []string{"nats://127.0.0.1:1"},
		NATSRandomizeUrls:    true,
		NATSMaxReconnects:    0,
		NATSReconnectWait:    10 * time.Millisecond,
		NATSReconnectBufSize: 1 << 20,
	}

	conn, err := Connect(cfg, zerolog.Nop())
	require.Error(t, err, "Connect to a refused address must surface an error")
	assert.Nil(t, conn, "no live conn must leak on error")
	assert.Contains(t, err.Error(), "nats connect",
		"wrapped error must carry the operation context for triage")
}

// TestBuildDisconnectErrHandler_NonNilErrLogsAtError covers the
// genuine-fault branch: a transport error MUST surface at ERROR with
// the cause attached so operators see the failure in their alert
// stream.
func TestBuildDisconnectErrHandler_NonNilErrLogsAtError(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	handler := buildDisconnectErrHandler(logger)
	handler(nil, errors.New("connection reset"))

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))

	assert.Equal(t, "error", record["level"], "transport fault must log at error")
	assert.Equal(t, "nats disconnected with error", record["message"])
	assert.Equal(t, "connection reset", record[zerolog.ErrorFieldName])
}
