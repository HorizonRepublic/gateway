package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSecretFile drops content into a fresh temp file and returns its
// path. t.TempDir handles cleanup.
func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nats_password")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestLoad_NATSPasswordFile_WinsOverPlainEnv pins the precedence
// contract: when both NATS_PASSWORD and NATS_PASSWORD_FILE are set,
// the file content is the credential. It also pins trailing-newline
// trimming — volume-mounted secrets routinely end in "\n".
func TestLoad_NATSPasswordFile_WinsOverPlainEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NATS_PASSWORD", "env-password")
	t.Setenv("NATS_PASSWORD_FILE", writeSecretFile(t, "file-password\r\n"))

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "file-password", cfg.NATSPassword.Reveal())
}

// TestLoad_NATSPasswordFile_MissingFileFailsClosed pins the fail-closed
// contract: an unreadable secret file aborts startup instead of
// silently degrading to the plain-env (or empty) password.
func TestLoad_NATSPasswordFile_MissingFileFailsClosed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NATS_PASSWORD", "env-password")
	t.Setenv("NATS_PASSWORD_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	cfg, err := Load()
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "NATS_PASSWORD_FILE")
}

// TestLoad_NATSPasswordFile_EmptyFileFailsClosed pins that a file whose
// content trims to nothing is a startup error — an empty credential
// from a broken mount must never reach the NATS handshake.
func TestLoad_NATSPasswordFile_EmptyFileFailsClosed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("NATS_PASSWORD_FILE", writeSecretFile(t, "\n"))

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// TestConfig_SecretNeverLeaksThroughDiagnostics is the redaction
// proof: a loaded Config dumped through every realistic diagnostic
// surface — fmt %v/%+v/%#v on both pointer and value, json.Marshal,
// and a zerolog Interface dump — must never contain the secret, while
// Reveal() still returns it for the intentional consumer.
func TestConfig_SecretNeverLeaksThroughDiagnostics(t *testing.T) {
	const secret = "sup3r-s3cret-pw"
	setRequiredEnv(t)
	t.Setenv("NATS_PASSWORD", secret)

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, secret, cfg.NATSPassword.Reveal(),
		"intentional access path must still see the raw value")

	jsonDump, err := json.Marshal(cfg)
	require.NoError(t, err)

	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)
	logger.Info().Interface("cfg", cfg).Msg("config dump")

	dumps := map[string]string{
		"fmt %v pointer":  fmt.Sprintf("%v", cfg),
		"fmt %+v pointer": fmt.Sprintf("%+v", cfg),
		"fmt %+v value":   fmt.Sprintf("%+v", *cfg),
		"fmt %#v value":   fmt.Sprintf("%#v", *cfg),
		"fmt field":       fmt.Sprint(cfg.NATSPassword),
		"json.Marshal":    string(jsonDump),
		"zerolog dump":    logBuf.String(),
	}
	for surface, dump := range dumps {
		assert.NotContains(t, dump, secret,
			"secret leaked through %s", surface)
	}

	assert.Contains(t, dumps["fmt %+v value"], redactedPlaceholder,
		"redaction placeholder must replace the value, not erase the field")
	assert.Contains(t, dumps["zerolog dump"], redactedPlaceholder)
}

// TestSecret_EmptyRendersEmpty pins that an unset secret renders as
// the empty string on every surface — masking absence would mislead
// operators into believing a credential is configured.
func TestSecret_EmptyRendersEmpty(t *testing.T) {
	var s Secret

	assert.Empty(t, s.String())

	text, err := s.MarshalText()
	require.NoError(t, err)
	assert.Empty(t, string(text))

	jsonBytes, err := json.Marshal(s)
	require.NoError(t, err)
	assert.Equal(t, `""`, string(jsonBytes))
}
