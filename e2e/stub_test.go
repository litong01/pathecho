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
Example: echo JSON request body fields with jsonPath.

	curl -X POST 'http://localhost:9095/users?DO=setup' ...
	curl -X POST 'http://localhost:9095/users' -d '{"user":{"name":"Sam","age":30}}'
*/
func TestStubJSONPathRequestBody(t *testing.T) {
	resetServer(t)

	setup := doJSON(t, http.MethodPost, "/users?DO=setup", map[string]any{
		"method": "POST",
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "application/json",
			},
			"body": map[string]any{
				"name": `{{jsonPath "$.user.name" .J}}`,
				"age":  `{{jsonPath "$.user.age" .J}}`,
				"raw":  "{{jsonString .Body}}",
			},
		},
	})
	mustStatus(t, setup, http.StatusCreated)

	resp := doRequest(t, http.MethodPost, "/users", "application/json",
		[]byte(`{"user":{"name":"Sam","age":30}}`))
	mustStatus(t, resp, http.StatusOK)

	var result struct {
		Name string  `json:"name"`
		Age  float64 `json:"age"`
		Raw  string  `json:"raw"`
	}
	decodeJSON(t, resp.Body, &result)
	if result.Name != "Sam" || result.Age != 30 || result.Raw != `{"user":{"name":"Sam","age":30}}` {
		t.Fatalf("unexpected rendered body: %+v", result)
	}
}

/*
Example: a bare {{.Body}} in a JSON object body is re-parsed as JSON, so a
JSON request payload is embedded as an object rather than a string.
*/
func TestStubRawBodyEmbeddedAsJSON(t *testing.T) {
	resetServer(t)

	setup := doJSON(t, http.MethodPost, "/echo?DO=setup", map[string]any{
		"method": "POST",
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "application/json",
			},
			"body": map[string]any{
				"received": "{{.Body}}",
			},
		},
	})
	mustStatus(t, setup, http.StatusCreated)

	resp := doRequest(t, http.MethodPost, "/echo", "application/json",
		[]byte(`{"user":{"name":"Sam","age":30}}`))
	mustStatus(t, resp, http.StatusOK)

	var result struct {
		Received struct {
			User struct {
				Name string  `json:"name"`
				Age  float64 `json:"age"`
			} `json:"user"`
		} `json:"received"`
	}
	decodeJSON(t, resp.Body, &result)
	if result.Received.User.Name != "Sam" || result.Received.User.Age != 30 {
		t.Fatalf("unexpected rendered body: %+v", result)
	}
}

