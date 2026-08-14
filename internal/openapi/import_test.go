package openapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pathecho/internal/stub"
)

func TestImportDirNoopWhenMissingOrEmpty(t *testing.T) {
	service := stub.NewService()

	result, err := ImportDir("", service)
	if err != nil || result.Setups != 0 {
		t.Fatalf("blank dir = %#v err=%v", result, err)
	}

	result, err = ImportDir(filepath.Join(t.TempDir(), "missing"), service)
	if err != nil || result.Setups != 0 {
		t.Fatalf("missing dir = %#v err=%v", result, err)
	}

	result, err = ImportDir(t.TempDir(), service)
	if err != nil || result.Files != 0 || result.Setups != 0 {
		t.Fatalf("empty dir = %#v err=%v", result, err)
	}

	recorder := httptest.NewRecorder()
	service.HandleList(recorder, httptest.NewRequest(http.MethodPost, "/?DO=list", nil))
	if !strings.Contains(recorder.Body.String(), `"count":0`) {
		t.Fatalf("expected no setups, got %s", recorder.Body.String())
	}
}

func TestImportDirInstallsOperationsAndAllowsOverride(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "users.yaml"), `
openapi: 3.0.3
paths:
  /users/{userId}:
    get:
      responses:
        "200":
          content:
            application/json:
              example:
                id: "user-1"
                name: Sam
    post:
      responses:
        "201":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/User"
    patch:
      responses:
        "200":
          content:
            application/json:
              example:
                patched: true
  /health:
    get:
      responses:
        "204":
          description: no content
components:
  schemas:
    User:
      type: object
      properties:
        id:
          type: string
          example: generated-id
        age:
          type: integer
`)

	service := stub.NewService()
	result, err := ImportDir(dir, service)
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 1 || result.Setups != 3 || result.Skipped != 1 {
		t.Fatalf("import result = %#v", result)
	}

	assertBody(t, service, http.MethodGet, "/users/abc", `{"id":"user-1","name":"Sam"}`)
	assertBody(t, service, http.MethodPost, "/users/abc", `{"age":0,"id":"generated-id"}`)

	recorder := httptest.NewRecorder()
	if service.ServeConfigured(recorder, httptest.NewRequest(http.MethodGet, "/health", nil)) {
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("health status = %d", recorder.Code)
		}
	} else {
		t.Fatal("expected /health setup")
	}

	// Manual setup still replaces the OpenAPI-generated one.
	if err := service.Install("/users/:userId", stub.Spec{
		Method: http.MethodGet,
		Response: stub.SpecResponse{
			Status: http.StatusOK,
			Body:   json.RawMessage(`"overridden"`),
		},
	}); err != nil {
		t.Fatal(err)
	}
	assertBody(t, service, http.MethodGet, "/users/abc", `overridden`)
}

func TestImportDirLaterFileWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.yaml"), `
openapi: 3.0.0
paths:
  /item:
    get:
      responses:
        "200":
          content:
            application/json:
              example: {"from":"a"}
`)
	writeFile(t, filepath.Join(dir, "b.yaml"), `
openapi: 3.0.0
paths:
  /item:
    get:
      responses:
        "200":
          content:
            application/json:
              example: {"from":"b"}
`)

	service := stub.NewService()
	result, err := ImportDir(dir, service)
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 2 || result.Setups != 2 {
		t.Fatalf("import result = %#v", result)
	}
	assertBody(t, service, http.MethodGet, "/item", `{"from":"b"}`)
}

func TestImportDirRejectsNonOpenAPI3(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "old.yaml"), "swagger: '2.0'\npaths: {}\n")
	_, err := ImportDir(dir, stub.NewService())
	if err == nil || !strings.Contains(err.Error(), "not 3.x") {
		t.Fatalf("error = %v", err)
	}
}

func TestToStubPath(t *testing.T) {
	if got := toStubPath("/users/{userId}/orders/{orderId}"); got != "/users/:userId/orders/:orderId" {
		t.Fatalf("path = %q", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertBody(t *testing.T, service *stub.Service, method, path, want string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, nil)
	if !service.ServeConfigured(recorder, request) {
		t.Fatalf("%s %s was not configured", method, path)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != want {
		t.Fatalf("%s %s body = %s want %s", method, path, got, want)
	}
}
