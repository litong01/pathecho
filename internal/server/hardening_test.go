package server

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pathecho/internal/httpapi"
)

func TestDefaultRequestBodyLimitsAndCompression(t *testing.T) {
	router := NewRouter()

	request := httptest.NewRequest(http.MethodPost, "/plain", strings.NewReader(`{"ok":true}`))
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("plain request with Accept-Encoding status = %d, body = %s", response.Code, response.Body.String())
	}

	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(`{"compressed":true}`))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/compressed", bytes.NewReader(compressed.Bytes()))
	request.Header.Set("Content-Encoding", "gzip")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("gzip request status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"/too-large",
		strings.NewReader(strings.Repeat("x", httpapi.MaxBodySize+1)),
	)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status = %d, body = %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/encoded", strings.NewReader("body"))
	request.Header.Set("Content-Encoding", "br")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported encoding status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestBuiltInRoutesUseExactPaths(t *testing.T) {
	router := NewRouter()

	response := performRequest(router, http.MethodGet, "/healthz/extra", "")
	if response.Code != http.StatusOK || response.Body.String() != "/healthz/extra" {
		t.Fatalf("health prefix status = %d, body = %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("fallback Content-Type = %q", got)
	}

	response = performRequest(router, http.MethodGet, "/version/extra", "")
	if response.Code != http.StatusOK || response.Body.String() != "/version/extra" {
		t.Fatalf("version prefix status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestVersionProxyBoundsUpstreamResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", httpapi.MaxBodySize+1)))
	}))
	defer upstream.Close()
	t.Setenv("version", upstream.URL)

	response := performRequest(NewRouter(), http.MethodGet, "/version", "")
	if response.Code != http.StatusOK || response.Body.String() != `{"status":"FAIL"}` {
		t.Fatalf("bounded version response status = %d, body = %q", response.Code, response.Body.String())
	}
}