/*
Example: a non-JSON request body is available as raw text through .Body.
*/
func TestStubNonJSONRequestBody(t *testing.T) {
	resetServer(t)

	setup := doJSON(t, http.MethodPost, "/plain?DO=setup", map[string]any{
		"method": "POST",
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "text/plain",
			},
			"body": "got={{.Body}}",
		},
	})
	mustStatus(t, setup, http.StatusCreated)

	resp := doRequest(t, http.MethodPost, "/plain", "text/plain", []byte("hello there"))
	mustStatus(t, resp, http.StatusOK)
	if strings.TrimSpace(resp.Body) != "got=hello there" {
		t.Fatalf("plain body = %q", resp.Body)
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

/*
Example: named setups and deferred setups (README "Named and deferred setups").

	# Saved, not active: same method and path as the active setup below.
	curl -X POST 'http://localhost:9095/status?DO=setup' \
	  -H 'Content-Type: application/json' \
	  -d '{"method":"GET","name":"status-done","response":{"body":{"state":"done"}}}'

	# Active now.
	curl -X POST 'http://localhost:9095/status?DO=setup' \
	  -H 'Content-Type: application/json' \
	  -d '{"method":"GET","response":{"body":{"state":"pending"}}}'

	# Serving POST /job swaps in the saved "status-done" setup.
	curl -X POST 'http://localhost:9095/job?DO=setup' \
	  -H 'Content-Type: application/json' \
	  -d '{"method":"POST","response":{"status":202,"body":{"state":"queued"}},"then":["status-done"]}'
*/
func TestStubNamedSetupDeferredActivation(t *testing.T) {
	resetServer(t)

	saved := doJSON(t, http.MethodPost, "/status?DO=setup", map[string]any{
		"method": "GET",
		"name":   "status-done",
		"response": map[string]any{
			"status":  200,
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    map[string]any{"state": "done"},
		},
	})
	mustStatus(t, saved, http.StatusCreated)
	containsJSON(t, saved.Body, `"status":"Saved"`)
	containsJSON(t, saved.Body, `"name":"status-done"`)

	// A saved setup must not activate on its own.
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/status", "", nil).Body); got != "/status" {
		t.Fatalf("saved setup activated too early: body = %q", got)
	}

	// The active setup shares the same method and path as the saved one.
	active := doJSON(t, http.MethodPost, "/status?DO=setup", map[string]any{
		"method": "GET",
		"response": map[string]any{
			"status":  200,
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    map[string]any{"state": "pending"},
		},
	})
	mustStatus(t, active, http.StatusCreated)
	containsJSON(t, active.Body, `"status":"Setup"`)

	trigger := doJSON(t, http.MethodPost, "/job?DO=setup", map[string]any{
		"method": "POST",
		"response": map[string]any{
			"status":  202,
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    map[string]any{"state": "queued"},
		},
		"then": []string{"status-done"},
	})
	mustStatus(t, trigger, http.StatusCreated)
	containsJSON(t, trigger.Body, `"then":["status-done"]`)

	before := doRequest(t, http.MethodGet, "/status", "", nil)
	mustStatus(t, before, http.StatusOK)
	containsJSON(t, before.Body, `"state":"pending"`)

	// Dependency traffic between the setup and the trigger changes nothing.
	for _, path := range []string{"/dependency/one", "/dependency/two"} {
		mustStatus(t, doRequest(t, http.MethodGet, path, "", nil), http.StatusOK)
	}
	containsJSON(t, doRequest(t, http.MethodGet, "/status", "", nil).Body, `"state":"pending"`)

	fired := doRequest(t, http.MethodPost, "/job", "application/json", []byte(`{}`))
	mustStatus(t, fired, http.StatusAccepted)
	containsJSON(t, fired.Body, `"state":"queued"`)

	after := doRequest(t, http.MethodGet, "/status", "", nil)
	mustStatus(t, after, http.StatusOK)
	containsJSON(t, after.Body, `"state":"done"`)
}

