package proxy

import (
	"testing"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// smallJSONInput returns a typical POST envelope shared across the
// encoder benchmarks: three headers, one query value, a small JSON
// body, and the usual metadata. Constructed once per benchmark run so
// b.N iterations reflect steady-state cost, not input setup.
func smallJSONInput() *EncodeInput {
	return &EncodeInput{
		Method: "POST",
		Path:   "/users",
		Body:   []byte(`{"email":"a@b.c","name":"Alice"}`),
		Query: map[string]QueryValue{
			"include": NewQueryValueString("profile"),
		},
		Headers: map[string]string{
			"authorization": "Bearer xxxx",
			"x-tenant-id":   "42",
			"content-type":  "application/json",
		},
		Route: routing.Route{
			Subject:      "users-svc__microservice.cmd.users.create",
			Method:       "POST",
			PathTemplate: "/users",
		},
		PathParams: map[string]string{},
		RequestID:  "01HXY0000000000000000000",
		RemoteAddr: "127.0.0.1",
		ReceivedAt: 1000,
		TimeoutMs:  30000,
	}
}

// BenchmarkEncoder_Encode_SmallJSON measures the hot path for a
// typical POST envelope using a caller-owned scratch buffer reused
// across iterations. The buf = buf[:0] reset before each Encode
// mimics what acquirePayload does on pool Get, so this benchmark
// isolates pure encoder cost from sync.Pool overhead.
func BenchmarkEncoder_Encode_SmallJSON(b *testing.B) {
	encoder := NewDefaultEncoder()
	input := smallJSONInput()

	buf := make([]byte, 0, 512)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf = buf[:0]
		if err := encoder.Encode(&buf, input); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncoder_Encode_WithPool measures the full pooled
// acquire/encode/release cycle that the production Handler path
// exercises on every request. Kept separate from
// BenchmarkEncoder_Encode_SmallJSON because the pool adds its own
// sync.Pool overhead which is a meaningful fraction of the cost
// budget and deserves its own baseline line.
func BenchmarkEncoder_Encode_WithPool(b *testing.B) {
	encoder := NewDefaultEncoder()
	input := smallJSONInput()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := acquirePayload()
		if err := encoder.Encode(buf, input); err != nil {
			b.Fatal(err)
		}
		releasePayload(buf)
	}
}
