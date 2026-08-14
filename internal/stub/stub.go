package stub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/ohler55/ojg/jp"
	"github.com/pathecho/internal/httpapi"
	goslog "golang.org/x/exp/slog"
)

const (
	maxLoggedBodySize    = 4 << 10
	maxRenderedSize      = 1 << 20 // 1 MiB
	maxStoredResponses   = 1024
	maxStoredDefinitions = 1024
	maxSetupNameLength   = 128
	unlimitedHits        = -1
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
	Delay           *responseDelay     // optional; compiled from setup "delays"
	Remaining       int
	InFlight        int
	// Then names setup definitions applied when this entry is served. They act
	// like DO=setup calls that only run once the associated request arrives,
	// which matters when an application makes many dependency requests before
	// the follow-up response is needed. Names are resolved at serve time.
	Then []string
	// Original setup fields retained for DO=list. Compiled templates above are
	// what serving uses; these are what operators inspect.
	HeaderSources map[string]string
	BodySource    json.RawMessage
	DelaysSource  json.RawMessage
}

// setupDefinition is a compiled, named setup awaiting application. Definitions
// are registered by a DO=setup carrying a name and are applied by name from the
// "then" list of a served response.
type setupDefinition struct {
	Name   string
	Method string
	Path   string
	Proto  *responseEntry // template copied fresh on each application
}

// freshCopy returns an independent entry that reuses the immutable compiled
// templates but resets per-serve state (InFlight) and clones the delay so its
// cycle position is not shared across applications.
func (e *responseEntry) freshCopy() *responseEntry {
	return &responseEntry{
		Status:          e.Status,
		HeaderTemplates: e.HeaderTemplates,
		BodyTemplate:    e.BodyTemplate,
		BodyJSON:        e.BodyJSON,
		Delay:           e.Delay.clone(),
		Remaining:       e.Remaining,
		Then:            e.Then,
		HeaderSources:   e.HeaderSources,
		BodySource:      append(json.RawMessage(nil), e.BodySource...),
		DelaysSource:    append(json.RawMessage(nil), e.DelaysSource...),
	}
}

type responseMatch struct {
	Key        responseKey
	Entry      *responseEntry
	PathParams map[string]string
}

// listedResponse is a listable snapshot of an active configured response.
type listedResponse struct {
	Method string
	Path   string
	Entry  *responseEntry
}

// listedDefinition is a listable snapshot of a named setup definition.
type listedDefinition struct {
	Name   string
	Method string
	Path   string
	Proto  *responseEntry
}

