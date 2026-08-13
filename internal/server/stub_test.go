package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfiguredResponseRendersRequestDataAndFunctions(t *testing.T) {
	router := NewRouter()
	setup := map[string]any{
		"method": "get",
		"response": map[string]any{
			"status": 202,
			"headers": map[string]string{
				"Content-Type": "application/json",
				"X-Stub":       "yes",
			},
			"body": map[string]any{
				"name":   "{{.Q.name}}",
				"age":    "{{add (parseInt .Q.age) 10}}",
				"method": "{{.Method}}",
				"path":   "{{.Path}}",
				"date":   "{{formatTime `2006-01-02` .Now}}",
				"header": "{{index .H `X-Test`}}",
				"id":     "{{index .Q `user-id`}}",
			},
		},
	}

	response := performJSONRequest(t, router, http.MethodPost, "/abcd/efgh?do=SETUP", setup)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", response.Code, response.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/abcd/efgh?name=A%22B&age=30&user-id=user-42&runtime=value", nil)
	request.Header.Set("X-Test", "request-header")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != 202 {
		t.Fatalf("configured status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Stub"); got != "yes" {
		t.Fatalf("X-Stub = %q", got)
	}
	var result struct {
		Name   string  `json:"name"`
		Age    float64 `json:"age"`
		Method string  `json:"method"`
		Path   string  `json:"path"`
		Date   string  `json:"date"`
		Header string  `json:"header"`
		ID     string  `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid rendered JSON: %v\n%s", err, response.Body.String())
	}
	if result.Name != `A"B` || result.Age != 40 || result.Method != http.MethodGet ||
		result.Path != "/abcd/efgh" || len(result.Date) != len("2006-01-02") ||
		result.Header != "request-header" || result.ID != "user-42" {
		t.Fatalf("unexpected rendered response: %+v", result)
	}
}

func TestConfiguredHeadersRenderRequestData(t *testing.T) {
	router := NewRouter()
	response := performJSONRequest(t, router, http.MethodPost, "/authorize?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"status": http.StatusFound,
			"headers": map[string]string{
				"Location":  "{{.Q.redirect_uri}}?code=test-code&state={{.Q.state}}",
				"X-Request": "{{index .H `X-Test`}}",
			},
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", response.Code, response.Body.String())
	}

	request := httptest.NewRequest(
		http.MethodGet,
		"/authorize?redirect_uri=https%3A%2F%2Fclient.example%2Fcallback&state=request-state",
		nil,
	)
	request.Header.Set("X-Test", "request-header")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusFound {
		t.Fatalf("redirect status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); got != "https://client.example/callback?code=test-code&state=request-state" {
		t.Fatalf("Location = %q", got)
	}
	if got := response.Header().Get("X-Request"); got != "request-header" {
		t.Fatalf("X-Request = %q", got)
	}
}

func TestConfiguredResponsesAreMethodSpecific(t *testing.T) {
	router := NewRouter()
	performSetup(t, router, "/same", http.MethodPost, "configured post", nil)

	getResponse := performRequest(router, http.MethodGet, "/same?ignored=yes", "")
	if getResponse.Code != http.StatusOK || getResponse.Body.String() != "/same?ignored=yes" {
		t.Fatalf("GET fallback = %d %q", getResponse.Code, getResponse.Body.String())
	}

	postResponse := performRequest(router, http.MethodPost, "/same?runtime=yes", "actual body")
	if postResponse.Code != http.StatusOK || postResponse.Body.String() != "configured post" {
		t.Fatalf("POST configured response = %d %q", postResponse.Code, postResponse.Body.String())
	}
}

func TestDOTIMEExpiresResponseAfterConfiguredHits(t *testing.T) {
	router := NewRouter()
	setup := map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"body": "limited",
		},
	}
	response := performJSONRequest(t, router, http.MethodPost, "/limited?DO=setup&DOTIME=2", setup)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", response.Code, response.Body.String())
	}

	for hit := 1; hit <= 2; hit++ {
		response = performRequest(router, http.MethodGet, "/limited?request=varies", "")
		if response.Body.String() != "limited" {
			t.Fatalf("hit %d body = %q", hit, response.Body.String())
		}
	}
	response = performRequest(router, http.MethodGet, "/limited?request=third", "")
	if response.Body.String() != "/limited?request=third" {
		t.Fatalf("expired response did not fall back: %q", response.Body.String())
	}
}

