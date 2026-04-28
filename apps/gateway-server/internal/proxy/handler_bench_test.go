package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// BenchmarkPreviewClaimsForLog_4KB pins the slice-before-string
// optimisation in previewClaimsForLog. The pre-fix shape allocated
// proportional to input size (a 4 KiB claim → ~4 KiB string), which
// the redaction step then operated on after truncating to 256 bytes.
// The fix slices the byte view to the preview cap first, so the
// string allocation is bounded by maxPreview regardless of the input.
//
// Expected: bytes-per-op ≈ maxPreview (256) regardless of input size.
func BenchmarkPreviewClaimsForLog_4KB(b *testing.B) {
	payload := json.RawMessage([]byte(strings.Repeat("a", 4096)))

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = previewClaimsForLog(payload)
	}
}

func BenchmarkPreviewClaimsForLog_Small(b *testing.B) {
	payload := json.RawMessage([]byte(`{"sub":"u1","scope":["read","write"]}`))

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = previewClaimsForLog(payload)
	}
}
