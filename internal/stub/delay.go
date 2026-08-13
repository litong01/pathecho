package stub

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Response delay support is intentionally isolated in this file.
// To disable it later: stop parsing setup "delays", leave responseEntry.Delay
// nil, and remove the applyResponseDelay call in ServeConfigured.

const maxDelay = 30 * time.Second

// delaySleep waits for d or until ctx is done. Tests may replace this.
var delaySleep = defaultDelaySleep

func defaultDelaySleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// responseDelay is the compiled form of setup "delays". A nil *responseDelay
// means no artificial latency.
type responseDelay struct {
	exprs []delayExpr
	mu    sync.Mutex
	index int
}

type delayExpr interface {
	duration() time.Duration
}

type fixedDelay time.Duration

func (d fixedDelay) duration() time.Duration { return time.Duration(d) }

type uniformDelay struct {
	min time.Duration
	max time.Duration
}

func (d uniformDelay) duration() time.Duration {
	if d.max <= d.min {
		return d.min
	}
	span := d.max - d.min
	return d.min + time.Duration(rand.Int64N(int64(span)+1))
}

func applyResponseDelay(ctx context.Context, delay *responseDelay) error {
	if delay == nil {
		return nil
	}
	return delaySleep(ctx, delay.next())
}

// clone returns a delay that reuses the immutable expression list but restarts
// the per-hit cycle. It lets a compiled delay be reused across freshly applied
// deferred setups without sharing cycle position.
func (d *responseDelay) clone() *responseDelay {
	if d == nil {
		return nil
	}
	return &responseDelay{exprs: d.exprs}
}

func (d *responseDelay) next() time.Duration {
	d.mu.Lock()
	expr := d.exprs[d.index%len(d.exprs)]
	d.index++
	d.mu.Unlock()
	return expr.duration()
}

// parseResponseDelay compiles the setup "delays" field.
// Accepted JSON shapes:
//   - number: 150
//   - string: "150", "R50", "R20-80"
//   - array of number/string: ["5", 10, "R20-80"] (cycles per hit)
// Omitted/null/"0"/0 means no delay.
func parseResponseDelay(raw json.RawMessage) (*responseDelay, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var values []json.RawMessage
	isArray := raw[0] == '['
	if isArray {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("delays must be a number, string, or array: %w", err)
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("delays array must not be empty")
		}
	} else {
		values = []json.RawMessage{raw}
	}

	exprs := make([]delayExpr, 0, len(values))
	for i, value := range values {
		expr, err := parseDelayValue(value)
		if err != nil {
			if isArray {
				return nil, fmt.Errorf("delays[%d]: %w", i, err)
			}
			return nil, err
		}
		exprs = append(exprs, expr)
	}
	if !hasPositiveDelay(exprs) {
		return nil, nil
	}
	return &responseDelay{exprs: exprs}, nil
}

func hasPositiveDelay(exprs []delayExpr) bool {
	for _, expr := range exprs {
		switch typed := expr.(type) {
		case fixedDelay:
			if typed > 0 {
				return true
			}
		case uniformDelay:
			if typed.max > 0 {
				return true
			}
		}
	}
	return false
}

func parseDelayValue(raw json.RawMessage) (delayExpr, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("delays value must be a number or string")
	}

	switch raw[0] {
	case '"':
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, fmt.Errorf("invalid delays string: %w", err)
		}
		return parseDelayText(strings.TrimSpace(text))
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return nil, fmt.Errorf("invalid delays number: %w", err)
		}
		return parseDelayMillis(number.String())
	default:
		return nil, fmt.Errorf("delays value must be a number or string")
	}
}

func parseDelayText(text string) (delayExpr, error) {
	if text == "" {
		return nil, fmt.Errorf("delays string must not be empty")
	}
	if text[0] == 'R' || text[0] == 'r' {
		return parseUniformDelay(text[1:])
	}
	return parseDelayMillis(text)
}

func parseUniformDelay(spec string) (delayExpr, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("delays random form must be R<max> or R<min>-<max>")
	}

	minText, maxText, hasRange := strings.Cut(spec, "-")
	if !hasRange {
		maxMillis, err := parseMillisToken(spec)
		if err != nil {
			return nil, fmt.Errorf("delays random form must be R<max> or R<min>-<max>")
		}
		return newUniformDelay(0, maxMillis)
	}

	minMillis, err := parseMillisToken(minText)
	if err != nil {
		return nil, fmt.Errorf("delays random form must be R<min>-<max>")
	}
	maxMillis, err := parseMillisToken(maxText)
	if err != nil {
		return nil, fmt.Errorf("delays random form must be R<min>-<max>")
	}
	return newUniformDelay(minMillis, maxMillis)
}

func parseDelayMillis(text string) (delayExpr, error) {
	millis, err := parseMillisToken(text)
	if err != nil {
		return nil, fmt.Errorf("delays: %w", err)
	}
	return fixedDelay(time.Duration(millis) * time.Millisecond), nil
}

func parseMillisToken(text string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("missing milliseconds value")
	}
	if strings.ContainsAny(text, ".eE") {
		return 0, fmt.Errorf("milliseconds must be an integer")
	}
	millis, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, err
	}
	if millis < 0 {
		return 0, fmt.Errorf("milliseconds must not be negative")
	}
	if time.Duration(millis)*time.Millisecond > maxDelay {
		return 0, fmt.Errorf("milliseconds must be at most %d", maxDelay/time.Millisecond)
	}
	return millis, nil
}

func newUniformDelay(minMillis, maxMillis int64) (delayExpr, error) {
	if minMillis > maxMillis {
		return nil, fmt.Errorf("delays random min must be <= max")
	}
	return uniformDelay{
		min: time.Duration(minMillis) * time.Millisecond,
		max: time.Duration(maxMillis) * time.Millisecond,
	}, nil
}
