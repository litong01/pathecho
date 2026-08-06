package stub

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"text/template"
	"time"
)

func TestTemplateFunctionsCoverNumericStringAndTimeHelpers(t *testing.T) {
	funcs := templateFunctions()
	now := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	source := strings.Join([]string{
		`{{add 1 2 3}}`,
		`{{sub 10 3}}`,
		`{{mul 2 4}}`,
		`{{div 9 3}}`,
		`{{mod 10 3}}`,
		`{{abs -5}}`,
		`{{min 3 1 4}}`,
		`{{max 3 1 4}}`,
		`{{pow 2 3}}`,
		`{{sqrt 9}}`,
		`{{log 1}}`,
		`{{round 1.6}}`,
		`{{floor 1.6}}`,
		`{{ceil 1.2}}`,
		`{{parseInt "42"}}`,
		`{{parseFloat "3.5"}}`,
		`{{lower "AbC"}}`,
		`{{upper "AbC"}}`,
		`{{trim "  x  "}}`,
		`{{contains "abc" "b"}}`,
		`{{replace "a-b" "-" "_"}}`,
		`{{join (split "a,b" ",") "-"}}`,
		`{{default "fallback" ""}}`,
		`{{default "fallback" 0}}`,
		`{{default "fallback" false}}`,
		`{{default "fallback" .NilValue}}`,
		`{{default "kept" "value"}}`,
		`{{formatTime "2006-01-02" .Now}}`,
		`{{unix .Now}}`,
		`{{formatTime "2006-01-02" (addTime .Now "24h")}}`,
		`{{formatTime "2006-01-02" (addTime "24h" .Now)}}`,
		`{{toJSON .Q}}`,
		`{{jsonString "quoted"}}`,
	}, "|")

	tmpl, err := template.New("funcs").Funcs(funcs).Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, struct {
		Q        map[string]string
		Now      time.Time
		NilValue any
	}{
		Q:        map[string]string{"k": "v"},
		Now:      now,
		NilValue: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	wantParts := []string{
		"6", "7", "8", "3", "1", "5", "1", "4", "8", "3", "0", "2", "1", "2",
		"42", "3.5", "abc", "ABC", "x", "true", "a_b", "a-b",
		"fallback", "fallback", "fallback", "fallback", "value",
		"2024-05-06", "1714979289", "2024-05-07", "2024-05-07",
		`{"k":"v"}`, `"quoted"`,
	}
	parts := strings.Split(got, "|")
	if len(parts) != len(wantParts) {
		t.Fatalf("part count = %d, want %d, got %q", len(parts), len(wantParts), got)
	}
	for index, want := range wantParts {
		if parts[index] != want {
			t.Fatalf("part %d = %q, want %q (full=%q)", index, parts[index], want, got)
		}
	}
}

func TestTemplateFunctionErrors(t *testing.T) {
	if _, err := toFloat(struct{}{}); err == nil {
		t.Fatal("toFloat accepted non-number")
	}
	for _, value := range []any{
		int(1), int8(1), int16(1), int32(1), int64(1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1),
		float32(1.5), float64(2.5), json.Number("3.25"), "4.5",
	} {
		if _, err := toFloat(value); err != nil {
			t.Fatalf("toFloat(%T) = %v", value, err)
		}
	}
	if _, err := ensureFinite(math.NaN()); err == nil {
		t.Fatal("NaN accepted")
	}
	if _, err := ensureFinite(math.Inf(1)); err == nil {
		t.Fatal("Inf accepted")
	}
	if _, err := numericFold(func(a, b float64) float64 { return a + b })(); err == nil {
		t.Fatal("empty fold accepted")
	}
	if _, err := numericFold(func(a, b float64) float64 { return a + b })("x"); err == nil {
		t.Fatal("non-numeric fold accepted")
	}
	if _, err := numericFold(func(a, b float64) float64 { return a + b })(1, "x"); err == nil {
		t.Fatal("non-numeric fold arg accepted")
	}
	if _, err := numericUnary(math.Sqrt)("x"); err == nil {
		t.Fatal("unary non-number accepted")
	}
	if _, err := numericBinary(math.Pow)("x", 2); err == nil {
		t.Fatal("binary left non-number accepted")
	}
	if _, err := numericBinary(math.Pow)(2, "x"); err == nil {
		t.Fatal("binary right non-number accepted")
	}
	if _, err := divide(1, 0); err == nil {
		t.Fatal("division by zero accepted")
	}
	if _, err := divide(1, "x"); err == nil {
		t.Fatal("divide non-number accepted")
	}
	if _, err := modulo(1, 0); err == nil {
		t.Fatal("modulo by zero accepted")
	}
	if _, err := modulo(1, "x"); err == nil {
		t.Fatal("modulo non-number accepted")
	}
	if got, err := divide(9, 3); err != nil || got != 3 {
		t.Fatalf("divide = (%v, %v)", got, err)
	}
	if got, err := modulo(10, 3); err != nil || got != 1 {
		t.Fatalf("modulo = (%v, %v)", got, err)
	}
	if _, err := parseFloat("nope"); err == nil {
		t.Fatal("parseFloat accepted invalid")
	}
	if _, err := parseFloat("NaN"); err == nil {
		t.Fatal("parseFloat accepted NaN")
	}
	if _, err := addTime("1h", "2h"); err == nil {
		t.Fatal("addTime without time accepted")
	}
	if _, err := addTime(time.Now(), "not-a-duration"); err == nil {
		t.Fatal("addTime invalid duration accepted")
	}
	if _, err := toJSON(math.Inf(1)); err == nil {
		t.Fatal("toJSON accepted Inf")
	}
	if defaultValue("x", nil) != "x" {
		t.Fatal("default nil failed")
	}
	if defaultValue("x", []string{}) != "x" {
		t.Fatal("default empty slice failed")
	}
	if defaultValue("x", map[string]int{}) != "x" {
		t.Fatal("default empty map failed")
	}
	if defaultValue("x", 0) != "x" || defaultValue("x", uint(0)) != "x" || defaultValue("x", 0.0) != "x" {
		t.Fatal("default zero numeric failed")
	}
	if defaultValue("x", true) != true {
		t.Fatal("default true failed")
	}
}
