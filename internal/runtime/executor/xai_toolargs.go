package executor

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// xaiIntegerArgKeys lists tool argument names Codex deserializes as i32/i64.
// Grok frequently emits them as JSON floats (44906.0), which makes the client
// reject the call, so whole-number floats are rewritten to integer literals.
var xaiIntegerArgKeys = map[string]bool{
	"session_id":    true,
	"yield_time_ms": true,
	"timeout_ms":    true,
	"max_output":    true,
	"pid":           true,
	"exit_code":     true,
	"line":          true,
	"offset":        true,
	"limit":         true,
	"count":         true,
	"port":          true,
}

// xaiLooksLikeIntegerArgKey reports whether a key name conventionally carries an
// integer, so unknown-but-obvious keys are coerced too.
func xaiLooksLikeIntegerArgKey(key string) bool {
	key = strings.ToLower(key)
	return strings.HasSuffix(key, "_id") ||
		strings.HasSuffix(key, "_ms") ||
		strings.HasSuffix(key, "_count") ||
		strings.HasSuffix(key, "_limit") ||
		key == "id" || key == "n" || key == "index"
}

// xaiIntegerLiteral converts a whole-number JSON float literal to its integer
// form. Values that are already integer literals, non-whole, or outside int64
// range are left alone so no precision is invented or lost.
func xaiIntegerLiteral(number json.Number) (json.Number, bool) {
	text := number.String()
	if !strings.ContainsAny(text, ".eE") {
		return number, false
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return number, false
	}
	truncated := int64(value)
	if float64(truncated) != value {
		return number, false
	}
	return json.Number(strconv.FormatInt(truncated, 10)), true
}

func coerceXAIArgumentMap(values map[string]any) bool {
	changed := false
	for key, value := range values {
		switch typed := value.(type) {
		case json.Number:
			if !xaiIntegerArgKeys[key] && !xaiLooksLikeIntegerArgKey(key) {
				continue
			}
			if coerced, ok := xaiIntegerLiteral(typed); ok {
				values[key] = coerced
				changed = true
			}
		case map[string]any:
			if coerceXAIArgumentMap(typed) {
				changed = true
			}
		case []any:
			if coerceXAIArgumentSlice(typed) {
				changed = true
			}
		}
	}
	return changed
}

func coerceXAIArgumentSlice(values []any) bool {
	changed := false
	for _, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			if coerceXAIArgumentMap(typed) {
				changed = true
			}
		case []any:
			if coerceXAIArgumentSlice(typed) {
				changed = true
			}
		}
	}
	return changed
}

// coerceXAIToolArgumentsJSON rewrites a JSON object of tool arguments so that
// whole-number floats on integer-typed keys become integer literals. Numbers on
// every other key are decoded as json.Number and re-emitted verbatim.
func coerceXAIToolArgumentsJSON(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" || trimmed[0] != '{' {
		return arguments
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return arguments
	}
	if !coerceXAIArgumentMap(values) {
		return arguments
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(values); err != nil {
		return arguments
	}
	return strings.TrimRight(buf.String(), "\n")
}
