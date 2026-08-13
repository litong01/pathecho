package stub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustSetup(t *testing.T, service *Service, target string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	service.HandleSetup(recorder, jsonRequest(http.MethodPost, target, body))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup %s = %d %s", target, recorder.Code, recorder.Body.String())
	}
	return recorder
}

func serve(t *testing.T, service *Service, method, target string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	recorder := httptest.NewRecorder()
	served := service.ServeConfigured(recorder, httptest.NewRequest(method, target, nil))
	return recorder, served
}

func TestNamedSetupStaysInactiveUntilTriggerServed(t *testing.T) {
	service := NewService()

	response := mustSetup(t, service, "/profile?DO=setup", map[string]any{
		"method":   http.MethodGet,
		"name":     "profile-ready",
		"response": map[string]any{"status": 201, "body": "profile"},
	})
	if !strings.Contains(response.Body.String(), `"status":"Saved"`) {
		t.Fatalf("named setup response = %s", response.Body.String())
	}
	mustSetup(t, service, "/login?DO=setup", map[string]any{
		"method":   http.MethodPost,
		"response": map[string]any{"body": "login"},
		"then":     []string{"profile-ready"},
	})

	if _, served := serve(t, service, http.MethodGet, "/profile"); served {
		t.Fatal("named setup was active before the trigger")
	}
	if _, served := serve(t, service, http.MethodPost, "/login"); !served {
		t.Fatal("trigger was not served")
	}
	recorder, served := serve(t, service, http.MethodGet, "/profile")
	if !served || recorder.Code != 201 || recorder.Body.String() != "profile" {
		t.Fatalf("deferred serve = served:%v code:%d body:%q", served, recorder.Code, recorder.Body.String())
	}
}

func TestNamedSetupAcceptsDONAME(t *testing.T) {
	service := NewService()

	mustSetup(t, service, "/profile?DO=setup&DONAME=profile-ready", map[string]any{
		"method":   http.MethodGet,
		"response": map[string]any{"body": "profile"},
	})
	mustSetup(t, service, "/login?DO=setup", map[string]any{
		"method":   http.MethodPost,
		"response": map[string]any{"body": "login"},
		"then":     []string{"profile-ready"},
	})

	if _, served := serve(t, service, http.MethodPost, "/login"); !served {
		t.Fatal("trigger was not served")
	}
	if _, served := serve(t, service, http.MethodGet, "/profile"); !served {
		t.Fatal("setup named via DONAME was not applied")
	}
}

func TestThenNamesResolveAtServeTime(t *testing.T) {
	service := NewService()

	mustSetup(t, service, "/login?DO=setup", map[string]any{
		"method":   http.MethodPost,
		"response": map[string]any{"body": "login"},
		"then":     []string{"profile-ready"},
	})
	mustSetup(t, service, "/profile?DO=setup", map[string]any{
		"method":   http.MethodGet,
		"name":     "profile-ready",
		"response": map[string]any{"body": "profile"},
	})

	if _, served := serve(t, service, http.MethodPost, "/login"); !served {
		t.Fatal("trigger was not served")
	}
	if _, served := serve(t, service, http.MethodGet, "/profile"); !served {
		t.Fatal("late named setup was not applied")
	}
}

func TestThenReplacesActiveResponse(t *testing.T) {
	service := NewService()

	mustSetup(t, service, "/status?DO=setup", map[string]any{
		"method":   http.MethodGet,
		"response": map[string]any{"body": "pending"},
	})
	mustSetup(t, service, "/status?DO=setup", map[string]any{
		"method":   http.MethodGet,
		"name":     "status-done",
		"response": map[string]any{"body": "done"},
	})
	mustSetup(t, service, "/job?DO=setup", map[string]any{
		"method":   http.MethodPost,
		"response": map[string]any{"body": "queued"},
		"then":     []string{"status-done"},
	})

	recorder, _ := serve(t, service, http.MethodGet, "/status")
	if recorder.Body.String() != "pending" {
		t.Fatalf("status before trigger = %q", recorder.Body.String())
	}
	serve(t, service, http.MethodPost, "/job")
	recorder, _ = serve(t, service, http.MethodGet, "/status")
	if recorder.Body.String() != "done" {
		t.Fatalf("status after trigger = %q", recorder.Body.String())
	}
}

func TestNamedSetupChainsStayFlat(t *testing.T) {
	service := NewService()

	mustSetup(t, service, "/step2?DO=setup", map[string]any{
		"method": http.MethodGet, "name": "step2",
		"response": map[string]any{"body": "step2"}, "then": []string{"step3"},
	})
	mustSetup(t, service, "/step3?DO=setup", map[string]any{
		"method": http.MethodGet, "name": "step3",
		"response": map[string]any{"body": "step3"},
	})
	mustSetup(t, service, "/step1?DO=setup", map[string]any{
		"method": http.MethodGet, "response": map[string]any{"body": "step1"},
		"then": []string{"step2"},
	})

	if _, served := serve(t, service, http.MethodGet, "/step2"); served {
		t.Fatal("step2 active before step1")
	}
	serve(t, service, http.MethodGet, "/step1")
	if _, served := serve(t, service, http.MethodGet, "/step3"); served {
		t.Fatal("step3 active before step2")
	}
	if _, served := serve(t, service, http.MethodGet, "/step2"); !served {
		t.Fatal("step2 was not applied")
	}
	if _, served := serve(t, service, http.MethodGet, "/step3"); !served {
		t.Fatal("step3 was not applied")
	}
}

