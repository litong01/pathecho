package stub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pathecho/internal/httpapi"
	goslog "golang.org/x/exp/slog"
)

func captureLogRecord(t *testing.T, run func()) map[string]any {
	t.Helper()

	var output bytes.Buffer
	previous := goslog.Default()
	goslog.SetDefault(goslog.New(goslog.NewJSONHandler(&output, nil)))
	defer goslog.SetDefault(previous)

	run()

	var record map[string]any
	if err := json.NewDecoder(&output).Decode(&record); err != nil {
		t.Fatalf("decode log record: %v; output = %q", err, output.String())
	}
	return record
}

func TestControlRequestLogging(t *testing.T) {
	service := NewService()

	t.Run("setup includes decoded body", func(t *testing.T) {
		record := captureLogRecord(t, func() {
			recorder := httptest.NewRecorder()
			request := jsonRequest(http.MethodPost, "/users?DO=setup", map[string]any{
				"method": http.MethodGet,
				"response": map[string]any{
					"status": http.StatusCreated,
					"body":   "created",
				},
			})
			service.HandleSetup(recorder, request)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("setup status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})

		if record["msg"] != http.MethodPost || record["path"] != "/users?DO=setup" || record["control"] != "setup" {
			t.Fatalf("setup log metadata = %#v", record)
		}
		content, ok := record["content"].(map[string]any)
		if !ok || content["method"] != http.MethodGet {
			t.Fatalf("setup log content = %#v", record["content"])
		}
	})

	t.Run("path reset includes decoded body", func(t *testing.T) {
		record := captureLogRecord(t, func() {
			recorder := httptest.NewRecorder()
			request := jsonRequest(http.MethodPost, "/users?DO=reset", map[string]any{"method": http.MethodGet})
			service.HandlePathReset(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("reset status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})

		content, ok := record["content"].(map[string]any)
		if record["control"] != "reset" || !ok || content["method"] != http.MethodGet {
			t.Fatalf("reset log = %#v", record)
		}
	})

	t.Run("invalid JSON logs metadata only", func(t *testing.T) {
		record := captureLogRecord(t, func() {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/invalid?DO=setup", strings.NewReader(`{`))
			service.HandleSetup(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("invalid setup status = %d", recorder.Code)
			}
		})

		if record["control"] != "setup" || record["path"] != "/invalid?DO=setup" {
			t.Fatalf("invalid setup log metadata = %#v", record)
		}
		if _, exists := record["content"]; exists {
			t.Fatalf("invalid setup unexpectedly logged content: %#v", record)
		}
	})

	t.Run("large body is truncated", func(t *testing.T) {
		record := captureLogRecord(t, func() {
			recorder := httptest.NewRecorder()
			request := jsonRequest(http.MethodPost, "/large?DO=setup", map[string]any{
				"method":   http.MethodGet,
				"response": map[string]any{"body": strings.Repeat("x", maxLoggedBodySize)},
			})
			service.HandleSetup(recorder, request)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("large setup status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})

		content, ok := record["content"].(string)
		if record["truncated"] != true || !ok || len(content) != maxLoggedBodySize {
			t.Fatalf("truncated setup log = %#v", record)
		}
	})

	t.Run("global reset logs control metadata", func(t *testing.T) {
		record := captureLogRecord(t, func() {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/RESET", nil)
			service.HandleGlobalReset(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("global reset status = %d", recorder.Code)
			}
		})

		if record["control"] != "reset-all" || record["path"] != "/RESET" {
			t.Fatalf("global reset log = %#v", record)
		}
	})
}

func TestServiceSetupServeResetAndValidation(t *testing.T) {
	service := NewService()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/users?DO=setup", strings.NewReader(`{`))
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON setup status = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=setup", map[string]any{
		"method": "PATCH",
		"response": map[string]any{
			"body": "x",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "method must be") {
		t.Fatalf("unsupported method status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"status": 199,
			"body":   "x",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid status setup = %d", recorder.Code)
	}

	times := 0
	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=setup", map[string]any{
		"method": http.MethodGet,
		"times":  times,
		"response": map[string]any{
			"body": "x",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("zero times setup = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=setup&DOTIME=2", map[string]any{
		"method": http.MethodGet,
		"times":  3,
		"response": map[string]any{
			"body": "x",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "only one of times or DOTIME") {
		t.Fatalf("times conflict = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=setup&DOTIME=abc", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"body": "x",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad DOTIME = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"body": "{{bad",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad template = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"headers": map[string]string{"X-Bad": "{{bad"},
			"body":    "ok",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad header template = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"status": 201,
			"headers": map[string]string{
				"Content-Type": "application/json",
				"X-Name":       "{{.Q.name}}",
			},
			"body": map[string]any{
				"name": "{{.Q.name}}",
				"tags": []any{"{{upper .Q.tag}}", 7, true, nil},
				"meta": map[string]any{"ok": true},
			},
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("valid setup = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/users?name=Sam&tag=go", nil)
	if !service.ServeConfigured(recorder, request) {
		t.Fatal("configured response was not served")
	}
	if recorder.Code != 201 || recorder.Header().Get("X-Name") != "Sam" {
		t.Fatalf("served response = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["name"] != "Sam" {
		t.Fatalf("payload = %#v", payload)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=reset", map[string]any{"method": "PATCH"})
	service.HandlePathReset(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid reset method = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/users?DO=reset", strings.NewReader(`{`))
	service.HandlePathReset(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid reset JSON = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/users?DO=reset", map[string]any{"method": http.MethodGet})
	service.HandlePathReset(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"removed":1`) {
		t.Fatalf("path reset = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/users", nil)
	if service.ServeConfigured(recorder, request) {
		t.Fatal("reset entry was still served")
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/again?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"body": "again",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("second setup = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/RESET", nil)
	service.HandleGlobalReset(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"removed":1`) {
		t.Fatalf("global reset = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestServeConfiguredRejectsInvalidRenderedOutput(t *testing.T) {
	service := NewService()

	setup := func(path string, headers map[string]string, body any) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := jsonRequest(http.MethodPost, path+"?DO=setup", map[string]any{
			"method": http.MethodGet,
			"response": map[string]any{
				"headers": headers,
				"body":    body,
			},
		})
		service.HandleSetup(recorder, request)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("setup %s = %d %s", path, recorder.Code, recorder.Body.String())
		}
	}

	setup("/newline", map[string]string{"X-Bad": "line\nbreak"}, "ok")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/newline", nil)
	if !service.ServeConfigured(recorder, request) || recorder.Code != http.StatusInternalServerError {
		t.Fatalf("newline header = %d %s", recorder.Code, recorder.Body.String())
	}

	setup("/math", nil, "{{sqrt -1}}")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/math", nil)
	if !service.ServeConfigured(recorder, request) || recorder.Code != http.StatusInternalServerError {
		t.Fatalf("math failure = %d %s", recorder.Code, recorder.Body.String())
	}

	setup("/invalid-json", map[string]string{"Content-Type": "application/json"}, "not-json")
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/invalid-json", nil)
	if !service.ServeConfigured(recorder, request) || recorder.Code != http.StatusInternalServerError {
		t.Fatalf("invalid JSON = %d %s", recorder.Code, recorder.Body.String())
	}

	setup("/string-body", nil, `hello {{.Q.name}}`)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/string-body?name=world", nil)
	if !service.ServeConfigured(recorder, request) || recorder.Body.String() != "hello world" {
		t.Fatalf("string body = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestServeConfiguredUsesRequestBodyAndJSONPath(t *testing.T) {
	service := NewService()

	recorder := httptest.NewRecorder()
	request := jsonRequest(http.MethodPost, "/echo?DO=setup", map[string]any{
		"method": http.MethodPost,
		"response": map[string]any{
			"status": 200,
			"headers": map[string]string{
				"Content-Type": "application/json",
				"X-Raw":        "{{.Body}}",
			},
			"body": map[string]any{
				"name":    `{{jsonPath "$.user.name" .J}}`,
				"age":     `{{jsonPath "$.user.age" .J}}`,
				"tag":     `{{.J | jsonPath "$.tags[0]"}}`,
				"nested":  `{{jsonPath "$.user" .J}}`,
				"missing": `{{jsonPath "$.nope" .J}}`,
			},
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodPost,
		"/echo",
		strings.NewReader(`{"user":{"name":"Sam","age":30},"tags":["go"]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	if !service.ServeConfigured(recorder, request) {
		t.Fatal("configured response was not served")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Raw"); got != `{"user":{"name":"Sam","age":30},"tags":["go"]}` {
		t.Fatalf("X-Raw = %q", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v body=%s", err, recorder.Body.String())
	}
	if payload["name"] != "Sam" || payload["age"] != float64(30) || payload["tag"] != "go" || payload["missing"] != "" {
		t.Fatalf("payload = %#v", payload)
	}
	nested, ok := payload["nested"].(map[string]any)
	if !ok || nested["name"] != "Sam" || nested["age"] != float64(30) {
		t.Fatalf("nested = %#v", payload["nested"])
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/plain?DO=setup", map[string]any{
		"method": http.MethodPost,
		"response": map[string]any{
			"body": "got={{.Body}}",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("plain setup = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/plain", strings.NewReader("not-json"))
	if !service.ServeConfigured(recorder, request) || recorder.Body.String() != "got=not-json" {
		t.Fatalf("plain body = %d %q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = jsonRequest(http.MethodPost, "/huge?DO=setup", map[string]any{
		"method":   http.MethodPost,
		"response": map[string]any{"body": "{{.Body}}"},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("huge setup = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/huge", strings.NewReader(strings.Repeat("x", httpapi.MaxBodySize+1)))
	if !service.ServeConfigured(recorder, request) || recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestServeConfiguredMatchesTemplatedPath(t *testing.T) {
	service := NewService()

	recorder := httptest.NewRecorder()
	request := jsonRequest(http.MethodPost, "/account/:accountID/users/:userId?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"headers": map[string]string{
				"Content-Type": "application/json",
				"X-Account-ID": "{{.Q.accountID}}",
			},
			"body": map[string]any{
				"accountID": "{{.Q.accountID}}",
				"userId":    "{{.Query.Get `userId`}}",
			},
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/account/acct-123/users/user-456", nil)
	if !service.ServeConfigured(recorder, request) {
		t.Fatal("templated response was not served")
	}
	if recorder.Header().Get("X-Account-ID") != "acct-123" {
		t.Fatalf("X-Account-ID = %q", recorder.Header().Get("X-Account-ID"))
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
	}
	if payload["accountID"] != "acct-123" || payload["userId"] != "user-456" {
		t.Fatalf("payload = %#v", payload)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodGet,
		"/account/path-account/users/path-user?accountID=query-account&userId=query-user",
		nil,
	)
	if !service.ServeConfigured(recorder, request) {
		t.Fatal("templated response with query parameters was not served")
	}
	if recorder.Header().Get("X-Account-ID") != "query-account" {
		t.Fatalf("query parameter did not override path parameter: %q", recorder.Header().Get("X-Account-ID"))
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, recorder.Body.String())
	}
	if payload["accountID"] != "query-account" || payload["userId"] != "query-user" {
		t.Fatalf("query parameters did not win: %#v", payload)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/account/acct-123/groups/group-456", nil)
	if service.ServeConfigured(recorder, request) {
		t.Fatal("non-matching path was served")
	}
}

func TestConfiguredPathMatchingPrecedence(t *testing.T) {
	service := NewService()
	setup := func(path, body string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		service.HandleSetup(recorder, jsonRequest(http.MethodPost, path+"?DO=setup", map[string]any{
			"method":   http.MethodGet,
			"response": map[string]any{"body": body},
		}))
		if recorder.Code != http.StatusCreated {
			t.Fatalf("setup %s = %d %s", path, recorder.Code, recorder.Body.String())
		}
	}

	setup("/account/:accountID", "template")
	setup("/account/special", "exact")
	setup("/category/:categoryID/:kind", "generic-template")
	setup("/category/:categoryID/users", "specific-template")

	recorder := httptest.NewRecorder()
	if !service.ServeConfigured(recorder, httptest.NewRequest(http.MethodGet, "/account/special", nil)) {
		t.Fatal("exact response was not served")
	}
	if recorder.Body.String() != "exact" {
		t.Fatalf("body = %q, want exact", recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	if !service.ServeConfigured(recorder, httptest.NewRequest(http.MethodGet, "/category/123/users", nil)) {
		t.Fatal("specific templated response was not served")
	}
	if recorder.Body.String() != "specific-template" {
		t.Fatalf("body = %q, want specific-template", recorder.Body.String())
	}
}

func TestMemoryStoreResetAndCompleteEdgeCases(t *testing.T) {
	store := newMemoryStore()
	_ = store.Set(http.MethodGet, "/one", &responseEntry{Remaining: 1})
	_ = store.Set(http.MethodPost, "/one", &responseEntry{Remaining: 1})
	_ = store.Set(http.MethodGet, "/two", &responseEntry{Remaining: 1})

	if removed := store.Reset(http.MethodPut, "/missing"); removed != 0 {
		t.Fatalf("missing reset removed = %d", removed)
	}
	if removed := store.Reset("", "/one"); removed != 2 {
		t.Fatalf("path reset removed = %d", removed)
	}
	if removed := store.ResetAll(); removed != 1 {
		t.Fatalf("reset all removed = %d", removed)
	}

	entry := &responseEntry{Remaining: 1}
	_ = store.Set(http.MethodGet, "/retry", entry)
	match, ok := store.Take(http.MethodGet, "/retry")
	if !ok {
		t.Fatal("take failed")
	}
	store.Complete(match, false)
	if entry.Remaining != 1 || entry.InFlight != 0 {
		t.Fatalf("failed complete left remaining=%d inFlight=%d", entry.Remaining, entry.InFlight)
	}
	store.Complete(&responseMatch{Key: responseKey{Method: http.MethodGet, Path: "/retry"}, Entry: &responseEntry{}}, true)
	store.Complete(&responseMatch{Key: responseKey{Method: http.MethodGet, Path: "/missing"}, Entry: entry}, true)

	exhausted := &responseEntry{Remaining: 0}
	_ = store.Set(http.MethodGet, "/empty", exhausted)
	if _, ok := store.Take(http.MethodGet, "/empty"); ok {
		t.Fatal("exhausted entry was taken")
	}
}

func TestMapValuesGetAndFirstValues(t *testing.T) {
	values := mapValues{"a": {"1", "2"}, "b": {}}
	if values.Get("a") != "1" || values.Get("b") != "" || values.Get("missing") != "" {
		t.Fatalf("Get results unexpected: %#v", values)
	}
	first := firstValues(map[string][]string{"a": {"1"}, "b": {}, "c": {"x", "y"}})
	if first["a"] != "1" || first["c"] != "x" {
		t.Fatalf("firstValues = %#v", first)
	}
	if _, ok := first["b"]; ok {
		t.Fatal("empty slice should be omitted")
	}
}

func TestCompileAndRenderJSONBodyEdgeCases(t *testing.T) {
	funcs := templateFunctions()
	if _, _, err := compileResponseBody("empty", nil, funcs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := compileResponseBody("bad-string", json.RawMessage(`"{{`), funcs); err == nil {
		t.Fatal("invalid string template accepted")
	}
	if _, _, err := compileResponseBody("invalid", json.RawMessage(`{`), funcs); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, _, err := compileResponseBody("bad-object", json.RawMessage(`{"x":"{{"}`), funcs); err == nil {
		t.Fatal("invalid nested template accepted")
	}
	if _, _, err := compileResponseBody("array", json.RawMessage(`["{{upper `+"`a`"+`}}", 1]`), funcs); err != nil {
		t.Fatal(err)
	}

	raw := json.RawMessage(`{"name":"{{.Q.name}}","n":"{{add 1 2}}","list":["{{upper ` + "`go`" + `}}"],"flag":true}`)
	data := templateData{Q: map[string]string{"name": "Sam"}, Now: time.Now().UTC()}
	output, err := renderJSONBody(raw, data, funcs)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["name"] != "Sam" || decoded["n"].(float64) != 3 {
		t.Fatalf("decoded = %#v", decoded)
	}

	remaining := 1
	if _, err := renderJSONValue("big", strings.Repeat("x", 8), data, funcs, &remaining); err == nil {
		t.Fatal("expected render size limit error")
	}
}

func TestLimitedBufferWritePaths(t *testing.T) {
	remaining := 0
	buffer := limitedBuffer{remaining: &remaining}
	if n, err := buffer.Write([]byte("x")); n != 0 || err != errRenderedTooLarge {
		t.Fatalf("empty remaining write = (%d, %v)", n, err)
	}
	remaining = 8
	buffer = limitedBuffer{remaining: &remaining}
	if n, err := buffer.Write([]byte("abcd")); n != 4 || err != nil || remaining != 4 {
		t.Fatalf("normal write = (%d, %v, %d)", n, err, remaining)
	}
}

func TestStoreRejectsWhenFull(t *testing.T) {
	service := NewService()
	store := service.store.(*memoryStore)
	for index := 0; index < maxStoredResponses; index++ {
		if err := store.Set(http.MethodGet, fmt.Sprintf("/%d", index), &responseEntry{Remaining: 1}); err != nil {
			t.Fatal(err)
		}
	}
	recorder := httptest.NewRecorder()
	request := jsonRequest(http.MethodPost, "/overflow?DO=setup", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"body": "x",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("full store setup = %d %s", recorder.Code, recorder.Body.String())
	}
}

func jsonRequest(method, target string, body any) *http.Request {
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	request := httptest.NewRequest(method, target, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	return request
}
