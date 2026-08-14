// Package openapi imports OpenAPI 3.x YAML specs as stub setups (beta).
//
// Importing only registers setups through stub.Service.Install. It does not
// change request matching or response serving behavior.
package openapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pathecho/internal/stub"
	goslog "golang.org/x/exp/slog"
	"gopkg.in/yaml.v3"
)

const maxSchemaDepth = 32

var openAPIPathParam = regexp.MustCompile(`\{([^{}/]+)\}`)

// Installer stores setups. *stub.Service implements this.
type Installer interface {
	Install(path string, spec stub.Spec) error
}

// Result summarizes a directory import.
type Result struct {
	Files   int
	Setups  int
	Skipped int
}

// ImportDir loads OpenAPI 3.x YAML files from dir and installs one active setup
// per supported operation. Empty dir, missing dir, or blank dir is a no-op.
// Later files and later operations replace earlier setups for the same method
// and path.
func ImportDir(dir string, installer Installer) (Result, error) {
	var result Result
	dir = strings.TrimSpace(dir)
	if dir == "" || installer == nil {
		return result, nil
	}

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, err
	}
	if !info.IsDir() {
		return result, fmt.Errorf("APIDIR %q is not a directory", dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return result, err
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	if len(files) == 0 {
		return result, nil
	}

	logger := goslog.Default()
	for _, file := range files {
		fileResult, err := importFile(file, installer)
		if err != nil {
			return result, fmt.Errorf("%s: %w", filepath.Base(file), err)
		}
		result.Files++
		result.Setups += fileResult.Setups
		result.Skipped += fileResult.Skipped
		logger.Info("openapi import",
			"file", filepath.Base(file),
			"setups", fileResult.Setups,
			"skipped", fileResult.Skipped,
		)
	}
	return result, nil
}

func importFile(path string, installer Installer) (Result, error) {
	var result Result
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}

	var doc document
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return result, fmt.Errorf("parse yaml: %w", err)
	}
	version := strings.TrimSpace(doc.OpenAPI)
	if version == "" || !strings.HasPrefix(version, "3.") {
		return result, fmt.Errorf("openapi version %q is not 3.x", version)
	}
	if len(doc.Paths) == 0 {
		return result, nil
	}

	paths := make([]string, 0, len(doc.Paths))
	for path := range doc.Paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, apiPath := range paths {
		item := doc.Paths[apiPath]
		if item == nil {
			continue
		}
		stubPath := toStubPath(apiPath)
		for _, method := range orderedMethods(item) {
			operation := item.operation(method)
			if operation == nil {
				continue
			}
			spec, skipReason, err := operationToSpec(method, operation, doc.Components)
			if err != nil {
				return result, fmt.Errorf("%s %s: %w", method, apiPath, err)
			}
			if skipReason != "" {
				result.Skipped++
				goslog.Default().Info("openapi skip operation",
					"method", method,
					"path", apiPath,
					"reason", skipReason,
				)
				continue
			}
			if err := installer.Install(stubPath, spec); err != nil {
				return result, fmt.Errorf("%s %s: %w", method, stubPath, err)
			}
			result.Setups++
		}
	}
	return result, nil
}

type document struct {
	OpenAPI    string              `yaml:"openapi"`
	Paths      map[string]*pathItem `yaml:"paths"`
	Components map[string]any      `yaml:"components"`
}

type pathItem struct {
	Get     *operation `yaml:"get"`
	Put     *operation `yaml:"put"`
	Post    *operation `yaml:"post"`
	Delete  *operation `yaml:"delete"`
	Patch   *operation `yaml:"patch"`
	Head    *operation `yaml:"head"`
	Options *operation `yaml:"options"`
	Trace   *operation `yaml:"trace"`
}

func (p *pathItem) operation(method string) *operation {
	switch method {
	case "GET":
		return p.Get
	case "PUT":
		return p.Put
	case "POST":
		return p.Post
	case "DELETE":
		return p.Delete
	case "PATCH":
		return p.Patch
	case "HEAD":
		return p.Head
	case "OPTIONS":
		return p.Options
	case "TRACE":
		return p.Trace
	default:
		return nil
	}
}

type operation struct {
	Responses map[string]*response `yaml:"responses"`
}

type response struct {
	Content map[string]*mediaType `yaml:"content"`
}

type mediaType struct {
	Schema   any            `yaml:"schema"`
	Example  any            `yaml:"example"`
	Examples map[string]any `yaml:"examples"`
}

