//go:build e2e

package e2e

import (
	"net/http"
	"testing"
)

// drainAndClose closes a WaitForRouteAt response we only needed for
// its status code.
func drainAndClose(resp *http.Response) {
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// TestE2E_ReloadMulti_DeltasLandOnBothReplicas pins watcher fan-out:
// every KV delta — new entry, modified entry, deleted entry — must
// land on EVERY replica's routing table within the WaitForRoute
// budget, not just the primary's. The single-replica reload tests
// probe replica A only; a regression that left replica B's watcher
// behind (stalled subscription, missed delta, partial rebuild) would
// be invisible to them while production traffic through B served
// stale routes.
//
// Reuses the two-replica topology (gateway-server + gateway-server-b)
// and the shadow echo handler; sequential staging through one KV key
// keeps the bucket clean for neighbours.
func TestE2E_ReloadMulti_DeltasLandOnBothReplicas(t *testing.T) {
	WaitReady(t)
	WaitReadyAt(t, GatewayURLB(t))

	const pathNew = "/__reload/multi-new"
	const pathV2 = "/__reload/multi-v2"

	t.Cleanup(func() { deleteHandlerEntry(t, echoHandlerKey) })

	// Stage 1 — new entry lands on both replicas.
	putHandlerEntry(t, echoHandlerKey, http.MethodGet, pathNew)
	drainAndClose(WaitForRouteAt(t, GatewayURL(t), http.MethodGet, pathNew, http.StatusOK))
	drainAndClose(WaitForRouteAt(t, GatewayURLB(t), http.MethodGet, pathNew, http.StatusOK))

	// Stage 2 — modified entry (same key, new path): the new path
	// comes up and the old path drops on BOTH replicas.
	putHandlerEntry(t, echoHandlerKey, http.MethodGet, pathV2)
	drainAndClose(WaitForRouteAt(t, GatewayURL(t), http.MethodGet, pathV2, http.StatusOK))
	drainAndClose(WaitForRouteAt(t, GatewayURLB(t), http.MethodGet, pathV2, http.StatusOK))
	drainAndClose(WaitForRouteAt(t, GatewayURL(t), http.MethodGet, pathNew, http.StatusNotFound))
	drainAndClose(WaitForRouteAt(t, GatewayURLB(t), http.MethodGet, pathNew, http.StatusNotFound))

	// Stage 3 — deleted entry drops on both replicas.
	deleteHandlerEntry(t, echoHandlerKey)
	drainAndClose(WaitForRouteAt(t, GatewayURL(t), http.MethodGet, pathV2, http.StatusNotFound))
	drainAndClose(WaitForRouteAt(t, GatewayURLB(t), http.MethodGet, pathV2, http.StatusNotFound))
}
