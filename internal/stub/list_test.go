package stub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleListActiveAndSaved(t *testing.T) {
	service := NewService()

	recorder := httptest.NewRecorder()
	service.HandleList(recorder, httptest.NewRequest(http.MethodPost, "/anything?DO=list", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("empty list status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var empty setupListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode empty list: %v", err)
	}
	if empty.Status != "List" || empty.Count != 0 || len(empty.Setups) != 0 {
		t.Fatalf("empty list = %#v", empty)
	}

	mustSetup(t, service, "/users?DO=setup&DOTIME=3", map[string]any{
		"method": http.MethodGet,
		"delays": "10",
		"response": map[string]any{
			"status": http.StatusAccepted,
			"headers": map[string]string{
				"Content-Type": "application/json",
			},
			"body": map[string]any{
				"name": "{{.Q.name}}",
			},
		},
	})
	mustSetup(t, service, "/status?DO=setup&DONAME=status-done", map[string]any{
		"method": http.MethodGet,
		"response": map[string]any{
			"status": http.StatusOK,
			"body":   "done",
		},
	})
	mustSetup(t, service, "/job?DO=setup", map[string]any{
		"method": http.MethodPost,
		"response": map[string]any{
			"status": http.StatusOK,
			"body":   "queued",
		},
		"then": []string{"status-done"},
	})

	// Consume one active hit so listed times reflects remaining uses.
	served := httptest.NewRecorder()
	if !service.ServeConfigured(served, httptest.NewRequest(http.MethodGet, "/users?name=Sam", nil)) {
		t.Fatal("expected configured /users response")
	}

	recorder = httptest.NewRecorder()
	service.HandleList(recorder, httptest.NewRequest(http.MethodPost, "/ignored?DO=list", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	var listed setupListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listed.Status != "List" || listed.Count != 3 || len(listed.Setups) != 3 {
		t.Fatalf("list summary = %#v", listed)
	}

	byKey := map[string]setupListEntry{}
	for _, item := range listed.Setups {
		key := item.State + " " + item.Method + " " + item.Path
		if item.Name != "" {
			key += " " + item.Name
		}
		byKey[key] = item
	}

	users := byKey["active GET /users"]
	if users.Times != float64(2) { // JSON numbers decode as float64
		t.Fatalf("users times = %#v", users.Times)
	}
	if users.Response.Status != http.StatusAccepted {
		t.Fatalf("users status = %d", users.Response.Status)
	}
	if got := users.Response.Headers["Content-Type"]; got != "application/json" {
		t.Fatalf("users Content-Type = %q", got)
	}
	if string(users.Delays) != `"10"` {
		t.Fatalf("users delays = %s", users.Delays)
	}
	var body map[string]any
	if err := json.Unmarshal(users.Response.Body, &body); err != nil || body["name"] != "{{.Q.name}}" {
		t.Fatalf("users body = %s err = %v", users.Response.Body, err)
	}

	job := byKey["active POST /job"]
	if job.Times != "unlimited" {
		t.Fatalf("job times = %#v", job.Times)
	}
	if len(job.Then) != 1 || job.Then[0] != "status-done" {
		t.Fatalf("job then = %#v", job.Then)
	}

	saved := byKey["saved GET /status status-done"]
	if saved.Name != "status-done" || string(saved.Response.Body) != `"done"` {
		t.Fatalf("saved entry = %#v", saved)
	}
}

func TestHandleListOrderIsStable(t *testing.T) {
	service := NewService()
	mustSetup(t, service, "/b?DO=setup", map[string]any{
		"method":   http.MethodGet,
		"response": map[string]any{"body": "b"},
	})
	mustSetup(t, service, "/a?DO=setup", map[string]any{
		"method":   http.MethodPost,
		"response": map[string]any{"body": "a-post"},
	})
	mustSetup(t, service, "/a?DO=setup", map[string]any{
		"method":   http.MethodGet,
		"response": map[string]any{"body": "a-get"},
	})
	mustSetup(t, service, "/z?DO=setup&DONAME=zebra", map[string]any{
		"method":   http.MethodGet,
		"response": map[string]any{"body": "z"},
	})
	mustSetup(t, service, "/m?DO=setup&DONAME=alpha", map[string]any{
		"method":   http.MethodGet,
		"response": map[string]any{"body": "m"},
	})

	recorder := httptest.NewRecorder()
	service.HandleList(recorder, httptest.NewRequest(http.MethodPost, "/?DO=list", nil))
	var listed setupListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	got := make([]string, 0, len(listed.Setups))
	for _, item := range listed.Setups {
		got = append(got, item.State+" "+item.Method+" "+item.Path+" "+item.Name)
	}
	want := []string{
		"active GET /a ",
		"active POST /a ",
		"active GET /b ",
		"saved GET /m alpha",
		"saved GET /z zebra",
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %q want %q (full %#v)", i, got[i], want[i], got)
		}
	}
}
