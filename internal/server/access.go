package server

import (
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/auth"
)

// unlimitedOnly serves next only to callers that may touch every connection (see
// auth.Principal.RequireAllConnections): the Prometheus metrics label every
// connection's jobs and databases, so a key limited to some connections is refused
// with 403 instead of being served a filtered scrape.
func (s *Server) unlimitedOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Without a principal the metrics are public (an administrator opted in).
		if p := auth.PrincipalFrom(r.Context()); p != nil && p.RequireAllConnections() != nil {
			writeError(w, http.StatusForbidden, "forbidden: a key limited to some connections cannot scrape the metrics of every connection")
			return
		}
		next.ServeHTTP(w, r)
	})
}
