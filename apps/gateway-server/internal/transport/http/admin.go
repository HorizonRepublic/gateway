package http

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// routeDumpDTO is the wire shape of one route on the admin
// introspection surface. It is a deliberate projection of
// routing.Route, not the struct itself: static header VALUES are
// dropped (they can carry injected credentials) and only the keys are
// listed, while auth is flattened to the facts an operator debugging
// "did my route land?" needs — whether the route is protected, the
// verifier subject, and whether auth is optional.
type routeDumpDTO struct {
	Method     string        `json:"method"`
	Path       string        `json:"path"`
	Subject    string        `json:"subject"`
	Auth       *routeAuthDTO `json:"auth,omitempty"`
	CORS       any           `json:"cors,omitempty"`
	RateLimit  any           `json:"rateLimit,omitempty"`
	TimeoutMs  int64         `json:"timeoutMs,omitempty"`
	HeaderKeys []string      `json:"headerKeys,omitempty"`
}

type routeAuthDTO struct {
	Required        bool   `json:"required"`
	Optional        bool   `json:"optional"`
	VerifierSubject string `json:"verifierSubject"`
}

type routeDumpResponse struct {
	Count  int            `json:"count"`
	Routes []routeDumpDTO `json:"routes"`
}

// newRouteDumpHandler serves the current routing table as JSON on the
// operator listener. provider reads the live table (the atomic value
// the registry watcher swaps on every rebuild), so the dump always
// reflects what THIS pod is serving right now — the answer to route
// drift and "did the SDK registration reach this replica?" without
// log-diving. Operator-listener-only by construction: it never shares
// the public socket.
func newRouteDumpHandler(provider func() routing.Table) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		table := provider()
		if table == nil {
			// The watcher's initial snapshot has not landed yet; report
			// an empty table rather than 500 so the endpoint is usable
			// during cold boot.
			writeJSON(w, routeDumpResponse{Count: 0, Routes: []routeDumpDTO{}})

			return
		}

		routes := table.Routes()
		dto := make([]routeDumpDTO, 0, len(routes))
		for i := range routes {
			dto = append(dto, toRouteDumpDTO(routes[i]))
		}

		writeJSON(w, routeDumpResponse{Count: len(dto), Routes: dto})
	}
}

func toRouteDumpDTO(route routing.Route) routeDumpDTO {
	d := routeDumpDTO{
		Method:    route.Method,
		Path:      route.PathTemplate,
		Subject:   route.Subject,
		TimeoutMs: route.Timeout.Milliseconds(),
	}

	if route.Auth != nil {
		d.Auth = &routeAuthDTO{
			Required:        true,
			Optional:        route.Auth.Optional,
			VerifierSubject: route.Auth.VerifierSubject,
		}
	}

	if route.CORS != nil {
		d.CORS = route.CORS
	}

	if route.RateLimit != nil {
		d.RateLimit = route.RateLimit
	}

	if len(route.Headers) > 0 {
		keys := make([]string, 0, len(route.Headers))
		for k := range route.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d.HeaderKeys = keys
	}

	return d
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
