//go:build bench

package bench

import (
	"net/http"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// Scenario is one hot path under test: a stable name plus a vegeta
// targeter that emits requests against the live gateway.
type Scenario struct {
	Name     string
	Targeter vegeta.Targeter
}

// Scenarios returns the three baseline scenarios bound to baseURL:
//   - proxy-echo:   unauth GET /users/alice (seeded fixture user), pure HTTP→NATS→Nest→HTTP.
//   - auth-verify:  GET /me with a valid bearer token (verifier hop on every call).
//   - rate-limited: GET /rl/bench, limiter evaluates but never rejects under load.
func Scenarios(baseURL string) []Scenario {
	authHeader := http.Header{}
	authHeader.Set("Authorization", "Bearer demo-alice")

	return []Scenario{
		{
			Name: "proxy-echo",
			Targeter: vegeta.NewStaticTargeter(vegeta.Target{
				Method: "GET",
				URL:    baseURL + "/users/alice",
			}),
		},
		{
			Name: "auth-verify",
			Targeter: vegeta.NewStaticTargeter(vegeta.Target{
				Method: "GET",
				URL:    baseURL + "/me",
				Header: authHeader,
			}),
		},
		{
			Name: "rate-limited",
			Targeter: vegeta.NewStaticTargeter(vegeta.Target{
				Method: "GET",
				URL:    baseURL + "/rl/bench",
			}),
		},
	}
}
