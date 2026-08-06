package stub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/pathecho/internal/httpapi"
	goslog "golang.org/x/exp/slog"
)

const (
	maxLoggedBodySize  = 4 << 10
	maxRenderedSize    = 1 << 20 // 1 MiB
	maxStoredResponses = 1024
	unlimitedHits      = -1
)

var errRenderedTooLarge = errors.New("rendered response exceeds maximum size")

type responseKey struct {
	Method string
	Path   string
}

type responseEntry struct {
	Status          int
	HeaderTemplates map[string]*template.Template
	BodyTemplate    *template.Template // set when response.body is a JSON string
	BodyJSON        json.RawMessage    // set when response.body is a JSON object/array
	Remaining       int
	InFlight        int
}

// responseStore hides the backing store so a shared implementation can be
// added later without changing the HTTP handlers.
type responseStore interface {
	Set(method, path string, entry *responseEntry) error
	Take(method, path string) (*responseEntry, bool)
	Complete(method, path string, entry *responseEntry, success bool)
	Reset(method, path string) int
	ResetAll() int
}

type memoryStore struct {
	mu      sync.Mutex
	entries map[responseKey]*responseEntry
}

func newMemoryStore() *memoryStore {
	return &memoryStore{entries: make(map[responseKey]*responseEntry)}
}

func (s *memoryStore) Set(method, path string, entry *responseEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := responseKey{Method: method, Path: path}
	if _, exists := s.entries[key]; !exists && len(s.entries) >= maxStoredResponses {
		return fmt.Errorf("response store limit of %d entries reached", maxStoredResponses)
	}
	s.entries[key] = entry
	return nil
}

// Take gets an entry and atomically consumes one of its allowed hits.
func (s *memoryStore) Take(method, path string) (*responseEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := responseKey{Method: method, Path: path}
	entry, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	if entry.Remaining == 0 {
		return nil, false
	}
	if entry.Remaining > 0 {
		entry.Remaining--
		entry.InFlight++
	}
	return entry, true
}

func (s *memoryStore) Complete(
	method string,
	path string,
	entry *responseEntry,
	success bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := responseKey{Method: method, Path: path}
	current, ok := s.entries[key]
	if !ok || current != entry || entry.InFlight == 0 {
		return
	}
	entry.InFlight--
	if !success {
		entry.Remaining++
	}
	if success && entry.Remaining == 0 && entry.InFlight == 0 {
		delete(s.entries, key)
	}
}

func (s *memoryStore) Reset(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if method != "" {
		key := responseKey{Method: method, Path: path}
		if _, ok := s.entries[key]; ok {
			delete(s.entries, key)
			return 1
		}
		return 0
	}

	removed := 0
	for key := range s.entries {
		if key.Path == path {
			delete(s.entries, key)
			removed++
		}
	}
	return removed
}

func (s *memoryStore) ResetAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := len(s.entries)
	s.entries = make(map[responseKey]*responseEntry)
	return removed
}

type setupRequest struct {
	Method   string        `json:"method"`
	Times    *int          `json:"times,omitempty"`
	Response setupResponse `json:"response"`
}