func orderedMethods(item *pathItem) []string {
	candidates := []string{"DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "TRACE"}
	var out []string
	for _, method := range candidates {
		if item.operation(method) != nil {
			out = append(out, method)
		}
	}
	return out
}

func toStubPath(apiPath string) string {
	if apiPath == "" {
		return "/"
	}
	return openAPIPathParam.ReplaceAllString(apiPath, ":$1")
}

func operationToSpec(method string, op *operation, components map[string]any) (stub.Spec, string, error) {
	if !stubSupportedMethod(method) {
		return stub.Spec{}, "unsupported method " + method, nil
	}

	status, mediaTypeName, media, err := pickResponse(op)
	if err != nil {
		return stub.Spec{}, "", err
	}

	headers := map[string]string{}
	var body json.RawMessage
	if media != nil {
		if mediaTypeName != "" {
			headers["Content-Type"] = mediaTypeName
		}
		value, err := mediaExample(media, components)
		if err != nil {
			return stub.Spec{}, "", err
		}
		if value != nil {
			encoded, err := json.Marshal(value)
			if err != nil {
				return stub.Spec{}, "", err
			}
			body = encoded
		}
	}

	spec := stub.Spec{
		Method: method,
		Response: stub.SpecResponse{
			Status:  status,
			Headers: headers,
			Body:    body,
		},
	}
	if len(headers) == 0 {
		spec.Response.Headers = nil
	}
	return spec, "", nil
}

func stubSupportedMethod(method string) bool {
	switch method {
	case "GET", "POST", "PUT", "DELETE":
		return true
	default:
		return false
	}
}

func pickResponse(op *operation) (status int, mediaType string, media *mediaType, err error) {
	if op == nil || len(op.Responses) == 0 {
		return 200, "", nil, nil
	}

	var codes []int
	hasDefault := false
	for key := range op.Responses {
		if strings.EqualFold(key, "default") {
			hasDefault = true
			continue
		}
		code, convErr := strconv.Atoi(key)
		if convErr != nil {
			continue
		}
		if code >= 200 && code <= 299 {
			codes = append(codes, code)
		}
	}
	sort.Ints(codes)

	chosenKey := ""
	chosenStatus := 200
	switch {
	case len(codes) > 0:
		chosenStatus = codes[0]
		for _, code := range codes {
			if code == 200 {
				chosenStatus = 200
				break
			}
		}
		chosenKey = strconv.Itoa(chosenStatus)
		// Response keys may be quoted differently; find the actual map key.
		for key := range op.Responses {
			if key == chosenKey {
				chosenKey = key
				break
			}
			if n, convErr := strconv.Atoi(key); convErr == nil && n == chosenStatus {
				chosenKey = key
				break
			}
		}
	case hasDefault:
		for key := range op.Responses {
			if strings.EqualFold(key, "default") {
				chosenKey = key
				break
			}
		}
		chosenStatus = 200
	default:
		// No 2xx and no default: still register a bare success stub.
		return 200, "", nil, nil
	}

	resp := op.Responses[chosenKey]
	if resp == nil || len(resp.Content) == 0 {
		return chosenStatus, "", nil, nil
	}

	mediaName, mediaValue := pickMediaType(resp.Content)
	return chosenStatus, mediaName, mediaValue, nil
}

func pickMediaType(content map[string]*mediaType) (string, *mediaType) {
	if item, ok := content["application/json"]; ok {
		return "application/json", item
	}
	for name, item := range content {
		if strings.HasPrefix(strings.ToLower(name), "application/json") {
			return name, item
		}
	}
	names := make([]string, 0, len(content))
	for name := range content {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", nil
	}
	return names[0], content[names[0]]
}

func mediaExample(media *mediaType, components map[string]any) (any, error) {
	if media == nil {
		return nil, nil
	}
	if media.Example != nil {
		return normalizeYAML(media.Example), nil
	}
	if len(media.Examples) > 0 {
		names := make([]string, 0, len(media.Examples))
		for name := range media.Examples {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			example := media.Examples[name]
			if value, ok := exampleValue(example); ok {
				return value, nil
			}
		}
	}
	if media.Schema == nil {
		return nil, nil
	}
	return exampleFromSchema(media.Schema, components, nil, 0)
}

func exampleValue(example any) (any, bool) {
	example = normalizeYAML(example)
	switch typed := example.(type) {
	case map[string]any:
		if value, ok := typed["value"]; ok {
			return normalizeYAML(value), true
		}
		if external, ok := typed["externalValue"]; ok {
			_ = external
			return nil, false
		}
		// Treat a bare object as the example payload.
		return typed, true
	default:
		if example == nil {
			return nil, false
		}
		return example, true
	}
}

func exampleFromSchema(schema any, components map[string]any, stack map[string]bool, depth int) (any, error) {
	if schema == nil {
		return nil, nil
	}
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("schema nesting exceeds %d", maxSchemaDepth)
	}

	resolved, ref, err := resolveSchema(schema, components)
	if err != nil {
		return nil, err
	}
	if ref != "" {
		if stack[ref] {
			return map[string]any{}, nil
		}
		if stack == nil {
			stack = map[string]bool{}
		}
		stack[ref] = true
		defer delete(stack, ref)
	}

	obj, ok := resolved.(map[string]any)
	if !ok {
		return normalizeYAML(resolved), nil
	}

	if example, exists := obj["example"]; exists {
		return normalizeYAML(example), nil
	}
	if def, exists := obj["default"]; exists {
		return normalizeYAML(def), nil
	}

	schemaType := schemaTypeOf(obj)
	switch schemaType {
	case "object":
		properties, _ := obj["properties"].(map[string]any)
		out := map[string]any{}
		if len(properties) == 0 {
			return out, nil
		}
		names := make([]string, 0, len(properties))
		for name := range properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value, err := exampleFromSchema(properties[name], components, stack, depth+1)
			if err != nil {
				return nil, err
			}
			out[name] = value
		}
		return out, nil
	case "array":
		if items, exists := obj["items"]; exists {
			value, err := exampleFromSchema(items, components, stack, depth+1)
			if err != nil {
				return nil, err
			}
			return []any{value}, nil
		}
		return []any{}, nil
	case "string":
		if enum := firstEnum(obj); enum != nil {
			return enum, nil
		}
		return "", nil
	case "integer":
		if enum := firstEnum(obj); enum != nil {
			return enum, nil
		}
		return 0, nil
	case "number":
		if enum := firstEnum(obj); enum != nil {
			return enum, nil
		}
		return 0, nil
	case "boolean":
		return false, nil
	case "null":
		return nil, nil
	}

	if allOf, ok := obj["allOf"].([]any); ok && len(allOf) > 0 {
		merged := map[string]any{}
		for _, part := range allOf {
			value, err := exampleFromSchema(part, components, stack, depth+1)
			if err != nil {
				return nil, err
			}
			partObj, ok := value.(map[string]any)
			if !ok {
				continue
			}
			for key, item := range partObj {
				merged[key] = item
			}
		}
		return merged, nil
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		options, ok := obj[key].([]any)
		if ok && len(options) > 0 {
			return exampleFromSchema(options[0], components, stack, depth+1)
		}
	}
	return nil, nil
}

