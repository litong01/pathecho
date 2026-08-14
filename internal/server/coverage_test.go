package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzVersionPutDeleteAndBodyLogging(t *testing.T) {
	router := NewRouter()

	response := performRequest(router, http.MethodGet, "/healthz", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"OK"`) {
		t.Fatalf("healthz = %d %s", response.Code, response.Body.String())
	}

	t.Setenv("version", "")
	response = performRequest(NewRouter(), http.MethodGet, "/version", "")
	if response.Code != http.StatusOK || response.Body.String() != `{"status":"FAIL"}` {
		t.Fatalf("empty version = %d %q", response.Code, response.Body.String())
	}

	upstreamOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1"}`))
	}))
	defer upstreamOK.Close()
	t.Setenv("version", upstreamOK.URL)
	response = performRequest(NewRouter(), http.MethodGet, "/version", "")
	if response.Code != http.StatusOK || response.Body.String() != `{"version":"1"}` {
		t.Fatalf("version proxy success = %d %q", response.Code, response.Body.String())
	}

	upstreamBadStatus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstreamBadStatus.Close()
	t.Setenv("version", upstreamBadStatus.URL)
	response = performRequest(NewRouter(), http.MethodGet, "/version", "")
	if response.Body.String() != `{"status":"FAIL"}` {
		t.Fatalf("version bad status = %q", response.Body.String())
	}

	t.Setenv("version", "http://127.0.0.1:1")
	response = performRequest(NewRouter(), http.MethodGet, "/version", "")
	if response.Body.String() != `{"status":"FAIL"}` {
		t.Fatalf("version dial fail = %q", response.Body.String())
	}

	response = performRequest(router, http.MethodPut, "/resource", `{"name":"updated"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"Updated"`) {
		t.Fatalf("default PUT = %d %s", response.Code, response.Body.String())
	}

	response = performRequest(router, http.MethodPatch, "/resource", `{"name":"patched"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"Updated"`) {
		t.Fatalf("default PATCH = %d %s", response.Code, response.Body.String())
	}

	response = performJSONRequest(t, router, http.MethodPost, "/resource?DO=setup", map[string]any{
		"method": http.MethodPut,
		"response": map[string]any{
			"status": 202,
			"body":   "configured-put",
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("PUT setup = %d %s", response.Code, response.Body.String())
	}
	response = performRequest(router, http.MethodPut, "/resource", "")
	if response.Code != 202 || response.Body.String() != "configured-put" {
		t.Fatalf("configured PUT = %d %q", response.Code, response.Body.String())
	}

	response = performJSONRequest(t, router, http.MethodPost, "/resource?DO=setup", map[string]any{
		"method": http.MethodPatch,
		"response": map[string]any{
			"status": 200,
			"body":   "configured-patch",
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("PATCH setup = %d %s", response.Code, response.Body.String())
	}
	response = performRequest(router, http.MethodPatch, "/resource", "")
	if response.Code != http.StatusOK || response.Body.String() != "configured-patch" {
		t.Fatalf("configured PATCH = %d %q", response.Code, response.Body.String())
	}

	response = performJSONRequest(t, router, http.MethodPost, "/gone?DO=setup", map[string]any{
		"method": http.MethodDelete,
		"response": map[string]any{
			"status": 200,
			"body":   "deleted",
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("DELETE setup = %d", response.Code)
	}
	response = performRequest(router, http.MethodDelete, "/gone", "")
	if response.Code != 200 || response.Body.String() != "deleted" {
		t.Fatalf("configured DELETE = %d %q", response.Code, response.Body.String())
	}

	large := strings.Repeat("x", maxLoggedBodySize+8)
	response = performRequest(router, http.MethodPost, "/large", large)
	if response.Code != http.StatusCreated {
		t.Fatalf("large body = %d", response.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/json-ct", strings.NewReader(`{"ok":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("json content-type = %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/json-bad", strings.NewReader(`{`))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("invalid json body still created = %d", recorder.Code)
	}

	response = performRequest(router, http.MethodDelete, "/missing-default", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("default DELETE = %d", response.Code)
	}

	response = performRequest(router, http.MethodGet, "/oauth/not-a-real-endpoint", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("oauth endpoint not found = %d %s", response.Code, response.Body.String())
	}
}
