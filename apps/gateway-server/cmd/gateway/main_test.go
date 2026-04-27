package main

import (
	"strings"
	"testing"
)

// TestBanner pins the bootstrap stub to a non-empty, identifiable
// string. The assertion intentionally tolerates cosmetic edits to the
// banner copy — only the substring `gateway-server` is enforced — so
// the test fails on real regressions (function deleted, init panics,
// returns empty) and not on every wording tweak.
func TestBanner(t *testing.T) {
	t.Parallel()

	got := banner()
	if got == "" {
		t.Fatal("banner() returned an empty string")
	}
	if !strings.Contains(got, "gateway-server") {
		t.Fatalf("banner() = %q, expected substring %q", got, "gateway-server")
	}
}