type setupResponse struct {
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

type resetRequest struct {
	Method string `json:"method,omitempty"`
}

type templateData struct {
	Method string
	Path   string
	Query  mapValues
	Header http.Header
	// Q and H are first-value maps for escape-friendly template access
	// such as {{.Q.age}} and {{.H.Authorization}}.
	Q   map[string]string
	H   map[string]string
	Now time.Time
}

type limitedBuffer struct {
	bytes.Buffer
	remaining *int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if *b.remaining <= 0 {
		return 0, errRenderedTooLarge
	}
	if len(data) > *b.remaining {
		allowed := *b.remaining
		_, _ = b.Buffer.Write(data[:allowed])
		*b.remaining = 0
		return allowed, errRenderedTooLarge
	}
	written, err := b.Buffer.Write(data)
	*b.remaining -= written
	return written, err
}

// mapValues is an alias that keeps url.Values.Get available to templates while
// avoiding a second copy of the query parameters.
type mapValues map[string][]string

func (v mapValues) Get(key string) string {
	if values := v[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func firstValues(values map[string][]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, items := range values {
		if len(items) > 0 {
			out[key] = items[0]
		}
	}
	return out
}

type Service struct {
	store responseStore
	funcs template.FuncMap
}

func NewService() *Service {
	return &Service{store: newMemoryStore(), funcs: templateFunctions()}
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, target any, allowEmpty bool) error {
	return httpapi.DecodeJSONBody(w, r, target, allowEmpty)
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	httpapi.WriteJSONError(w, status, err)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	httpapi.WriteJSON(w, status, value)
}

func logControlRequest(r *http.Request, control string, content any) {
	logger := goslog.Default()
	if content == nil {
		logger.Info(r.Method, "path", r.RequestURI, "control", control)
		return
	}

	data, err := json.Marshal(content)
	if err != nil {
		logger.Info(r.Method, "path", r.RequestURI, "control", control, "content", fmt.Sprint(content))
		return
	}
	if len(data) > maxLoggedBodySize {
		logger.Info(
			r.Method,
			"path", r.RequestURI,
			"control", control,
			"content", string(data[:maxLoggedBodySize]),
			"truncated", true,
		)
		return
	}

	var jsonData any
	if json.Unmarshal(data, &jsonData) == nil {
		logger.Info(r.Method, "path", r.RequestURI, "control", control, "content", jsonData)
		return
	}
	logger.Info(r.Method, "path", r.RequestURI, "control", control, "content", string(data))
}

func (s *Service) HandleSetup(w http.ResponseWriter, r *http.Request) {
	var request setupRequest
	if err := decodeJSONBody(w, r, &request, false); err != nil {
		logControlRequest(r, "setup", nil)
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	logControlRequest(r, "setup", request)

	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if !supportedMethod(method) {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("method must be GET, POST, PUT, or DELETE"))
		return
	}

	status := request.Response.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 200 || status > 599 {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("response status must be between 200 and 599"))
		return
	}

	remaining := unlimitedHits
	if request.Times != nil {
		remaining = *request.Times
	}
	if rawTimes, ok := queryValueFold(r, "DOTIME"); ok {
		if request.Times != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("specify only one of times or DOTIME"))
			return
		}
		value, err := strconv.Atoi(rawTimes)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("DOTIME must be a positive integer"))
			return
		}
		remaining = value
	}
	if remaining != unlimitedHits && remaining <= 0 {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("times must be a positive integer"))
		return
	}

	bodyTemplate, bodyJSON, err := compileResponseBody(method+" "+r.URL.Path, request.Response.Body, s.funcs)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	headerTemplates, err := compileResponseHeaders(method+" "+r.URL.Path, request.Response.Headers, s.funcs)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	entry := &responseEntry{
		Status:          status,
		HeaderTemplates: headerTemplates,
		BodyTemplate:    bodyTemplate,
		BodyJSON:        bodyJSON,
		Remaining:       remaining,
	}
	if err := s.store.Set(method, r.URL.Path, entry); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"status": "Setup",
		"method": method,
		"path":   r.URL.Path,
		"times":  remainingValue(remaining),
	})
}

func (s *Service) HandlePathReset(w http.ResponseWriter, r *http.Request) {
	var request resetRequest
	if err := decodeJSONBody(w, r, &request, true); err != nil {
		logControlRequest(r, "reset", nil)
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	logControlRequest(r, "reset", request)

	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method != "" && !supportedMethod(method) {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("method must be GET, POST, PUT, or DELETE"))
		return
	}
	removed := s.store.Reset(method, r.URL.Path)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "Reset",
		"method":  method,
		"path":    r.URL.Path,
		"removed": removed,
	})
}

func (s *Service) HandleGlobalReset(w http.ResponseWriter, r *http.Request) {
	logControlRequest(r, "reset-all", nil)

	httpapi.DrainBody(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "Reset",
		"removed": s.store.ResetAll(),
	})
}

func (s *Service) ServeConfigured(w http.ResponseWriter, r *http.Request) bool {
	entry, ok := s.store.Take(r.Method, r.URL.Path)
	if !ok {
		return false
	}
	success := false
	defer func() {
		s.store.Complete(r.Method, r.URL.Path, entry, success)
	}()

	query := r.URL.Query()
	headers := r.Header.Clone()
	data := templateData{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  mapValues(query),
		Header: headers,
		Q:      firstValues(query),
		H:      firstValues(headers),
		Now:    time.Now().UTC(),
	}

	responseHeaders, err := renderResponseHeaders(entry.HeaderTemplates, data)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("response header template failed: %w", err))
		return true
	}

	var output []byte
	if entry.BodyJSON != nil {
		output, err = renderJSONBody(entry.BodyJSON, data, s.funcs)
	} else {
		remaining := maxRenderedSize
		buf := limitedBuffer{remaining: &remaining}
		err = entry.BodyTemplate.Execute(&buf, data)
		output = buf.Bytes()
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("response template failed: %w", err))
		return true
	}
	if len(output) > maxRenderedSize {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("rendered response exceeds %d bytes", maxRenderedSize))
		return true
	}
	if strings.Contains(strings.ToLower(responseHeaders.Get("Content-Type")), "application/json") &&
		!json.Valid(output) {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("response template produced invalid JSON"))
		return true
	}

	for name, values := range responseHeaders {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(entry.Status)
	_, _ = w.Write(output)
	success = true
	goslog.Default().Info(r.Method, "path", r.RequestURI, "configured", true)
	return true
}

