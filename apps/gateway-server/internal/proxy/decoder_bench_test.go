package proxy

import "testing"

// BenchmarkDecoder_Decode_SmallJSON measures a typical reply parse:
// status + one custom header + a small JSON body. The input slice
// is reused across iterations so the benchmark reflects parse cost
// alone, not any per-iteration allocation inside the harness.
func BenchmarkDecoder_Decode_SmallJSON(b *testing.B) {
	decoder := NewDefaultDecoder()
	payload := []byte(`{"status":201,"headers":{"x-custom":"yes"},"body":{"id":"42","name":"Alice"}}`)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := decoder.Decode(payload); err != nil {
			b.Fatal(err)
		}
	}
}
