package stub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseResponseDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantNil bool
		wantErr string
		check   func(t *testing.T, delay *responseDelay)
	}{
		{name: "omitted", raw: "", wantNil: true},
		{name: "null", raw: "null", wantNil: true},
		{name: "zero number", raw: "0", wantNil: true},
		{name: "zero string", raw: `"0"`, wantNil: true},
		{name: "zero random", raw: `"R0"`, wantNil: true},
		{
			name: "fixed number",
			raw:  "150",
			check: func(t *testing.T, delay *responseDelay) {
				t.Helper()
				if got := delay.next(); got != 150*time.Millisecond {
					t.Fatalf("duration = %v, want 150ms", got)
				}
			},
		},
		{
			name: "fixed string",
			raw:  `"80"`,
			check: func(t *testing.T, delay *responseDelay) {
				t.Helper()
				if got := delay.next(); got != 80*time.Millisecond {
					t.Fatalf("duration = %v, want 80ms", got)
				}
			},
		},
		{
			name: "random max",
			raw:  `"R50"`,
			check: func(t *testing.T, delay *responseDelay) {
				t.Helper()
				assertUniformBounds(t, delay.exprs[0], 0, 50*time.Millisecond)
			},
		},
		{
			name: "random range",
			raw:  `"R20-80"`,
			check: func(t *testing.T, delay *responseDelay) {
				t.Helper()
				assertUniformBounds(t, delay.exprs[0], 20*time.Millisecond, 80*time.Millisecond)
			},
		},
		{
			name: "equal random bounds",
			raw:  `"r15-15"`,
			check: func(t *testing.T, delay *responseDelay) {
				t.Helper()
				assertUniformBounds(t, delay.exprs[0], 15*time.Millisecond, 15*time.Millisecond)
				if got := delay.next(); got != 15*time.Millisecond {
					t.Fatalf("duration = %v, want 15ms", got)
				}
			},
		},
		{
			name: "sequence cycles and keeps zero slots",
			raw:  `["5", 0, "10"]`,
			check: func(t *testing.T, delay *responseDelay) {
				t.Helper()
				if len(delay.exprs) != 3 {
					t.Fatalf("exprs = %d, want 3", len(delay.exprs))
				}
				if got := delay.next(); got != 5*time.Millisecond {
					t.Fatalf("first = %v, want 5ms", got)
				}
				if got := delay.next(); got != 0 {
					t.Fatalf("second = %v, want 0", got)
				}
				if got := delay.next(); got != 10*time.Millisecond {
					t.Fatalf("third = %v, want 10ms", got)
				}
				if got := delay.next(); got != 5*time.Millisecond {
					t.Fatalf("fourth = %v, want 5ms (cycle)", got)
				}
			},
		},
		{name: "empty array", raw: `[]`, wantErr: "must not be empty"},
		{name: "invalid array", raw: `[1,`, wantErr: "number, string, or array"},
		{name: "invalid array item", raw: `[1, "nope"]`, wantErr: "delays[1]"},
		{name: "negative", raw: `-1`, wantErr: "must not be negative"},
		{name: "too large", raw: `30001`, wantErr: "at most 30000"},
		{name: "float", raw: `1.5`, wantErr: "integer"},
		{name: "bad random", raw: `"Rx"`, wantErr: "R<max>"},
		{name: "empty random", raw: `"R"`, wantErr: "R<max>"},
		{name: "bad random min", raw: `"Rx-20"`, wantErr: "R<min>-<max>"},
		{name: "bad random max", raw: `"R20-y"`, wantErr: "R<min>-<max>"},
		{name: "empty random max", raw: `"R20-"`, wantErr: "R<min>-<max>"},
		{name: "inverted range", raw: `"R80-20"`, wantErr: "min must be <="},
		{name: "bool", raw: `true`, wantErr: "number or string"},
		{name: "empty string", raw: `""`, wantErr: "must not be empty"},
		{name: "array null item", raw: `[null]`, wantErr: "delays[0]"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var raw json.RawMessage
			if test.raw != "" {
				raw = json.RawMessage(test.raw)
			}
			delay, err := parseResponseDelay(raw)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if test.wantNil {
				if delay != nil {
					t.Fatalf("delay = %#v, want nil", delay)
				}
				return
			}
			if delay == nil {
				t.Fatal("delay is nil")
			}
			if test.check != nil {
				test.check(t, delay)
			}
		})
	}
}

