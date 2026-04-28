//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// TestMain owns the Compose stack lifecycle for the whole test
// process. m.Run runs every Test* function against the shared stack;
// stopStack tears containers and volumes down even on panic.
func TestMain(m *testing.M) {
	ctx := context.Background()

	s, err := startStack(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e harness startup failed: %v\n", err)
		os.Exit(1)
	}
	stack = s
	stackOnce.Do(func() {})

	code := m.Run()

	stopStack(ctx, s)
	os.Exit(code)
}
