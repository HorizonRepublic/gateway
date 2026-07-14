package nats

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// natsDefaultMaxPayload mirrors the NATS server's stock max_payload
// (1 MiB). The integration suite pins the live value against a real
// server; unit tests here only need a representative constant.
const natsDefaultMaxPayload = 1 << 20

// TestValidatePayloadBudget_DefaultsFitStockMaxPayload pins the
// coherence contract between the gateway's own defaults and a stock
// NATS server: HTTP_MAX_BODY_BYTES=983040 and
// HTTP_MAX_HEADER_BYTES=16384 must fit under max_payload=1048576 with
// the envelope headroom applied. If this fails, an all-default
// deployment refuses to start — the exact foot-gun the default values
// exist to prevent.
func TestValidatePayloadBudget_DefaultsFitStockMaxPayload(t *testing.T) {
	err := ValidatePayloadBudget(natsDefaultMaxPayload, 983040, 16384)
	require.NoError(t, err,
		"all-default config must fit the stock NATS max_payload")
}

// TestValidatePayloadBudget_BodyCapEqualToMaxPayloadFails pins the
// original incident shape: a body cap equal to max_payload cannot
// leave room for the envelope, so startup must fail loudly instead of
// letting near-cap requests 5xx at publish time.
func TestValidatePayloadBudget_BodyCapEqualToMaxPayloadFails(t *testing.T) {
	err := ValidatePayloadBudget(natsDefaultMaxPayload, natsDefaultMaxPayload, 16384)
	require.Error(t, err)
	assert.ErrorContains(t, err, "HTTP_MAX_BODY_BYTES")
	assert.ErrorContains(t, err, "HTTP_MAX_HEADER_BYTES")
	assert.ErrorContains(t, err, "max_payload")
}

// TestValidatePayloadBudget_ExactFitPasses verifies the boundary: a
// body cap that lands exactly on max_payload minus the headroom is
// accepted — the check is `>` (misfit), not `>=`.
func TestValidatePayloadBudget_ExactFitPasses(t *testing.T) {
	headroom := EnvelopeOverheadBudget(16384)
	err := ValidatePayloadBudget(natsDefaultMaxPayload, natsDefaultMaxPayload-headroom, 16384)
	assert.NoError(t, err)
}

// TestValidatePayloadBudget_OneByteOverFails verifies the boundary
// from the other side: one byte past the exact fit must fail.
func TestValidatePayloadBudget_OneByteOverFails(t *testing.T) {
	headroom := EnvelopeOverheadBudget(16384)
	err := ValidatePayloadBudget(natsDefaultMaxPayload, natsDefaultMaxPayload-headroom+1, 16384)
	assert.Error(t, err)
}

// TestValidatePayloadBudget_HeaderCapContributesToBudget pins that
// the header cap participates in the budget: with a huge header cap
// even a small body cap must fail against a small max_payload.
func TestValidatePayloadBudget_HeaderCapContributesToBudget(t *testing.T) {
	err := ValidatePayloadBudget(natsDefaultMaxPayload, 1024, 1<<20)
	require.Error(t, err,
		"header-buffer bytes travel on the envelope and must count against max_payload")
}

// TestEnvelopeOverheadBudget_Shape pins the budget formula so an
// accidental change to the inflation factor or the fixed overhead
// surfaces as a reviewed diff, not a silent behaviour shift in the
// startup validation.
func TestEnvelopeOverheadBudget_Shape(t *testing.T) {
	assert.Equal(t, int64(3*16384+4096), EnvelopeOverheadBudget(16384))
}