// responseStore hides the backing store so a shared implementation can be
// added later without changing the HTTP handlers.
type responseStore interface {
	Set(method, path string, entry *responseEntry) error
	Take(method, path string) (*responseMatch, bool)
	Complete(match *responseMatch, success bool)
	Reset(method, path string) int
	ResetAll() int
	List() []listedResponse
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

// Take gets the most specific matching entry and atomically consumes one of
// its allowed hits. Exact paths take precedence over templated paths.
func (s *memoryStore) Take(method, path string) (*responseMatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := responseKey{Method: method, Path: path}
	entry, ok := s.entries[key]
	if !ok {
		key, entry, ok = s.findTemplateMatch(method, path)
		if !ok {
			return nil, false
		}
	}
	if entry.Remaining == 0 {
		return nil, false
	}
	if entry.Remaining > 0 {
		entry.Remaining--
		entry.InFlight++
	}
	params, _ := matchTemplatePath(key.Path, path)
	return &responseMatch{Key: key, Entry: entry, PathParams: params}, true
}

func (s *memoryStore) findTemplateMatch(method, path string) (responseKey, *responseEntry, bool) {
	var bestKey responseKey
	var bestEntry *responseEntry
	bestLiteralCount := -1

	for key, entry := range s.entries {
		if key.Method != method {
			continue
		}
		_, literalCount, ok := matchTemplatePathSpecificity(key.Path, path)
		if !ok || literalCount < bestLiteralCount ||
			(literalCount == bestLiteralCount && bestEntry != nil && key.Path >= bestKey.Path) {
			continue
		}
		bestKey = key
		bestEntry = entry
		bestLiteralCount = literalCount
	}
	return bestKey, bestEntry, bestEntry != nil
}

func (s *memoryStore) Complete(match *responseMatch, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.entries[match.Key]
	if !ok || current != match.Entry || match.Entry.InFlight == 0 {
		return
	}
	match.Entry.InFlight--
	if !success {
		match.Entry.Remaining++
	}
	if success && match.Entry.Remaining == 0 && match.Entry.InFlight == 0 {
		delete(s.entries, match.Key)
	}
}

func matchTemplatePath(pattern, path string) (map[string]string, bool) {
	params, _, ok := matchTemplatePathSpecificity(pattern, path)
	return params, ok
}

func matchTemplatePathSpecificity(pattern, path string) (map[string]string, int, bool) {
	patternSegments := strings.Split(pattern, "/")
	pathSegments := strings.Split(path, "/")
	if len(patternSegments) != len(pathSegments) {
		return nil, 0, false
	}

	params := make(map[string]string)
	literalCount := 0
	hasParam := false
	for index, patternSegment := range patternSegments {
		pathSegment := pathSegments[index]
		if strings.HasPrefix(patternSegment, ":") && len(patternSegment) > 1 {
			if pathSegment == "" {
				return nil, 0, false
			}
			params[patternSegment[1:]] = pathSegment
			hasParam = true
			continue
		}
		if patternSegment != pathSegment {
			return nil, 0, false
		}
		literalCount++
	}
	if !hasParam {
		return nil, 0, false
	}
	return params, literalCount, true
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

func (s *memoryStore) List() []listedResponse {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]listedResponse, 0, len(s.entries))
	for key, entry := range s.entries {
		out = append(out, listedResponse{
			Method: key.Method,
			Path:   key.Path,
			Entry:  entry.freshCopy(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// definitionStore holds named setups awaiting application. It is kept separate
// from responseStore so active responses and pending definitions can be backed
// independently.
type definitionStore interface {
	Define(definition *setupDefinition) error
	Lookup(name string) (*setupDefinition, bool)
	Reset(method, path string) int
	ResetAll() int
	List() []listedDefinition
}

type memoryDefinitionStore struct {
	mu          sync.Mutex
	definitions map[string]*setupDefinition
}

func newMemoryDefinitionStore() *memoryDefinitionStore {
	return &memoryDefinitionStore{definitions: make(map[string]*setupDefinition)}
}

func (s *memoryDefinitionStore) Define(definition *setupDefinition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.definitions[definition.Name]
	if !exists && len(s.definitions) >= maxStoredDefinitions {
		return fmt.Errorf("setup definition limit of %d entries reached", maxStoredDefinitions)
	}
	s.definitions[definition.Name] = definition
	return nil
}

func (s *memoryDefinitionStore) Lookup(name string) (*setupDefinition, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	definition, ok := s.definitions[name]
	return definition, ok
}

// Reset removes definitions registered on path, optionally narrowed to one
// method, so resetting a path also clears what was defined there.
func (s *memoryDefinitionStore) Reset(method, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for name, definition := range s.definitions {
		if definition.Path != path {
			continue
		}
		if method != "" && definition.Method != method {
			continue
		}
		delete(s.definitions, name)
		removed++
	}
	return removed
}

func (s *memoryDefinitionStore) ResetAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := len(s.definitions)
	s.definitions = make(map[string]*setupDefinition)
	return removed
}

func (s *memoryDefinitionStore) List() []listedDefinition {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]listedDefinition, 0, len(s.definitions))
	for _, definition := range s.definitions {
		out = append(out, listedDefinition{
			Name:   definition.Name,
			Method: definition.Method,
			Path:   definition.Path,
			Proto:  definition.Proto.freshCopy(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

type setupRequest struct {
	Method string `json:"method"`
	// Name registers this setup as a definition that "then" lists can apply
	// later. It may also be supplied as the DONAME query parameter.
	Name     string          `json:"name,omitempty"`
	Times    *int            `json:"times,omitempty"`
	Delays   json.RawMessage `json:"delays,omitempty"`
	Response setupResponse   `json:"response"`
	// Then names the setup definitions to apply when this response is served.
	Then []string `json:"then,omitempty"`
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
	Q map[string]string
	H map[string]string
	// Body is the raw request body (any content type). J is the parsed JSON
	// value when Body is valid JSON; otherwise J is nil.
	Body string
	J    any
	Now  time.Time
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
	store       responseStore
	definitions definitionStore
	funcs       template.FuncMap
}

func NewService() *Service {
	return &Service{
		store:       newMemoryStore(),
		definitions: newMemoryDefinitionStore(),
		funcs:       templateFunctions(),
	}
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

// HandleSetup activates an unnamed response immediately. A named setup is saved
// as a definition instead and becomes active only when a served response names
// it in "then".
func (s *Service) HandleSetup(w http.ResponseWriter, r *http.Request) {
	var request setupRequest
	if err := decodeJSONBody(w, r, &request, false); err != nil {
		logControlRequest(r, "setup", nil)
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	logControlRequest(r, "setup", request)

	name, err := resolveSetupName(r, request)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	request.Name = name

	remaining, err := resolveRemaining(r, request)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if remaining != unlimitedHits {
		request.Times = &remaining
	} else {
		request.Times = nil
	}

	result, err := s.install(r.URL.Path, request)
	if err != nil {
		status := http.StatusBadRequest
		if isSetupCapacityError(err) {
			status = http.StatusServiceUnavailable
		}
		writeJSONError(w, status, err)
		return
	}

	response := map[string]any{
		"status": "Setup",
		"method": result.Method,
		"path":   result.Path,
		"times":  remainingValue(result.Remaining),
	}
	if result.Name != "" {
		response["status"] = "Saved"
		response["name"] = result.Name
	}
	if len(result.Then) > 0 {
		response["then"] = result.Then
	}
	writeJSON(w, http.StatusCreated, response)
}

// Spec is a programmatic setup request used by importers (for example OpenAPI).
type Spec struct {
	Method   string
	Name     string
	Times    *int
	Delays   json.RawMessage
	Response SpecResponse
	Then     []string
}

// SpecResponse is the response portion of Spec.
type SpecResponse struct {
	Status  int
	Headers map[string]string
	Body    json.RawMessage
}

type installResult struct {
	Method    string
	Path      string
	Name      string
	Remaining int
	Then      []string
}

// Install compiles and stores a setup for path without going through HTTP.
// An empty Name activates the setup immediately; a non-empty Name saves it as a
// deferred definition. Installing the same method and path again replaces the
// previous active setup.
func (s *Service) Install(path string, spec Spec) error {
	_, err := s.install(path, setupRequest{
		Method: spec.Method,
		Name:   spec.Name,
		Times:  spec.Times,
		Delays: spec.Delays,
		Response: setupResponse{
			Status:  spec.Response.Status,
			Headers: spec.Response.Headers,
			Body:    spec.Response.Body,
		},
		Then: spec.Then,
	})
	return err
}

func (s *Service) install(path string, request setupRequest) (installResult, error) {
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if !supportedMethod(method) {
		return installResult{}, fmt.Errorf("method must be GET, POST, PUT, or DELETE")
	}

	name := strings.TrimSpace(request.Name)
	if name != "" {
		if err := validateSetupName(name); err != nil {
			return installResult{}, err
		}
	}

	remaining := unlimitedHits
	if request.Times != nil {
		remaining = *request.Times
		if remaining != unlimitedHits && remaining <= 0 {
			return installResult{}, fmt.Errorf("times must be a positive integer")
		}
	}

	entry, err := compileEntry(method+" "+path, request, remaining, s.funcs)
	if err != nil {
		return installResult{}, err
	}

	if name == "" {
		if err := s.store.Set(method, path, entry.freshCopy()); err != nil {
			return installResult{}, err
		}
	} else {
		definition := &setupDefinition{
			Name:   name,
			Method: method,
			Path:   path,
			Proto:  entry,
		}
		if err := s.definitions.Define(definition); err != nil {
			return installResult{}, err
		}
	}

	return installResult{
		Method:    method,
		Path:      path,
		Name:      name,
		Remaining: remaining,
		Then:      entry.Then,
	}, nil
}

func isSetupCapacityError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "response store limit") ||
		strings.Contains(message, "setup definition limit")
}

// resolveSetupName reads the setup name from the body or the DONAME query
// parameter. An empty name means the setup is not registered as a definition.
func resolveSetupName(r *http.Request, request setupRequest) (string, error) {
	name := strings.TrimSpace(request.Name)
	if rawName, ok := queryValueFold(r, "DONAME"); ok {
		if name != "" {
			return "", fmt.Errorf("specify only one of name or DONAME")
		}
		name = strings.TrimSpace(rawName)
	}
	if name == "" {
		return "", nil
	}
	if err := validateSetupName(name); err != nil {
		return "", err
	}
	return name, nil
}

func validateSetupName(name string) error {
	if len(name) > maxSetupNameLength {
		return fmt.Errorf("name must be at most %d characters", maxSetupNameLength)
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("name must not contain control characters")
		}
	}
	return nil
}

// resolveRemaining resolves the allowed hit count from the body "times" field
// or the DOTIME query parameter.
func resolveRemaining(r *http.Request, request setupRequest) (int, error) {
	remaining := unlimitedHits
	if request.Times != nil {
		remaining = *request.Times
	}
	if rawTimes, ok := queryValueFold(r, "DOTIME"); ok {
		if request.Times != nil {
			return 0, fmt.Errorf("specify only one of times or DOTIME")
		}
		value, err := strconv.Atoi(rawTimes)
		if err != nil {
			return 0, fmt.Errorf("DOTIME must be a positive integer")
		}
		remaining = value
	}
	if remaining != unlimitedHits && remaining <= 0 {
		return 0, fmt.Errorf("times must be a positive integer")
	}
	return remaining, nil
}

// compileEntry validates and compiles a setup spec into a responseEntry.
// remaining is the resolved hit count for this entry.
func compileEntry(name string, spec setupRequest, remaining int, funcs template.FuncMap) (*responseEntry, error) {
	status := spec.Response.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 200 || status > 599 {
		return nil, fmt.Errorf("response status must be between 200 and 599")
	}

	bodyTemplate, bodyJSON, err := compileResponseBody(name, spec.Response.Body, funcs)
	if err != nil {
		return nil, err
	}
	headerTemplates, err := compileResponseHeaders(name, spec.Response.Headers, funcs)
	if err != nil {
		return nil, err
	}
	delay, err := parseResponseDelay(spec.Delays)
	if err != nil {
		return nil, err
	}
	thenNames, err := normalizeThenNames(spec.Then)
	if err != nil {
		return nil, err
	}
	return &responseEntry{
		Status:          status,
		HeaderTemplates: headerTemplates,
		BodyTemplate:    bodyTemplate,
		BodyJSON:        bodyJSON,
		Delay:           delay,
		Remaining:       remaining,
		Then:            thenNames,
		HeaderSources:   cloneHeaderSources(spec.Response.Headers),
		BodySource:      append(json.RawMessage(nil), spec.Response.Body...),
		DelaysSource:    append(json.RawMessage(nil), spec.Delays...),
	}, nil
}

func cloneHeaderSources(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for key, value := range headers {
		out[key] = value
	}
	return out
}

// normalizeThenNames trims and validates the "then" names. Names are resolved
// when the response is served, so a definition may be registered before or
// after the setup that references it.
func normalizeThenNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for index, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("then[%d]: name must not be empty", index)
		}
		if err := validateSetupName(name); err != nil {
			return nil, fmt.Errorf("then[%d]: %w", index, err)
		}
		if seen[name] {
			return nil, fmt.Errorf("then[%d]: duplicate name %q", index, name)
		}
		seen[name] = true
		normalized = append(normalized, name)
	}
	return normalized, nil
}

// applyThenSetups looks up each named definition and activates it. It runs
// after the triggering response is committed, so a missing name or a full store
// is logged rather than surfaced to the triggering caller.
func (s *Service) applyThenSetups(r *http.Request, names []string) {
	trigger := r.Method + " " + r.URL.Path
	logger := goslog.Default()
	for _, name := range names {
		definition, ok := s.definitions.Lookup(name)
		if !ok {
			logger.Warn("deferred setup not found", "trigger", trigger, "name", name)
			continue
		}
		if err := s.store.Set(definition.Method, definition.Path, definition.Proto.freshCopy()); err != nil {
			logger.Warn("deferred setup failed",
				"trigger", trigger,
				"name", name,
				"method", definition.Method,
				"path", definition.Path,
				"error", err.Error(),
			)
			continue
		}
		logger.Info("deferred setup applied",
			"trigger", trigger,
			"name", name,
			"method", definition.Method,
			"path", definition.Path,
		)
	}
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
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "Reset",
		"method":             method,
		"path":               r.URL.Path,
		"removed":            s.store.Reset(method, r.URL.Path),
		"removedDefinitions": s.definitions.Reset(method, r.URL.Path),
	})
}

// HandleList returns every active configured response and every saved named
// setup definition. The request path is ignored; listing is always global.
func (s *Service) HandleList(w http.ResponseWriter, r *http.Request) {
	logControlRequest(r, "list", nil)
	httpapi.DrainBody(r)

	active := s.store.List()
	saved := s.definitions.List()
	setups := make([]setupListEntry, 0, len(active)+len(saved))
	for _, item := range active {
		setups = append(setups, newSetupListEntry("active", "", item.Method, item.Path, item.Entry))
	}
	for _, item := range saved {
		setups = append(setups, newSetupListEntry("saved", item.Name, item.Method, item.Path, item.Proto))
	}

	writeJSON(w, http.StatusOK, setupListResponse{
		Status: "List",
		Count:  len(setups),
		Setups: setups,
	})
}

type setupListResponse struct {
	Status string           `json:"status"`
	Count  int              `json:"count"`
	Setups []setupListEntry `json:"setups"`
}

type setupListEntry struct {
	State    string          `json:"state"`
	Name     string          `json:"name,omitempty"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Times    any             `json:"times"`
	Delays   json.RawMessage `json:"delays,omitempty"`
	Then     []string        `json:"then,omitempty"`
	Response setupResponse   `json:"response"`
}

func newSetupListEntry(state, name, method, path string, entry *responseEntry) setupListEntry {
	item := setupListEntry{
		State:  state,
		Name:   name,
		Method: method,
		Path:   path,
		Times:  remainingValue(entry.Remaining),
		Response: setupResponse{
			Status: entry.Status,
		},
	}
	if len(entry.Then) > 0 {
		item.Then = append([]string(nil), entry.Then...)
	}
	if len(entry.DelaysSource) > 0 && string(entry.DelaysSource) != "null" {
		item.Delays = append(json.RawMessage(nil), entry.DelaysSource...)
	}
	if len(entry.HeaderSources) > 0 {
		item.Response.Headers = cloneHeaderSources(entry.HeaderSources)
	}
	if len(entry.BodySource) > 0 && string(entry.BodySource) != "null" {
		item.Response.Body = append(json.RawMessage(nil), entry.BodySource...)
	}
	return item
}

func (s *Service) HandleGlobalReset(w http.ResponseWriter, r *http.Request) {
	logControlRequest(r, "reset-all", nil)

	httpapi.DrainBody(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "Reset",
		"removed":            s.store.ResetAll(),
		"removedDefinitions": s.definitions.ResetAll(),
	})
}

func (s *Service) ServeConfigured(w http.ResponseWriter, r *http.Request) bool {
	match, ok := s.store.Take(r.Method, r.URL.Path)
	if !ok {
		return false
	}
	success := false
	defer func() {
		s.store.Complete(match, success)
	}()
	entry := match.Entry

	if err := applyResponseDelay(r.Context(), entry.Delay); err != nil {
		return true
	}

	query := r.URL.Query()
	for name, value := range match.PathParams {
		if _, exists := query[name]; !exists {
			query.Set(name, value)
		}
	}
	headers := r.Header.Clone()
	body, err := httpapi.ReadBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, httpapi.ErrBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSONError(w, status, err)
		return true
	}
	data := templateData{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  mapValues(query),
		Header: headers,
		Q:      firstValues(query),
		H:      firstValues(headers),
		Body:   string(body),
		J:      parseJSONBody(body),
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
	if len(entry.Then) > 0 {
		s.applyThenSetups(r, entry.Then)
	}
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
		"jsonPath":   jsonPath,
	}
}

// parseJSONBody returns the decoded JSON value when data is valid JSON.
// Non-JSON or empty bodies return nil.
func parseJSONBody(data []byte) any {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	var parsed any
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		return nil
	}
	return parsed
}

// jsonPath evaluates a JSONPath expression against doc.
// Call as {{jsonPath "$.name" .J}} or {{.J | jsonPath "$.name"}}.
// Missing matches render as empty. A single string match is returned as-is;
// other single values and multi-matches are returned as JSON text so object
// response bodies can re-parse them into typed JSON values.
func jsonPath(path string, doc any) (string, error) {
	if doc == nil {
		return "", nil
	}
	if raw, ok := doc.(string); ok {
		parsed := parseJSONBody([]byte(raw))
		if parsed == nil {
			if strings.TrimSpace(raw) == "" {
				return "", nil
			}
			return "", fmt.Errorf("jsonPath: document is not valid JSON")
		}
		doc = parsed
	}
	expr, err := jp.ParseString(path)
	if err != nil {
		return "", fmt.Errorf("jsonPath: %w", err)
	}
	results := expr.Get(doc)
	switch len(results) {
	case 0:
		return "", nil
	case 1:
		if value, ok := results[0].(string); ok {
			return value, nil
		}
		return toJSON(results[0])
	default:
		return toJSON(results)
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
