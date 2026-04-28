package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
)

// errQueryValueEmptyJSON signals that UnmarshalJSON received an empty
// byte slice. Exposed as a sentinel so tests can assert via errors.Is
// without depending on the exact wording of the formatted message.
var errQueryValueEmptyJSON = errors.New("query value: empty JSON input")

// QueryValue mirrors the TypeScript `string | readonly string[]` union
// that appears in `IGatewayRequest.query` values. Go has no native sum
// types, so the union is expressed as a struct with two mutually-
// exclusive fields plus custom JSON codec methods that marshal the
// active variant as the correct wire shape.
//
// Exactly one of Multi and Single is meaningful at any time:
//
//   - When Multi is non-nil, the value is a repeated query key and
//     marshals as a JSON array — even if the slice contains a single
//     element, matching the semantics of a key that was observed to
//     repeat on the wire.
//   - When Multi is nil, Single (possibly empty) is the scalar value
//     and marshals as a JSON string.
//
// This layout preserves the wire contract exactly: a single-occurrence
// key that arrived as `?a=1` is serialized as `"1"`, while a repeated
// key `?a=1&a=2` is serialized as `["1","2"]`. Collapsing every value
// to an array would break the TypeScript `Array.isArray` discriminator
// that handler code relies on to distinguish the two cases.
type QueryValue struct {
	// Single holds the scalar value for single-occurrence query keys.
	// Only meaningful when Multi is nil.
	Single string
	// Multi holds the slice for repeated query keys. Non-nil indicates
	// the multi-variant, including for single-element slices where the
	// caller intends to preserve "repeated" semantics.
	Multi []string
}

// NewQueryValueString constructs a QueryValue in the scalar variant.
// Preferred over struct-literal construction because it makes the
// variant choice visible at the call site.
func NewQueryValueString(value string) QueryValue {
	return QueryValue{Single: value}
}

// NewQueryValueStrings constructs a QueryValue in the slice variant.
// Accepts any non-nil slice, including the single-element case. A nil
// slice would be indistinguishable from the scalar variant and is
// therefore normalized to an empty slice.
func NewQueryValueStrings(values []string) QueryValue {
	if values == nil {
		return QueryValue{Multi: []string{}}
	}
	return QueryValue{Multi: values}
}

// MarshalJSON emits the scalar variant as a JSON string and the slice
// variant as a JSON array. Implements encoding/json.Marshaler so
// callers can use QueryValue transparently inside maps, structs, or
// bare values.
func (q QueryValue) MarshalJSON() ([]byte, error) {
	if q.Multi != nil {
		b, err := json.Marshal(q.Multi)
		if err != nil {
			return nil, fmt.Errorf("query value marshal multi: %w", err)
		}

		return b, nil
	}

	b, err := json.Marshal(q.Single)
	if err != nil {
		return nil, fmt.Errorf("query value marshal single: %w", err)
	}

	return b, nil
}

// UnmarshalJSON accepts either a JSON string (routed to the scalar
// variant) or a JSON array of strings (routed to the slice variant).
// Any other shape — number, boolean, object, null — yields an error
// so that malformed inbound envelopes fail loudly rather than silently
// corrupting the handler's view of the query string.
func (q *QueryValue) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return errQueryValueEmptyJSON
	}
	switch data[0] {
	case '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("query value unmarshal string: %w", err)
		}
		q.Single = s
		q.Multi = nil

		return nil
	case '[':
		var slice []string
		if err := json.Unmarshal(data, &slice); err != nil {
			return fmt.Errorf("query value unmarshal slice: %w", err)
		}
		if slice == nil {
			slice = []string{}
		}
		q.Single = ""
		q.Multi = slice

		return nil
	default:
		// Do not return a *json.UnmarshalTypeError here: its Error()
		// method dereferences the embedded reflect.Type, so the
		// zero-value Type would panic the first time the error is
		// formatted (logs, %w wrapping, test assertions). A plain
		// formatted error keeps the message stable and allocation-free
		// at the call site.
		return fmt.Errorf("query value: cannot unmarshal %s into string or []string", string(data))
	}
}
