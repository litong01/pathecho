package openapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pathecho/internal/stub"
)

func TestImportRealApisFixture(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "apis")
	service := stub.NewService()
	result, err := ImportDir(dir, service)
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || result.Setups != 2 {
		t.Fatalf("result = %#v", result)
	}
	recorder := httptest.NewRecorder()
	if !service.ServeConfigured(recorder, httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("GET / missing")
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"v2.0"`) {
		t.Fatalf("GET / = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	if !service.ServeConfigured(recorder, httptest.NewRequest(http.MethodGet, "/v2", nil)) {
		t.Fatal("GET /v2 missing")
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"media-types"`) {
		t.Fatalf("GET /v2 = %d %s", recorder.Code, recorder.Body.String())
	}
}
