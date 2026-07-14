package proxy

import (
	"fmt"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/codec"
)

// Status range bounds for a FINAL proxied response. The upper bound
// is the RFC 9110 §15 ceiling; the lower bound starts at 200 because
// 1xx codes are interim responses (RFC 9110 §15.2) that can never
// terminate a request/reply exchange — a gateway writing `100
// Continue` as the sole response leaves standards-compliant clients
// hanging for the real response until their own timeout. A reply
// envelope carrying a 1xx status is upstream garbage and takes the
// same fail-closed 502 path as a status of 0 or 600.
const (
	minValidHTTPStatus = 200
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
// Validates that status is a final-response status (200-599; 1xx is
// interim per RFC 9110 §15.2 and can never be relayed as a NATS reply).
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
