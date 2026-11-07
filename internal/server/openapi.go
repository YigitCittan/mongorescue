package server

import (
	_ "embed"
	"net/http"
)

// openAPIPath is the OpenAPI description of the API. It is not public (the public
// surface is pinned to six routes): any signed-in user or API key with the read
// scope may fetch it.
const openAPIPath = "/api/v1/openapi.json"

// openAPIDocument is the OpenAPI 3.1 description of /api/v1 as JSON. It is a copy of
// docs/openapi.yaml, which is the source; TestOpenAPIJSONMatchesYAML fails when the
// two differ and regenerates this file with -update.
//
//go:embed openapi.json
var openAPIDocument []byte

// handleOpenAPI serves the embedded OpenAPI document.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(openAPIDocument)
}
