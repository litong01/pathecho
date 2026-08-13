package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDrainBodyNilSafe(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Body = nil
	DrainBody(request)
}

func TestReadBodyLimitsAndEmpty(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Body = nil
	data, err := ReadBody(request)
	if err != nil || data != nil {
		t.Fatalf("nil body = %q, %v", data, err)
	}

	request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ok":true}`))
	data, err = ReadBody(request)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("ReadBody = %q, %v", data, err)
	}

	request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", MaxBodySize+1)))
	if _, err := ReadBody(request); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("oversized body err = %v", err)
	}
}

func TestDecodeJSONBodyRejectsMultipleValuesAndTrailingJunk(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}{"b":2}`))
	var target map[string]any
	if err := DecodeJSONBody(recorder, request, &target, false); err == nil ||
		!strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("multiple values error = %v", err)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}[`))
	if err := DecodeJSONBody(recorder, request, &target, false); err == nil {
		t.Fatal("trailing junk was accepted")
	}
}

func TestControlActionAndWriteHelpers(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/path?Do=%20SETUP%20", nil)
	if got := ControlAction(request); got != "setup" {
		t.Fatalf("ControlAction = %q", got)
	}
	request = httptest.NewRequest(http.MethodPost, "/path", nil)
	if got := ControlAction(request); got != "" {
		t.Fatalf("missing ControlAction = %q", got)
	}

	recorder := httptest.NewRecorder()
	WriteJSONError(recorder, http.StatusBadRequest, errString("boom"))
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), `"Failed"`) ||
		!strings.Contains(recorder.Body.String(), "boom") {
		t.Fatalf("WriteJSONError = %d %s", recorder.Code, recorder.Body.String())
	}
}

type errString string

func (e errString) Error() string { return string(e) }
