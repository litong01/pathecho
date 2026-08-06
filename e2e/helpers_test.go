//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type httpResponse struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func doRequest(t *testing.T, method, path, contentType string, body []byte) httpResponse {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return httpResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       string(data),
	}
}

func doJSON(t *testing.T, method, path string, payload any) httpResponse {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return doRequest(t, method, path, "application/json", data)
}

func doForm(t *testing.T, path string, values url.Values) httpResponse {
	t.Helper()
	return doRequest(t, http.MethodPost, path, "application/x-www-form-urlencoded", []byte(values.Encode()))
}

func mustStatus(t *testing.T, resp httpResponse, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, want, resp.Body)
	}
}

func decodeJSON(t *testing.T, body string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), target); err != nil {
		t.Fatalf("decode JSON: %v\nbody: %s", err, body)
	}
}

func containsJSON(t *testing.T, body, fragment string) {
	t.Helper()
	if !strings.Contains(body, fragment) {
		t.Fatalf("body missing %q:\n%s", fragment, body)
	}
}