func TestJSONBodyTemplateAndTimesField(t *testing.T) {
	router := NewRouter()
	response := performJSONRequest(t, router, http.MethodPost, "/object?DO=setup", map[string]any{
		"method": http.MethodGet,
		"times":  1,
		"response": map[string]any{
			"headers": map[string]string{"Content-Type": "application/json"},
			"body": map[string]any{
				"value": `{{upper .Q.value}}`,
			},
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", response.Code, response.Body.String())
	}

	response = performRequest(router, http.MethodGet, "/object?value=hello", "")
	if response.Code != http.StatusOK || response.Body.String() != `{"value":"HELLO"}` {
		t.Fatalf("configured JSON response = %d %s", response.Code, response.Body.String())
	}
	if got := performRequest(router, http.MethodGet, "/object?value=again", "").Body.String(); got != "/object?value=again" {
		t.Fatalf("times field did not expire response: %q", got)
	}
}

func TestMethodPathAndGlobalReset(t *testing.T) {
	router := NewRouter()
	performSetup(t, router, "/reset-me", http.MethodGet, "get response", nil)
	performSetup(t, router, "/reset-me", http.MethodPost, "post response", nil)

	response := performJSONRequest(t, router, http.MethodPost, "/reset-me?DO=reset", map[string]any{
		"method": http.MethodGet,
	})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"removed":1`) {
		t.Fatalf("method reset = %d %s", response.Code, response.Body.String())
	}
	if got := performRequest(router, http.MethodGet, "/reset-me", "").Body.String(); got != "/reset-me" {
		t.Fatalf("GET was not reset: %q", got)
	}
	if got := performRequest(router, http.MethodPost, "/reset-me", "").Body.String(); got != "post response" {
		t.Fatalf("POST should remain configured: %q", got)
	}

	performSetup(t, router, "/reset-me", http.MethodGet, "get response", nil)
	response = performRequest(router, http.MethodPost, "/reset-me?DO=reset", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"removed":2`) {
		t.Fatalf("path reset = %d %s", response.Code, response.Body.String())
	}

	performSetup(t, router, "/one", http.MethodGet, "one", nil)
	performSetup(t, router, "/two", http.MethodDelete, "two", nil)
	response = performRequest(router, http.MethodPost, "/RESET", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"removed":2`) {
		t.Fatalf("global reset = %d %s", response.Code, response.Body.String())
	}
	if got := performRequest(router, http.MethodGet, "/one", "").Body.String(); got != "/one" {
		t.Fatalf("global reset left GET setup: %q", got)
	}
	if code := performRequest(router, http.MethodDelete, "/two", "").Code; code != http.StatusNoContent {
		t.Fatalf("global reset left DELETE setup, status = %d", code)
	}
}

func TestListConfiguredSetups(t *testing.T) {
	router := NewRouter()
	response := performJSONRequest(t, router, http.MethodPost, "/listed?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "text/plain",
			},
			"body": "hello",
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("active setup = %d %s", response.Code, response.Body.String())
	}
	response = performJSONRequest(t, router, http.MethodPost, "/listed?DO=setup&DONAME=later", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"status": 200,
			"body":   "later",
		},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("named setup = %d %s", response.Code, response.Body.String())
	}

	response = performRequest(router, http.MethodPost, "/anywhere?DO=list", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"status":"List"`) {
		t.Fatalf("list body = %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"state":"active"`) ||
		!strings.Contains(response.Body.String(), `"state":"saved"`) ||
		!strings.Contains(response.Body.String(), `"name":"later"`) ||
		!strings.Contains(response.Body.String(), `"body":"hello"`) {
		t.Fatalf("list missing expected setups: %s", response.Body.String())
	}

	var payload struct {
		Count  int `json:"count"`
		Setups []struct {
			State string `json:"state"`
		} `json:"setups"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if payload.Count != 2 || len(payload.Setups) != 2 {
		t.Fatalf("list count = %#v", payload)
	}
}

func TestInvalidTemplatesAndMathErrorsAreReported(t *testing.T) {
	router := NewRouter()

	response := performJSONRequest(t, router, http.MethodPost, "/bad?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"body": `{{unknownFunction}}`,
		},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown function setup status = %d, body = %s", response.Code, response.Body.String())
	}

	response = performJSONRequest(t, router, http.MethodPost, "/bad-header?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"headers": map[string]string{"Location": `{{unknownFunction}}`},
		},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown header function setup status = %d, body = %s", response.Code, response.Body.String())
	}

	performSetup(t, router, "/math", http.MethodGet, `{{sqrt -1}}`, nil)
	response = performRequest(router, http.MethodGet, "/math", "")
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "non-finite") {
		t.Fatalf("invalid math response = %d %s", response.Code, response.Body.String())
	}

	once := 1
	performSetup(t, router, "/limited-math", http.MethodGet, `{{sqrt -1}}`, &once)
	for attempt := 1; attempt <= 2; attempt++ {
		response = performRequest(router, http.MethodGet, "/limited-math", "")
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("failed render attempt %d status = %d, body = %s", attempt, response.Code, response.Body.String())
		}
	}
}

// A named setup must stay inactive until a served response names it in
// "then", even though the app makes unrelated requests in between.
func TestNamedSetupIsAppliedByTriggeringRequest(t *testing.T) {
	router := NewRouter()

	response := performJSONRequest(t, router, http.MethodPost, "/status?DO=setup&DONAME=status-done", map[string]any{
		"method":   http.MethodGet,
		"response": map[string]any{"body": "done"},
	})
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"status":"Saved"`) {
		t.Fatalf("named setup = %d, body = %s", response.Code, response.Body.String())
	}

	performSetup(t, router, "/status", http.MethodGet, "pending", nil)
	response = performJSONRequest(t, router, http.MethodPost, "/job?DO=setup", map[string]any{
		"method":   http.MethodPost,
		"response": map[string]any{"body": "queued"},
		"then":     []string{"status-done"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("trigger setup = %d, body = %s", response.Code, response.Body.String())
	}

	if response = performRequest(router, http.MethodGet, "/status", ""); response.Body.String() != "pending" {
		t.Fatalf("status before trigger = %s", response.Body.String())
	}
	// An unrelated dependency request must not disturb the pending definition.
	performRequest(router, http.MethodGet, "/some/dependency", "")

	if response = performRequest(router, http.MethodPost, "/job", ""); response.Body.String() != "queued" {
		t.Fatalf("trigger serve = %s", response.Body.String())
	}
	if response = performRequest(router, http.MethodGet, "/status", ""); response.Body.String() != "done" {
		t.Fatalf("status after trigger = %s", response.Body.String())
	}
}

func performSetup(
	t *testing.T,
	router http.Handler,
	path string,
	method string,
	body string,
	times *int,
) {
	t.Helper()
	setup := map[string]any{
		"method": method,
		"response": map[string]any{
			"body": body,
		},
	}
	if times != nil {
		setup["times"] = *times
	}
	response := performJSONRequest(t, router, http.MethodPost, path+"?DO=setup", setup)
	if response.Code != http.StatusCreated {
		t.Fatalf("setup %s %s = %d, body = %s", method, path, response.Code, response.Body.String())
	}
}

func performJSONRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	target string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, target, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func performRequest(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
