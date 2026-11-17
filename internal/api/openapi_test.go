package api

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const openAPIPath = "../../docs/openapi.yaml"

// TestOpenAPIDescribesThePublicRoutes keeps docs/openapi.yaml honest: it must
// parse, every reference must resolve, and its operations must be exactly the
// routes the public handler serves.
func TestOpenAPIDescribesThePublicRoutes(t *testing.T) {
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("docs/openapi.yaml does not parse: %v", err)
	}

	for _, ref := range regexp.MustCompile(`\$ref: "#/([^"]+)"`).FindAllStringSubmatch(string(raw), -1) {
		var node any = doc
		for _, part := range strings.Split(ref[1], "/") {
			m, ok := node.(map[string]any)
			if !ok || m[part] == nil {
				t.Errorf("unresolved reference #/%s", ref[1])
				break
			}
			node = m[part]
		}
	}

	var documented []string
	for path, item := range doc["paths"].(map[string]any) {
		for method := range item.(map[string]any) {
			if method != "parameters" {
				documented = append(documented, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(documented)
	want := []string{
		"DELETE /v1/sessions/{id}",
		"GET /v1/health",
		"GET /v1/sessions",
		"GET /v1/sessions/{id}",
		"GET /v1/sessions/{id}/cdp",
		"POST /v1/sessions",
	}
	if strings.Join(documented, "\n") != strings.Join(want, "\n") {
		t.Fatalf("documented operations:\n%s\nwant:\n%s", strings.Join(documented, "\n"), strings.Join(want, "\n"))
	}

	// Each documented operation is a real route (anything else is a 404),
	// and nothing undocumented is served under /v1.
	h := newHarness(t, nil)
	for _, op := range documented {
		method, path, _ := strings.Cut(op, " ")
		path = strings.ReplaceAll(path, "{id}", "sess_unknown")
		resp := h.api(method, path, `{"ttlSeconds": 0}`)
		if resp.status == http.StatusNotFound && resp.errorCode(t) == "not_found" {
			t.Errorf("%s is documented but not served", op)
		}
	}
	for _, op := range []string{"PUT /v1/sessions", "PATCH /v1/sessions/sess_unknown", "POST /v1/health", "GET /v1"} {
		method, path, _ := strings.Cut(op, " ")
		if resp := h.api(method, path, ""); resp.status != http.StatusNotFound && resp.status != http.StatusMethodNotAllowed {
			t.Errorf("%s is served (status %d) but not documented", op, resp.status)
		}
	}
}
