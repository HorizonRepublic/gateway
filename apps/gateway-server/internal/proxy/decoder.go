package proxy

import (
	"fmt"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/codec"
)

// Status range bounds taken from RFC 9110 §15. Values outside this
// range are rejected as malformed because they are unparseable by
// standards-compliant HTTP clients.
const (
	minValidHTTPStatus = 100
	maxValidHTTPStatus = 599
)

// Decoder parses GatewayReply envelopes returned by Nest over NATS.
// The contract is narrow on purpose so fakes stub it trivially and an
// alternative decoder (protobuf, streaming) can swap in without
// touching Handler.
type Decoder interface {
	Decode(replyBytes []byte) (*GatewayReply, error)
}

// DefaultDecoder parses JSON replies through the codec package. It is
// stateless and safe for concurrent use.
type DefaultDecoder struct{}

// NewDefaultDecoder returns a JSON-based Decoder. The returned pointer
// is safe to share across goroutines.
func NewDefaultDecoder() *DefaultDecoder {
	return &DefaultDecoder{}
}

// Compile-time assertion that DefaultDecoder satisfies the Decoder
// contract.
var _ Decoder = (*DefaultDecoder)(nil)

// Decode parses replyBytes into a GatewayReply.
//
// Validates that status is within the RFC 9110 status range (100-599).
// Out-of-range or unparseable payloads produce an error so the caller
// can return a 502 Bad Gateway upstream rather than forwarding garbage
// to the HTTP client.
func (d *DefaultDecoder) Decode(replyBytes []byte) (*GatewayReply, error) {
	reply := &GatewayReply{}
	if err := codec.Unmarshal(replyBytes, reply); err != nil {
		return nil, fmt.Errorf("proxy decoder unmarshal: %w", err)
	}
	if reply.Status < minValidHTTPStatus || reply.Status > maxValidHTTPStatus {
		return nil, fmt.Errorf("proxy decoder: invalid status %d", reply.Status)
	}
	return reply, nil
}
