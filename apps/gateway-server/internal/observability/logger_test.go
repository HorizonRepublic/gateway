package observability

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLogger_ValidLevelsAndFormats(t *testing.T) {
	cases := []struct {
		level  string
		format string
	}{
		{"trace", "json"},
		{"debug", "json"},
		{"info", "json"},
		{"warn", "console"},
		{"error", "console"},
		{"TRACE", "JSON"},
		{"Info", "Console"},
	}

	for _, tc := range cases {
		logger, err := NewLogger(tc.level, tc.format)
		require.NoError(t, err, "level=%s format=%s", tc.level, tc.format)
		assert.NotEqual(t, zerolog.Nop(), logger)
	}
}

func TestNewLogger_InvalidLevelReturnsError(t *testing.T) {
	_, err := NewLogger("loud", "json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse log level")
}

func TestNewLogger_InvalidFormatReturnsError(t *testing.T) {
	_, err := NewLogger("info", "xml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported log format")
}

func TestNewLogger_DefaultFormatIsJSON(t *testing.T) {
	_, err := NewLogger("info", "")
	require.NoError(t, err)
}

// TestZerologTimestampFunc_EmitsUTC pins the package init() override
// of zerolog.TimestampFunc. The godoc on NewLogger advertises a UTC
// stamp; without the override zerolog uses time.Now (local time) and
// the documented behaviour silently drifts in any deployment that
// runs in a non-UTC timezone.
func TestZerologTimestampFunc_EmitsUTC(t *testing.T) {
	stamp := zerolog.TimestampFunc()
	assert.Equal(t, time.UTC, stamp.Location(), "TimestampFunc must produce a UTC time value")

	var buf bytes.Buffer
	logger := zerolog.New(&buf).With().Timestamp().Logger()
	logger.Info().Msg("probe")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))

	raw, ok := record[zerolog.TimestampFieldName].(string)
	require.True(t, ok, "timestamp field must be a string")
	assert.True(t, strings.HasSuffix(raw, "Z"), "RFC3339 timestamp must carry the UTC suffix, got %q", raw)
}