func TestThenAppliesFreshCopyOnEachServe(t *testing.T) {
	service := NewService()

	mustSetup(t, service, "/deferred?DO=setup", map[string]any{
		"method": http.MethodGet, "name": "deferred", "times": 1,
		"response": map[string]any{"body": "deferred"},
	})
	mustSetup(t, service, "/trigger?DO=setup", map[string]any{
		"method": http.MethodGet, "times": 2,
		"response": map[string]any{"body": "trigger"}, "then": []string{"deferred"},
	})

	for attempt := 0; attempt < 2; attempt++ {
		if _, served := serve(t, service, http.MethodGet, "/trigger"); !served {
			t.Fatalf("trigger %d was not served", attempt)
		}
		if _, served := serve(t, service, http.MethodGet, "/deferred"); !served {
			t.Fatalf("deferred %d was not served", attempt)
		}
		if _, served := serve(t, service, http.MethodGet, "/deferred"); served {
			t.Fatalf("deferred %d exceeded times", attempt)
		}
	}
}

func TestNamedSetupSupportsTemplatedPath(t *testing.T) {
	service := NewService()

	mustSetup(t, service, "/orders/:orderID?DO=setup", map[string]any{
		"method": http.MethodGet, "name": "order-detail",
		"response": map[string]any{
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    map[string]any{"orderID": "{{.Q.orderID}}"},
		},
	})
	mustSetup(t, service, "/orders?DO=setup", map[string]any{
		"method": http.MethodPost, "response": map[string]any{"body": "ok"},
		"then": []string{"order-detail"},
	})

	serve(t, service, http.MethodPost, "/orders")
	recorder, served := serve(t, service, http.MethodGet, "/orders/order-123")
	if !served || !strings.Contains(recorder.Body.String(), `"orderID":"order-123"`) {
		t.Fatalf("templated response = served:%v body:%s", served, recorder.Body.String())
	}
}

func TestThenNotAppliedWhenTriggerFails(t *testing.T) {
	service := NewService()

	mustSetup(t, service, "/deferred?DO=setup", map[string]any{
		"method": http.MethodGet, "name": "deferred",
		"response": map[string]any{"body": "deferred"},
	})
	mustSetup(t, service, "/trigger?DO=setup", map[string]any{
		"method": http.MethodGet, "response": map[string]any{"body": "{{sqrt -1}}"},
		"then": []string{"deferred"},
	})

	recorder, served := serve(t, service, http.MethodGet, "/trigger")
	if !served || recorder.Code != http.StatusInternalServerError {
		t.Fatalf("trigger = served:%v code:%d", served, recorder.Code)
	}
	if _, served := serve(t, service, http.MethodGet, "/deferred"); served {
		t.Fatal("deferred setup applied after failed trigger")
	}
}

func TestNamedSetupValidation(t *testing.T) {
	service := NewService()

	tests := []struct {
		name   string
		target string
		body   map[string]any
		want   string
	}{
		{"name conflict", "/x?DO=setup&DONAME=a", map[string]any{
			"method": http.MethodGet, "name": "b", "response": map[string]any{"body": "x"},
		}, "only one of name or DONAME"},
		{"long name", "/x?DO=setup", map[string]any{
			"method": http.MethodGet, "name": strings.Repeat("n", maxSetupNameLength+1),
			"response": map[string]any{"body": "x"},
		}, "at most"},
		{"empty then", "/x?DO=setup", map[string]any{
			"method": http.MethodGet, "response": map[string]any{"body": "x"},
			"then": []string{" "},
		}, "then[0]: name must not be empty"},
		{"duplicate then", "/x?DO=setup", map[string]any{
			"method": http.MethodGet, "response": map[string]any{"body": "x"},
			"then": []string{"a", "a"},
		}, "then[1]: duplicate name"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			service.HandleSetup(recorder, jsonRequest(http.MethodPost, test.target, test.body))
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDefinitionStoreRejectsWhenFull(t *testing.T) {
	service := NewService()
	store := service.definitions.(*memoryDefinitionStore)
	for index := 0; index < maxStoredDefinitions; index++ {
		if err := store.Define(&setupDefinition{
			Name: fmt.Sprintf("definition-%d", index), Method: http.MethodGet,
			Path: "/full", Proto: &responseEntry{Remaining: 1},
		}); err != nil {
			t.Fatalf("define %d: %v", index, err)
		}
	}

	recorder := httptest.NewRecorder()
	service.HandleSetup(recorder, jsonRequest(http.MethodPost, "/overflow?DO=setup", map[string]any{
		"method": http.MethodGet, "name": "overflow",
		"response": map[string]any{"body": "x"},
	}))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("full store = %d %s", recorder.Code, recorder.Body.String())
	}
}
