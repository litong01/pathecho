package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConfiguredResponseRendersRequestDataAndFunctions(t *testing.T) {
	router := newRouter(newMemoryStore())
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
	router := newRouter(newMemoryStore())
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
	router := newRouter(newMemoryStore())
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
	router := newRouter(newMemoryStore())
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
	router := newRouter(newMemoryStore())
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
	router := newRouter(newMemoryStore())
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

func TestInvalidTemplatesAndMathErrorsAreReported(t *testing.T) {
	router := newRouter(newMemoryStore())

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
}

func TestMemoryStoreConsumesHitsAtomically(t *testing.T) {
	store := newMemoryStore()
	store.Set(http.MethodGet, "/limited", &responseEntry{Remaining: 10})

	var served int32
	var wait sync.WaitGroup
	for request := 0; request < 50; request++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, ok := store.Take(http.MethodGet, "/limited"); ok {
				atomic.AddInt32(&served, 1)
			}
		}()
	}
	wait.Wait()
	if served != 10 {
		t.Fatalf("served hits = %d, want 10", served)
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