func TestDefaultDelaySleep(t *testing.T) {
	if err := defaultDelaySleep(context.Background(), 0); err != nil {
		t.Fatalf("zero delay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := defaultDelaySleep(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v, want context.Canceled", err)
	}

	start := time.Now()
	if err := defaultDelaySleep(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatalf("short wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Fatalf("elapsed = %v, want at least 5ms", elapsed)
	}
}

func TestParseDelayValueEdgeCases(t *testing.T) {
	t.Parallel()

	if _, err := parseDelayValue(json.RawMessage("   ")); err == nil || !strings.Contains(err.Error(), "number or string") {
		t.Fatalf("whitespace-only value error = %v", err)
	}
	if _, err := parseDelayValue(json.RawMessage(`"unterminated`)); err == nil || !strings.Contains(err.Error(), "invalid delays string") {
		t.Fatalf("bad string error = %v", err)
	}
	if _, err := parseDelayValue(json.RawMessage(`12x`)); err == nil || !strings.Contains(err.Error(), "invalid delays number") {
		t.Fatalf("bad number error = %v", err)
	}
}

func assertUniformBounds(t *testing.T, expr delayExpr, min, max time.Duration) {
	t.Helper()
	uniform, ok := expr.(uniformDelay)
	if !ok {
		t.Fatalf("expr type = %T, want uniformDelay", expr)
	}
	if uniform.min != min || uniform.max != max {
		t.Fatalf("bounds = [%v, %v], want [%v, %v]", uniform.min, uniform.max, min, max)
	}
}

func TestApplyResponseDelay(t *testing.T) {
	t.Run("nil is no-op", func(t *testing.T) {
		if err := applyResponseDelay(context.Background(), nil); err != nil {
			t.Fatalf("apply nil: %v", err)
		}
	})

	t.Run("uses delaySleep", func(t *testing.T) {
		previous := delaySleep
		defer func() { delaySleep = previous }()

		var slept atomic.Int64
		delaySleep = func(ctx context.Context, d time.Duration) error {
			slept.Store(int64(d))
			return nil
		}

		delay, err := parseResponseDelay(json.RawMessage(`25`))
		if err != nil {
			t.Fatal(err)
		}
		if err := applyResponseDelay(context.Background(), delay); err != nil {
			t.Fatal(err)
		}
		if slept.Load() != int64(25*time.Millisecond) {
			t.Fatalf("slept = %d, want %d", slept.Load(), 25*time.Millisecond)
		}
	})

	t.Run("context cancel", func(t *testing.T) {
		previous := delaySleep
		defer func() { delaySleep = previous }()
		delaySleep = func(ctx context.Context, d time.Duration) error {
			return context.Canceled
		}

		delay, err := parseResponseDelay(json.RawMessage(`10`))
		if err != nil {
			t.Fatal(err)
		}
		err = applyResponseDelay(context.Background(), delay)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

func TestUniformDelayStaysInRange(t *testing.T) {
	t.Parallel()

	delay, err := parseResponseDelay(json.RawMessage(`"R10-20"`))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		got := delay.next()
		if got < 10*time.Millisecond || got > 20*time.Millisecond {
			t.Fatalf("sample %d = %v, want in [10ms, 20ms]", i, got)
		}
	}
}

func TestSetupAndServeWithDelays(t *testing.T) {
	previous := delaySleep
	defer func() { delaySleep = previous }()

	var slept atomic.Int64
	delaySleep = func(ctx context.Context, d time.Duration) error {
		slept.Store(int64(d))
		return nil
	}

	service := NewService()
	recorder := httptest.NewRecorder()
	request := jsonRequest(http.MethodPost, "/slow?DO=setup", map[string]any{
		"method": http.MethodGet,
		"delays": "40",
		"response": map[string]any{
			"status": http.StatusOK,
			"body":   "ok",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/slow", nil)
	if !service.ServeConfigured(recorder, request) {
		t.Fatal("configured response was not served")
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("served = %d %q", recorder.Code, recorder.Body.String())
	}
	if slept.Load() != int64(40*time.Millisecond) {
		t.Fatalf("slept = %d, want %d", slept.Load(), 40*time.Millisecond)
	}
}

func TestSetupRejectsInvalidDelays(t *testing.T) {
	service := NewService()
	recorder := httptest.NewRecorder()
	request := jsonRequest(http.MethodPost, "/slow?DO=setup", map[string]any{
		"method": http.MethodGet,
		"delays": "nope",
		"response": map[string]any{
			"body": "ok",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "delays") {
		t.Fatalf("invalid delays setup = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestCanceledDelayRefundsHit(t *testing.T) {
	previous := delaySleep
	defer func() { delaySleep = previous }()
	delaySleep = func(ctx context.Context, d time.Duration) error {
		return context.Canceled
	}

	service := NewService()
	recorder := httptest.NewRecorder()
	request := jsonRequest(http.MethodPost, "/once?DO=setup", map[string]any{
		"method": http.MethodGet,
		"times":  1,
		"delays": 15,
		"response": map[string]any{
			"body": "ok",
		},
	})
	service.HandleSetup(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/once", nil)
	if !service.ServeConfigured(recorder, request) {
		t.Fatal("configured path should still be claimed")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("canceled delay unexpectedly wrote body %q", recorder.Body.String())
	}

	delaySleep = func(ctx context.Context, d time.Duration) error { return nil }

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/once", nil)
	if !service.ServeConfigured(recorder, request) {
		t.Fatal("hit should have been refunded after canceled delay")
	}
	if recorder.Body.String() != "ok" {
		t.Fatalf("retry body = %q", recorder.Body.String())
	}
}
