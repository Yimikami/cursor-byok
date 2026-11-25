package agent

import (
	"encoding/json"
	"strings"
)

// Some OpenAI-compatible providers double-encode collection arguments (for
// example CreatePlan.todos). Decode only where the client's schema explicitly
// requires a collection. Strings, unknown fields and ambiguous schemas retain
// their exact meaning; Cursor still validates and approves the resulting call.
func normalizeNativeArguments(arguments string, schema map[string]any) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	if !json.Valid([]byte(arguments)) || decoder.Decode(&value) != nil {
		return arguments
	}
	value, changed := normalizeNativeArgumentValue(value, schema, 0)
	if !changed {
		return arguments
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return arguments
	}
	return string(encoded)
}

func normalizeNativeArgumentValue(value any, schema map[string]any, depth int) (any, bool) {
	if depth >= 64 || schema == nil {
		return value, false
	}
	// Do not choose a branch of a union or override a schema constraint.
	for _, key := range []string{"anyOf", "oneOf", "allOf", "$ref", "enum", "const"} {
		if _, exists := schema[key]; exists {
			return value, false
		}
	}
	kind, _ := schema["type"].(string)
	changed := false
	if text, ok := value.(string); ok && (kind == "array" || kind == "object") && json.Valid([]byte(text)) {
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if decoder.Decode(&decoded) == nil {
			_, array := decoded.([]any)
			_, object := decoded.(map[string]any)
			if kind == "array" && array || kind == "object" && object {
				value, changed = decoded, true
			}
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		if kind != "object" {
			return value, changed
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, child := range typed {
			childSchema, known := properties[key]
			if !known {
				childSchema = schema["additionalProperties"]
			}
			childSchemaMap, _ := childSchema.(map[string]any)
			normalized, childChanged := normalizeNativeArgumentValue(child, childSchemaMap, depth+1)
			if childChanged {
				typed[key], changed = normalized, true
			}
		}
	case []any:
		if kind != "array" {
			return value, changed
		}
		items, _ := schema["items"].(map[string]any)
		for index, child := range typed {
			normalized, childChanged := normalizeNativeArgumentValue(child, items, depth+1)
			if childChanged {
				typed[index], changed = normalized, true
			}
		}
	}
	return value, changed
}