/*
Example: chained named setups and DONAME, one step per served request.

	curl -X POST 'http://localhost:9095/step2?DO=setup&DONAME=step2' \
	  -H 'Content-Type: application/json' \
	  -d '{"method":"GET","response":{"body":"step2"},"then":["step3"]}'

	curl -X POST 'http://localhost:9095/step3?DO=setup&DONAME=step3' \
	  -H 'Content-Type: application/json' \
	  -d '{"method":"GET","response":{"body":"step3"}}'

	curl -X POST 'http://localhost:9095/step1?DO=setup' \
	  -H 'Content-Type: application/json' \
	  -d '{"method":"GET","response":{"body":"step1"},"then":["step2"]}'
*/
func TestStubNamedSetupChainAndDefinitionReset(t *testing.T) {
	resetServer(t)

	// step3 is named after the setup that references it, proving names are
	// resolved when the triggering response is served.
	mustStatus(t, doJSON(t, http.MethodPost, "/step2?DO=setup&DONAME=step2", map[string]any{
		"method":   "GET",
		"response": map[string]any{"status": 200, "body": "step2"},
		"then":     []string{"step3"},
	}), http.StatusCreated)
	mustStatus(t, doJSON(t, http.MethodPost, "/step3?DO=setup&DONAME=step3", map[string]any{
		"method":   "GET",
		"response": map[string]any{"status": 200, "body": "step3"},
	}), http.StatusCreated)
	mustStatus(t, doJSON(t, http.MethodPost, "/step1?DO=setup", map[string]any{
		"method":   "GET",
		"response": map[string]any{"status": 200, "body": "step1"},
		"then":     []string{"step2"},
	}), http.StatusCreated)

	// Only step1 is active; the chain advances one served request at a time.
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/step2", "", nil).Body); got != "/step2" {
		t.Fatalf("step2 active before step1 was served: body = %q", got)
	}
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/step1", "", nil).Body); got != "step1" {
		t.Fatalf("step1 body = %q", got)
	}
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/step3", "", nil).Body); got != "/step3" {
		t.Fatalf("step3 active before step2 was served: body = %q", got)
	}
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/step2", "", nil).Body); got != "step2" {
		t.Fatalf("step2 was not activated by step1: body = %q", got)
	}
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/step3", "", nil).Body); got != "step3" {
		t.Fatalf("step3 was not activated by step2: body = %q", got)
	}

	// A path reset also drops the named setups saved on that path.
	mustStatus(t, doJSON(t, http.MethodPost, "/saved?DO=setup&DONAME=saved-response", map[string]any{
		"method":   "GET",
		"response": map[string]any{"status": 200, "body": "saved"},
	}), http.StatusCreated)
	reset := doJSON(t, http.MethodPost, "/saved?DO=reset", map[string]any{"method": "GET"})
	mustStatus(t, reset, http.StatusOK)
	containsJSON(t, reset.Body, `"removedDefinitions":1`)

	// The name is gone, so a trigger referencing it activates nothing.
	mustStatus(t, doJSON(t, http.MethodPost, "/fire?DO=setup", map[string]any{
		"method":   "POST",
		"response": map[string]any{"status": 200, "body": "fired"},
		"then":     []string{"saved-response"},
	}), http.StatusCreated)
	mustStatus(t, doRequest(t, http.MethodPost, "/fire", "application/json", []byte(`{}`)), http.StatusOK)
	if got := strings.TrimSpace(doRequest(t, http.MethodGet, "/saved", "", nil).Body); got != "/saved" {
		t.Fatalf("reset name was still activated: body = %q", got)
	}

	// The global reset reports saved setups it discarded. Start from a clean
	// server so the count covers only the setup saved below.
	resetServer(t)
	mustStatus(t, doJSON(t, http.MethodPost, "/kept?DO=setup&DONAME=kept-response", map[string]any{
		"method":   "GET",
		"response": map[string]any{"status": 200, "body": "kept"},
	}), http.StatusCreated)
	globalReset := doRequest(t, http.MethodPost, "/RESET", "", nil)
	mustStatus(t, globalReset, http.StatusOK)
	containsJSON(t, globalReset.Body, `"removedDefinitions":1`)
}

/*
Example: list configured setups (README "List configured setups").

	curl -X POST 'http://localhost:9095/?DO=list'
*/
func TestListConfiguredSetups(t *testing.T) {
	resetServer(t)

	mustStatus(t, doJSON(t, http.MethodPost, "/users?DO=setup&DOTIME=5", map[string]any{
		"method": "GET",
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "application/json",
			},
			"body": map[string]any{"name": "{{.Q.name}}"},
		},
	}), http.StatusCreated)
	mustStatus(t, doJSON(t, http.MethodPost, "/status?DO=setup&DONAME=status-done", map[string]any{
		"method":   "GET",
		"response": map[string]any{"status": 200, "body": "done"},
	}), http.StatusCreated)

	listed := doRequest(t, http.MethodPost, "/?DO=list", "", nil)
	mustStatus(t, listed, http.StatusOK)
	containsJSON(t, listed.Body, `"status":"List"`)
	containsJSON(t, listed.Body, `"count":2`)
	containsJSON(t, listed.Body, `"state":"active"`)
	containsJSON(t, listed.Body, `"state":"saved"`)
	containsJSON(t, listed.Body, `"name":"status-done"`)
	containsJSON(t, listed.Body, `"path":"/users"`)
}
