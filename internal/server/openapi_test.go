package server

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/apierr"
	"github.com/yigitcittan/mongorescue/internal/auth"
)

// updateOpenAPI regenerates openapi.json from docs/openapi.yaml:
//
//	go test ./internal/server -run OpenAPI -update
var updateOpenAPI = flag.Bool("update", false, "regenerate internal/server/openapi.json from docs/openapi.yaml")

const openAPISource = "../../docs/openapi.yaml"

// specMethods are the operation keys of a path item.
var specMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// loadSpec parses docs/openapi.yaml.
func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	src, err := os.ReadFile(openAPISource)
	if err != nil {
		t.Fatal(err)
	}
	v, err := parseYAMLSubset(string(src))
	if err != nil {
		t.Fatalf("docs/openapi.yaml: %v", err)
	}
	doc, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("docs/openapi.yaml is %T, want a mapping", v)
	}
	return doc
}

// asMap and asList read a node of the parsed spec, failing the test on another type.
func asMap(t *testing.T, where string, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want a mapping", where, v)
	}
	return m
}

func asList(t *testing.T, where string, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T, want a sequence", where, v)
	}
	return l
}

// specOperations returns the "METHOD /path" of every operation of the spec.
func specOperations(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for path, item := range asMap(t, "paths", doc["paths"]) {
		for method, op := range asMap(t, "paths."+path, item) {
			if !slices.Contains(specMethods, method) {
				continue
			}
			out[strings.ToUpper(method)+" "+path] = asMap(t, "paths."+path+"."+method, op)
		}
	}
	return out
}

// isAPIRoute reports whether a ServeMux pattern is a /api/v1 route, which the spec
// describes. The others are the dashboard, the Prometheus endpoint, MCP and the
// single sign-on redirects, each with its own documentation.
func isAPIRoute(pattern string) bool {
	_, path, ok := strings.Cut(pattern, " ")
	return ok && strings.HasPrefix(path, "/api/v1/")
}

