//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
Example: OpenAPI bootstrap from apis/openapi.yaml (README "OpenAPI bootstrap").

The e2e container mounts repo apis/ as APIDIR=/apis. After reload, GET / and
GET /v2 are served from the imported examples.
*/
func TestOpenAPIBootstrapFromApisDir(t *testing.T) {
	reloadServer(t)

	listed := doRequest(t, http.MethodPost, "/?DO=list", "", nil)
	mustStatus(t, listed, http.StatusOK)
	containsJSON(t, listed.Body, `"status":"List"`)
	containsJSON(t, listed.Body, `"count":2`)
	containsJSON(t, listed.Body, `"path":"/"`)
	containsJSON(t, listed.Body, `"path":"/v2"`)
	containsJSON(t, listed.Body, `"method":"GET"`)
	containsJSON(t, listed.Body, `"state":"active"`)

	root := doRequest(t, http.MethodGet, "/", "", nil)
	mustStatus(t, root, http.StatusOK)
	if ct := root.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("GET / Content-Type = %q", ct)
	}
	containsJSON(t, root.Body, `"id":"v2.0"`)
	containsJSON(t, root.Body, `"status":"CURRENT"`)
	containsJSON(t, root.Body, `"versions"`)

	version := doRequest(t, http.MethodGet, "/v2", "", nil)
	mustStatus(t, version, http.StatusOK)
	containsJSON(t, version.Body, `"media-types"`)
	containsJSON(t, version.Body, `"id":"v2.0"`)

	// Manual setup still overrides an OpenAPI-imported route.
	mustStatus(t, doJSON(t, http.MethodPost, "/v2?DO=setup", map[string]any{
		"method": "GET",
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "application/json",
			},
			"body": map[string]any{"overridden": true},
		},
	}), http.StatusCreated)
	overridden := doRequest(t, http.MethodGet, "/v2", "", nil)
	mustStatus(t, overridden, http.StatusOK)
	containsJSON(t, overridden.Body, `"overridden":true`)

	var payload struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(listed.Body), &payload); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if payload.Count != 2 {
		t.Fatalf("list count = %d", payload.Count)
	}
}
