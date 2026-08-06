package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const MaxBodySize = 1 << 20 // 1 MiB

func DrainBody(r *http.Request) {
	if r.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, MaxBodySize+1))
	_ = r.Body.Close()
}

func ControlAction(r *http.Request) string {
	value, ok := queryValueFold(r, "DO")
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func DecodeJSONBody(w http.ResponseWriter, r *http.Request, target any, allowEmpty bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodySize)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("invalid JSON body: multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func WriteJSONError(w http.ResponseWriter, status int, err error) {
	WriteJSON(w, status, map[string]any{"status": "Failed", "error": err.Error()})
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func queryValueFold(r *http.Request, wanted string) (string, bool) {
	for name, values := range r.URL.Query() {
		if strings.EqualFold(name, wanted) && len(values) > 0 {
			return values[0], true
		}
	}
	return "", false
}
