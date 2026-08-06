//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// These tests mirror the README curl examples against a live container.

func TestHealthz(t *testing.T) {
	resp := doRequest(t, http.MethodGet, "/healthz", "", nil)
	mustStatus(t, resp, http.StatusOK)
	containsJSON(t, resp.Body, `"status":"OK"`)
}

func TestDefaultResponses(t *testing.T) {
	resetServer(t)

	// Unconfigured GET echoes the request URI as text/plain.
	resp := doRequest(t, http.MethodGet, "/anything/here", "", nil)
	mustStatus(t, resp, http.StatusOK)
	if got := strings.TrimSpace(resp.Body); got != "/anything/here" {
		t.Fatalf("default GET body = %q", got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}

	// Unconfigured POST returns a Created JSON envelope.
	resp = doRequest(t, http.MethodPost, "/anything/here", "application/json", []byte(`{"hello":"world"}`))
	mustStatus(t, resp, http.StatusCreated)
	containsJSON(t, resp.Body, `"status": "Created"`)
}

/*
Example: configure a templated stub response (README "Configure stub responses").

	curl -X POST 'http://localhost:9095/users?DO=setup&DOTIME=10' \
	  -H 'Content-Type: application/json' \
	  -d '{
	    "method": "GET",
	    "response": {
	      "status": 200,
	      "headers": {"Content-Type": "application/json"},
	      "body": {
	        "name": "{{.Q.name}}",
	        "age": "{{add (parseInt .Q.age) 10}}",
	        "auth": "{{.H.Authorization}}",
	        "userId": "{{index .Q `user-id`}}",
	        "servedAt": "{{formatTime `2006-01-02T15:04:05Z07:00` .Now}}"
	      }
	    }
	  }'

	curl 'http://localhost:9095/users?name=Sam&age=30&user-id=u-42'
*/
func TestStubTemplatedJSONResponse(t *testing.T) {
	resetServer(t)

	setup := doJSON(t, http.MethodPost, "/users?DO=setup&DOTIME=10", map[string]any{
		"method": "GET",
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "application/json",
			},
			"body": map[string]any{
				"name":     "{{.Q.name}}",
				"age":      "{{add (parseInt .Q.age) 10}}",
				"auth":     "{{.H.Authorization}}",
				"userId":   "{{index .Q `user-id`}}",
				"servedAt": "{{formatTime `2006-01-02T15:04:05Z07:00` .Now}}",
			},
		},
	})
	mustStatus(t, setup, http.StatusCreated)
	containsJSON(t, setup.Body, `"status":"Setup"`)

	req, err := http.NewRequest(http.MethodGet, baseURL+"/users?name=Sam&age=30&user-id=u-42", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	raw, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Body.Close()
	data, err := io.ReadAll(raw.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if raw.StatusCode != http.StatusOK {
		t.Fatalf("configured GET status = %d, body = %s", raw.StatusCode, body)
	}

	var result struct {
		Name     string  `json:"name"`
		Age      float64 `json:"age"`
		Auth     string  `json:"auth"`
		UserID   string  `json:"userId"`
		ServedAt string  `json:"servedAt"`
	}
	decodeJSON(t, body, &result)
	if result.Name != "Sam" || result.Age != 40 || result.Auth != "Bearer test-token" ||
		result.UserID != "u-42" || result.ServedAt == "" {
		t.Fatalf("unexpected rendered body: %+v", result)
	}
}

/*
Example: templated redirect Location header.

	curl -X POST 'http://localhost:9095/authorize?DO=setup' \
	  -H 'Content-Type: application/json' \
	  -d '{
	    "method": "GET",
	    "response": {
	      "status": 302,
	      "headers": {
	        "Location": "{{.Q.redirect_uri}}?code=test-code&state={{.Q.state}}"
	      }
	    }
	  }'
*/
func TestStubTemplatedRedirect(t *testing.T) {
	resetServer(t)

	setup := doJSON(t, http.MethodPost, "/authorize?DO=setup", map[string]any{
		"method": "GET",
		"response": map[string]any{
			"status": 302,
			"headers": map[string]string{
				"Location": "{{.Q.redirect_uri}}?code=test-code&state={{.Q.state}}",
			},
		},
	})
	mustStatus(t, setup, http.StatusCreated)

	resp := doRequest(t, http.MethodGet,
		"/authorize?redirect_uri="+url.QueryEscape("http://localhost:3000/callback")+
			"&state=xyz",
		"", nil)
	mustStatus(t, resp, http.StatusFound)
	location := resp.Header.Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != "http://localhost:3000/callback" {
		t.Fatalf("Location path = %q", location)
	}
	if parsed.Query().Get("code") != "test-code" || parsed.Query().Get("state") != "xyz" {
		t.Fatalf("Location query = %s", location)
	}
}

/*
Example: hit limits and resets.

	curl -X POST 'http://localhost:9095/limited?DO=setup&DOTIME=1' ...
	curl -X POST 'http://localhost:9095/limited?DO=reset' -d '{"method":"GET"}'
	curl -X POST 'http://localhost:9095/RESET'
*/
func TestStubHitLimitAndReset(t *testing.T) {
	resetServer(t)

	setup := doJSON(t, http.MethodPost, "/limited?DO=setup&DOTIME=1", map[string]any{
		"method": "GET",
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "text/plain",
			},
			"body": "only-once",
		},
	})
	mustStatus(t, setup, http.StatusCreated)

	first := doRequest(t, http.MethodGet, "/limited", "", nil)
	mustStatus(t, first, http.StatusOK)
	if strings.TrimSpace(first.Body) != "only-once" {
		t.Fatalf("first hit body = %q", first.Body)
	}

	// After the hit limit, the default echo behavior returns.
	second := doRequest(t, http.MethodGet, "/limited", "", nil)
	mustStatus(t, second, http.StatusOK)
	if strings.TrimSpace(second.Body) != "/limited" {
		t.Fatalf("second hit body = %q", second.Body)
	}

	// Re-setup, then reset one method.
	mustStatus(t, doJSON(t, http.MethodPost, "/limited?DO=setup", map[string]any{
		"method": "GET",
		"response": map[string]any{
			"status": 200,
			"body":   "again",
		},
	}), http.StatusCreated)
	mustStatus(t, doJSON(t, http.MethodPost, "/limited?DO=reset", map[string]any{
		"method": "GET",
	}), http.StatusOK)
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/limited", "", nil).Body); got != "/limited" {
		t.Fatalf("after method reset body = %q", got)
	}

	mustStatus(t, doJSON(t, http.MethodPost, "/one?DO=setup", map[string]any{
		"method":   "GET",
		"response": map[string]any{"status": 200, "body": "one"},
	}), http.StatusCreated)
	mustStatus(t, doRequest(t, http.MethodPost, "/RESET", "", nil), http.StatusOK)
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/one", "", nil).Body); got != "/one" {
		t.Fatalf("after global reset body = %q", got)
	}
}
