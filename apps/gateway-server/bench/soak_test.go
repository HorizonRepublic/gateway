//go:build bench

package bench

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSize_DockerUnits(t *testing.T) {
	mib, gib := float64(1<<20), float64(1<<30)
	cases := map[string]uint64{
		"12.3MiB": uint64(12.3 * mib),
		"1.94GiB": uint64(1.94 * gib),
		"512KiB":  512 << 10,
		"100MB":   100_000_000,
		"42B":     42,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}

	_, err := parseSize("12.3XiB")
	assert.Error(t, err, "unknown unit must error, not silently zero")
}