func TestOpenAPIJSONMatchesYAML(t *testing.T) {
	doc := loadSpec(t)
	want, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if *updateOpenAPI {
		if err := os.WriteFile("openapi.json", want, 0o644); err != nil { //nolint:gosec // a checked-in source file
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(openAPIDocument, want) {
		t.Fatal("internal/server/openapi.json is out of date with docs/openapi.yaml; run: go test ./internal/server -run OpenAPI -update")
	}
}

func TestOpenAPIMatchesRouteTable(t *testing.T) {
	f := newScopeFixture(t)
	var routes []string
	for _, p := range f.srv.patterns {
		if isAPIRoute(p) {
			routes = append(routes, p)
			continue
		}
		// Everything else must be a route the spec deliberately leaves out.
		_, path := concrete(p)
		if p != "GET /" && path != "/metrics" && path != MCPPath && !strings.HasPrefix(path, oidcFlowCookiePath) {
			t.Errorf("route %q is neither under /api/v1/ nor a known non-API route", p)
		}
	}
	if len(routes) < 100 {
		t.Fatalf("only %d /api/v1 routes registered; the fixture is missing some", len(routes))
	}
	spec := specOperations(t, loadSpec(t))
	for _, r := range routes {
		if _, ok := spec[r]; !ok {
			t.Errorf("route %q is not in docs/openapi.yaml", r)
		}
	}
	for op := range spec {
		if !slices.Contains(routes, op) {
			t.Errorf("docs/openapi.yaml describes %q, which the server does not register", op)
		}
	}
}

func TestOpenAPIErrorCodesMatchAPIErr(t *testing.T) {
	doc := loadSpec(t)
	schemas := asMap(t, "components.schemas", asMap(t, "components", doc["components"])["schemas"])
	enum := asList(t, "ErrorCode.enum", asMap(t, "ErrorCode", schemas["ErrorCode"])["enum"])
	var got []string
	for _, c := range enum {
		got = append(got, c.(string))
	}
	want := apierr.All()
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("ErrorCode enum = %v, want the codes of internal/apierr %v", got, want)
	}
	ready := asList(t, "ReadyCode.enum", asMap(t, "ReadyCode", schemas["ReadyCode"])["enum"])
	for _, r := range []string{ReadyNotActive, ReadyDraining, ReadyStoreUnwritable, ReadyDiskFull} {
		if !slices.Contains(ready, any(r)) {
			t.Errorf("ReadyCode enum lacks %q", r)
		}
	}
	if len(ready) != 4 {
		t.Errorf("ReadyCode enum has %d values, want the 4 Ready* reasons", len(ready))
	}
}

func TestOpenAPIScopesAndSecurityMatchServer(t *testing.T) {
	for key, op := range specOperations(t, loadSpec(t)) {
		method, path := concrete(key)
		scope, hasScope := op["x-required-scope"].(string)
		security := asList(t, key+" security", op["security"])
		if publicPaths[path] {
			if hasScope || len(security) != 0 {
				t.Errorf("%s is public but declares scope %q and %d security requirements", key, scope, len(security))
			}
			continue
		}
		if want := string(requiredScope(key)); scope != want {
			t.Errorf("%s: x-required-scope %q, the server requires %q", key, scope, want)
		}
		if !slices.Contains(auth.Scopes(), auth.Scope(scope)) {
			t.Errorf("%s: unknown scope %q", key, scope)
		}
		unsafe := method != http.MethodGet && method != http.MethodHead
		var withCSRF bool
		for _, s := range security {
			if _, ok := asMap(t, key+" security", s)["csrfToken"]; ok {
				withCSRF = true
			}
		}
		if withCSRF != unsafe {
			t.Errorf("%s: csrfToken required=%v, want %v (unsafe methods need the CSRF token)", key, withCSRF, unsafe)
		}
	}
}

func TestOpenAPIIsStructurallyValid(t *testing.T) {
	doc := loadSpec(t)
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Errorf("openapi = %v, want 3.1.x", doc["openapi"])
	}
	info := asMap(t, "info", doc["info"])
	for _, k := range []string{"title", "version"} {
		if s, _ := info[k].(string); s == "" {
			t.Errorf("info.%s is empty", k)
		}
	}
	components := asMap(t, "components", doc["components"])
	schemes := asMap(t, "securitySchemes", components["securitySchemes"])
	tags := map[string]bool{}
	for _, tg := range asList(t, "tags", doc["tags"]) {
		tags[asMap(t, "tag", tg)["name"].(string)] = true
	}

	// Every $ref points at something that exists.
	var walk func(where string, v any)
	walk = func(where string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if r, ok := x["$ref"].(string); ok {
				if !refResolves(doc, r) {
					t.Errorf("%s: $ref %q does not resolve", where, r)
				}
			}
			for k, c := range x {
				walk(where+"."+k, c)
			}
		case []any:
			for _, c := range x {
				walk(where+"[]", c)
			}
		}
	}
	walk("$", doc)

	for name, v := range asMap(t, "securitySchemes", schemes) {
		if asMap(t, name, v)["type"] == nil {
			t.Errorf("security scheme %s has no type", name)
		}
	}
	ids := map[string]string{}
	paramRe := regexp.MustCompile(`\{(\w+)\}`)
	for key, op := range specOperations(t, doc) {
		if id, _ := op["operationId"].(string); id == "" {
			t.Errorf("%s has no operationId", key)
		} else if other, dup := ids[id]; dup {
			t.Errorf("%s and %s share operationId %q", key, other, id)
		} else {
			ids[id] = key
		}
		if s, _ := op["summary"].(string); s == "" {
			t.Errorf("%s has no summary", key)
		}
		for _, tg := range asList(t, key+" tags", op["tags"]) {
			if !tags[tg.(string)] {
				t.Errorf("%s uses undeclared tag %v", key, tg)
			}
		}
		responses := asMap(t, key+" responses", op["responses"])
		if len(responses) == 0 {
			t.Errorf("%s has no responses", key)
		}
		var success bool
		for code := range responses {
			if strings.HasPrefix(code, "2") {
				success = true
			}
		}
		if !success {
			t.Errorf("%s declares no 2xx response", key)
		}
		for _, req := range asList(t, key+" security", op["security"]) {
			for name := range asMap(t, key+" security", req) {
				if _, ok := schemes[name]; !ok {
					t.Errorf("%s uses undefined security scheme %q", key, name)
				}
			}
		}
		// Every {param} of the path is declared as a path parameter.
		_, path, _ := strings.Cut(key, " ")
		declared := map[string]bool{}
		if params, ok := op["parameters"].([]any); ok {
			for _, p := range params {
				pm := asMap(t, key+" parameter", p)
				if r, ok := pm["$ref"].(string); ok {
					pm = asMap(t, r, resolveRef(doc, r))
				}
				if pm["in"] == "path" {
					declared[pm["name"].(string)] = true
					if pm["required"] != true {
						t.Errorf("%s: path parameter %v is not required", key, pm["name"])
					}
				}
			}
		}
		for _, m := range paramRe.FindAllStringSubmatch(path, -1) {
			if !declared[m[1]] {
				t.Errorf("%s: path parameter {%s} is not declared", key, m[1])
			}
		}
		if len(declared) != len(paramRe.FindAllString(path, -1)) {
			t.Errorf("%s declares path parameters %v that the path does not have", key, declared)
		}
	}
}

// resolveRef returns the node a local $ref ("#/a/b/c") names, or nil.
func resolveRef(doc map[string]any, ref string) any {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil
	}
	var cur any = doc
	for _, part := range strings.Split(rest, "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func refResolves(doc map[string]any, ref string) bool { return resolveRef(doc, ref) != nil }

func TestOpenAPIServedToReaders(t *testing.T) {
	f := newScopeFixture(t)
	if rec := serve(f.h, http.MethodGet, openAPIPath, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET %s without credentials = %d, want 401 (the public surface is pinned)", openAPIPath, rec.Code)
	}
	rec := serve(f.h, http.MethodGet, openAPIPath, nil, map[string]string{"Authorization": "Bearer " + f.keys[auth.ScopeRead]})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", openAPIPath, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), openAPIDocument) {
		t.Error("the served document differs from the embedded one")
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the served document is not JSON: %v", err)
	}
	if doc["openapi"] == nil || doc["paths"] == nil {
		t.Error("the served document has no openapi or paths")
	}
}