func schemaTypeOf(obj map[string]any) string {
	switch typed := obj["type"].(type) {
	case string:
		return typed
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "null" {
				return text
			}
		}
	}
	if _, ok := obj["properties"]; ok {
		return "object"
	}
	if _, ok := obj["items"]; ok {
		return "array"
	}
	return ""
}

func firstEnum(obj map[string]any) any {
	values, ok := obj["enum"].([]any)
	if !ok || len(values) == 0 {
		return nil
	}
	return normalizeYAML(values[0])
}

func resolveSchema(schema any, components map[string]any) (any, string, error) {
	schema = normalizeYAML(schema)
	obj, ok := schema.(map[string]any)
	if !ok {
		return schema, "", nil
	}
	ref, _ := obj["$ref"].(string)
	if ref == "" {
		return obj, "", nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, ref, fmt.Errorf("unsupported external $ref %q", ref)
	}
	target, err := lookupRef(ref, map[string]any{
		"components": components,
	})
	if err != nil {
		return nil, ref, err
	}
	return target, ref, nil
}

func lookupRef(ref string, root map[string]any) (any, error) {
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	var current any = root
	for _, part := range parts {
		part = strings.ReplaceAll(part, "~1", "/")
		part = strings.ReplaceAll(part, "~0", "~")
		obj, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unresolved $ref %q", ref)
		}
		next, exists := obj[part]
		if !exists {
			return nil, fmt.Errorf("unresolved $ref %q", ref)
		}
		current = normalizeYAML(next)
	}
	return current, nil
}

// normalizeYAML converts yaml.v3 map keys to map[string]any recursively.
func normalizeYAML(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizeYAML(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[fmt.Sprint(key)] = normalizeYAML(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = normalizeYAML(item)
		}
		return out
	default:
		return typed
	}
}