func compileResponseBody(name string, raw json.RawMessage, funcs template.FuncMap) (*template.Template, json.RawMessage, error) {
	if len(raw) == 0 {
		tmpl, err := parseTemplate(name, "", funcs)
		return tmpl, nil, err
	}
	if raw[0] == '"' {
		var source string
		if err := json.Unmarshal(raw, &source); err != nil {
			return nil, nil, fmt.Errorf("invalid response body: %w", err)
		}
		tmpl, err := parseTemplate(name, source, funcs)
		return tmpl, nil, err
	}
	if !json.Valid(raw) {
		return nil, nil, fmt.Errorf("response body must be valid JSON or a JSON string")
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, nil, fmt.Errorf("invalid response body: %w", err)
	}
	if err := validateJSONTemplates(name, document, funcs); err != nil {
		return nil, nil, err
	}
	return nil, append(json.RawMessage(nil), raw...), nil
}

func parseTemplate(name, source string, funcs template.FuncMap) (*template.Template, error) {
	tmpl, err := template.New(name).
		Funcs(funcs).
		Option("missingkey=zero").
		Parse(source)
	if err != nil {
		return nil, fmt.Errorf("invalid response template: %w", err)
	}
	return tmpl, nil
}

func compileResponseHeaders(
	name string,
	headers map[string]string,
	funcs template.FuncMap,
) (map[string]*template.Template, error) {
	compiled := make(map[string]*template.Template, len(headers))
	for headerName, value := range headers {
		tmpl, err := parseTemplate(name+" header "+headerName, value, funcs)
		if err != nil {
			return nil, fmt.Errorf("invalid response header %q: %w", headerName, err)
		}
		compiled[http.CanonicalHeaderKey(headerName)] = tmpl
	}
	return compiled, nil
}

func renderResponseHeaders(
	templates map[string]*template.Template,
	data templateData,
) (http.Header, error) {
	headers := make(http.Header, len(templates))
	remaining := maxRenderedSize
	for name, tmpl := range templates {
		buf := limitedBuffer{remaining: &remaining}
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		value := buf.String()
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("%s contains a newline", name)
		}
		headers.Set(name, value)
	}
	return headers, nil
}

