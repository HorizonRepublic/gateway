// Package main is the entry point for the gateway-server binary.
//
// At the bootstrap stage the binary does nothing operationally useful:
// it prints a single-line banner identifying the build and exits with
// a zero status. The shape exists so the Go toolchain, golangci-lint,
// the Nx build/test/lint targets, and the smoke test all have a real
// program to chew on. Each subsequent port (config, observability,
// transport/nats, transport/http, registry, routing, auth, ratelimit,
// proxy, lifecycle, trustedproxy) wires itself in here in dependency
// order until the binary is feature-complete relative to its source.
package main

import (
	"fmt"
	"os"
)

// banner returns the operator-facing identification string. Returning
// the value (instead of writing it directly) lets the smoke test
// assert on the result without scraping stdout, keeping the test
// stable across cosmetic edits to the print site.
func banner() string {
	return "horizon-gateway-server: bootstrap stage; no runtime wiring yet"
}

func main() {
	if _, err := fmt.Fprintln(os.Stdout, banner()); err != nil {
		// stdout write failures during the bootstrap stub are still
		// surfaced through the process exit code so a broken pipe in
		// CI shows up as a target failure, not a silent zero.
		os.Exit(1)
	}
}
