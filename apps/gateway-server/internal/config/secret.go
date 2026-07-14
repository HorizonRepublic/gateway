package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// redactedPlaceholder is what every non-empty Secret renders as on any
// diagnostic surface (fmt verbs, JSON marshalling, zerolog dumps). The
// bracketed form is deliberately grep-able so operators can confirm
// redaction fired rather than wonder whether the field was empty.
const redactedPlaceholder = "[REDACTED]"

// Secret is a string whose value must never appear on any diagnostic
// surface. It redacts itself at the type level: fmt (%v, %+v, %s via
// Stringer; %#v via GoStringer), encoding/json and encoding.TextMarshaler
// consumers (which covers zerolog's Interface/Any dump path — zerolog's
// default InterfaceMarshalFunc is json.Marshal) all observe
// redactedPlaceholder instead of the value.
//
// Redaction at the type level rather than on Config's String() is
// deliberate: a method on *Config protects only the exact expression
// `fmt.Sprintf("%+v", cfg)` — printing the dereferenced struct, a
// sub-struct copy, or the field itself would still leak. A self-
// redacting field type makes every accidental dump safe regardless of
// how the containing struct reaches the formatter.
//
// The raw value is available exclusively through Reveal(), so every
// intentional use of the secret is a grep-able call site.
//
// An empty Secret renders as the empty string, not the placeholder —
// masking absence would mislead operators into believing a credential
// is configured when it is not.
type Secret string

// Reveal returns the raw secret value. This is the only way to read
// the value out of a Secret; call it exactly at the point of use
// (e.g. building the NATS handshake options) and never store the
// result in a longer-lived structure.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer; non-empty secrets render as the
// redaction placeholder.
func (s Secret) String() string {
	if s == "" {
		return ""
	}

	return redactedPlaceholder
}

// GoString implements fmt.GoStringer so the %#v verb — which bypasses
// Stringer — is redacted as well.
func (s Secret) GoString() string {
	return fmt.Sprintf("config.Secret(%q)", s.String())
}

// MarshalText implements encoding.TextMarshaler. encoding/json falls
// back to TextMarshaler for types that implement it, and zerolog's
// default interface dump goes through json.Marshal, so this single
// method redacts both surfaces.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// MarshalJSON implements json.Marshaler explicitly (json.Marshaler
// wins over TextMarshaler) so the JSON shape is pinned rather than
// inherited from the fallback chain.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(s.String())), nil
}

// readSecretFile loads a secret from path for *_FILE env indirection
// (the standard Kubernetes/Docker secret-mount pattern). Trailing CR/LF
// bytes are trimmed because volume-mounted secrets routinely carry a
// terminal newline that is not part of the credential.
//
// Fail-closed contract: an unreadable path or a file whose content
// trims to empty is a startup error, never a silent empty credential —
// authenticating with an empty password because a mount went missing
// is strictly worse than refusing to boot.
func readSecretFile(path string) (Secret, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}

	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", fmt.Errorf("secret file %q is empty after trimming trailing newlines", path)
	}

	return Secret(value), nil
}