func validateJSONTemplates(name string, value any, funcs template.FuncMap) error {
	switch typed := value.(type) {
	case string:
		_, err := parseTemplate(name, typed, funcs)
		return err
	case []any:
		for index, item := range typed {
			if err := validateJSONTemplates(fmt.Sprintf("%s[%d]", name, index), item, funcs); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range typed {
			if err := validateJSONTemplates(name+"."+key, item, funcs); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderJSONBody(raw json.RawMessage, data templateData, funcs template.FuncMap) ([]byte, error) {
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	remaining := maxRenderedSize
	rendered, err := renderJSONValue("body", document, data, funcs, &remaining)
	if err != nil {
		return nil, err
	}
	output, err := json.Marshal(rendered)
	if err == nil && len(output) > maxRenderedSize {
		return nil, errRenderedTooLarge
	}
	return output, err
}

func renderJSONValue(
	name string,
	value any,
	data templateData,
	funcs template.FuncMap,
	remaining *int,
) (any, error) {
	switch typed := value.(type) {
	case string:
		tmpl, err := parseTemplate(name, typed, funcs)
		if err != nil {
			return nil, err
		}
		buf := limitedBuffer{remaining: remaining}
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, err
		}
		rendered := buf.String()
		var asJSON any
		if err := json.Unmarshal([]byte(rendered), &asJSON); err == nil {
			return asJSON, nil
		}
		return rendered, nil
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			rendered, err := renderJSONValue(fmt.Sprintf("%s[%d]", name, index), item, data, funcs, remaining)
			if err != nil {
				return nil, err
			}
			out[index] = rendered
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			rendered, err := renderJSONValue(name+"."+key, item, data, funcs, remaining)
			if err != nil {
				return nil, err
			}
			out[key] = rendered
		}
		return out, nil
	default:
		return value, nil
	}
}

func supportedMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func queryValueFold(r *http.Request, wanted string) (string, bool) {
	for name, values := range r.URL.Query() {
		if strings.EqualFold(name, wanted) && len(values) > 0 {
			return values[0], true
		}
	}
	return "", false
}

func remainingValue(remaining int) any {
	if remaining == unlimitedHits {
		return "unlimited"
	}
	return remaining
}

func templateFunctions() template.FuncMap {
	return template.FuncMap{
		"add":        numericFold(func(a, b float64) float64 { return a + b }),
		"sub":        numericFold(func(a, b float64) float64 { return a - b }),
		"mul":        numericFold(func(a, b float64) float64 { return a * b }),
		"div":        divide,
		"mod":        modulo,
		"abs":        numericUnary(math.Abs),
		"min":        numericFold(math.Min),
		"max":        numericFold(math.Max),
		"pow":        numericBinary(math.Pow),
		"sqrt":       numericUnary(math.Sqrt),
		"log":        numericUnary(math.Log),
		"round":      numericUnary(math.Round),
		"floor":      numericUnary(math.Floor),
		"ceil":       numericUnary(math.Ceil),
		"parseInt":   parseInt,
		"parseFloat": parseFloat,
		"lower":      strings.ToLower,
		"upper":      strings.ToUpper,
		"trim":       strings.TrimSpace,
		"contains":   strings.Contains,
		"replace":    strings.ReplaceAll,
		"split":      strings.Split,
		"join":       strings.Join,
		"default":    defaultValue,
		"now":        func() time.Time { return time.Now().UTC() },
		"formatTime": func(layout string, value time.Time) string { return value.Format(layout) },
		"addTime":    addTime,
		"unix":       func(value time.Time) int64 { return value.Unix() },
		"toJSON":     toJSON,
		"jsonString": toJSON,
	}
}

func toFloat(value any) (float64, error) {
	switch number := value.(type) {
	case int:
		return float64(number), nil
	case int8:
		return float64(number), nil
	case int16:
		return float64(number), nil
	case int32:
		return float64(number), nil
	case int64:
		return float64(number), nil
	case uint:
		return float64(number), nil
	case uint8:
		return float64(number), nil
	case uint16:
		return float64(number), nil
	case uint32:
		return float64(number), nil
	case uint64:
		return float64(number), nil
	case float32:
		return float64(number), nil
	case float64:
		return number, nil
	case json.Number:
		return number.Float64()
	case string:
		return strconv.ParseFloat(number, 64)
	default:
		return 0, fmt.Errorf("%v is not a number", value)
	}
}

func ensureFinite(value float64) (float64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("math operation produced a non-finite result")
	}
	return value, nil
}

func numericUnary(operation func(float64) float64) func(any) (float64, error) {
	return func(value any) (float64, error) {
		number, err := toFloat(value)
		if err != nil {
			return 0, err
		}
		return ensureFinite(operation(number))
	}
}

func numericBinary(operation func(float64, float64) float64) func(any, any) (float64, error) {
	return func(left, right any) (float64, error) {
		a, err := toFloat(left)
		if err != nil {
			return 0, err
		}
		b, err := toFloat(right)
		if err != nil {
			return 0, err
		}
		return ensureFinite(operation(a, b))
	}
}

func numericFold(operation func(float64, float64) float64) func(...any) (float64, error) {
	return func(values ...any) (float64, error) {
		if len(values) == 0 {
			return 0, fmt.Errorf("at least one number is required")
		}
		result, err := toFloat(values[0])
		if err != nil {
			return 0, err
		}
		for _, value := range values[1:] {
			number, err := toFloat(value)
			if err != nil {
				return 0, err
			}
			result = operation(result, number)
		}
		return ensureFinite(result)
	}
}

func divide(left, right any) (float64, error) {
	divisor, err := toFloat(right)
	if err != nil {
		return 0, err
	}
	if divisor == 0 {
		return 0, fmt.Errorf("division by zero")
	}
	return numericBinary(func(a, b float64) float64 { return a / b })(left, divisor)
}

func modulo(left, right any) (float64, error) {
	divisor, err := toFloat(right)
	if err != nil {
		return 0, err
	}
	if divisor == 0 {
		return 0, fmt.Errorf("modulo by zero")
	}
	return numericBinary(math.Mod)(left, divisor)
}

func parseInt(value string) (int64, error) {
	return strconv.ParseInt(value, 10, 64)
}

func parseFloat(value string) (float64, error) {
	result, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	return ensureFinite(result)
}

func defaultValue(fallback, value any) any {
	if value == nil {
		return fallback
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		if rv.Len() == 0 {
			return fallback
		}
	case reflect.Bool:
		if !rv.Bool() {
			return fallback
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if rv.Int() == 0 {
			return fallback
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if rv.Uint() == 0 {
			return fallback
		}
	case reflect.Float32, reflect.Float64:
		if rv.Float() == 0 {
			return fallback
		}
	}
	return value
}

func addTime(first, second any) (time.Time, error) {
	var value time.Time
	var rawDuration string
	switch typed := first.(type) {
	case time.Time:
		value = typed
		rawDuration, _ = second.(string)
	case string:
		rawDuration = typed
		value, _ = second.(time.Time)
	}
	if value.IsZero() || rawDuration == "" {
		return time.Time{}, fmt.Errorf("addTime requires a time and duration string")
	}
	duration, err := time.ParseDuration(rawDuration)
	if err != nil {
		return time.Time{}, err
	}
	return value.Add(duration), nil
}

func toJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
