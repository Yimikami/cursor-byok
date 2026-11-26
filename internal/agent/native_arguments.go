package agent

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

// Cursor 3.32.4 can send AgentTool.parameters as {"jsonSchema": {...}}
// instead of the JSON schema itself. Unwrap only that exact outer container:
// schema keywords, properties named jsonSchema, and unknown shapes stay intact.
func nativeFunctionSchema(parameters map[string]any) map[string]any {
	if len(parameters) == 1 {
		if schema, ok := parameters["jsonSchema"].(map[string]any); ok {
			return schema
		}
	}
	return parameters
}

// Some OpenAI-compatible providers double-encode collection arguments (for
// example CreatePlan.todos). Decode only where the client's schema explicitly
// requires a collection. Strings, unknown fields and ambiguous schemas retain
// their exact meaning; Cursor still validates and approves the resulting call.
func normalizeNativeArguments(arguments string, schema map[string]any) string {
	schema = nativeFunctionSchema(schema)
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
			if (kind == "array" && array || kind == "object" && object) && nativeCollectionNumbersRoundTrip(decoded, 0) {
				value, changed = decoded, true
			}
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		if kind != "object" {
			return value, changed
		}
		// Pattern schemas can overlap named properties, and matching keys do
		// not use additionalProperties. Without ECMA-262 pattern matching and
		// schema conjunction evaluation, leave every member's value intact.
		if _, patterned := schema["patternProperties"]; patterned {
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
		var prefix []any
		if rawPrefix, exists := schema["prefixItems"]; exists {
			var ok bool
			prefix, ok = rawPrefix.([]any)
			if !ok {
				return value, changed
			}
		}
		items, _ := schema["items"].(map[string]any)
		for index, child := range typed {
			childSchema := items
			if index < len(prefix) {
				// items applies only after the tuple prefix. A boolean prefix
				// schema supplies no collection type, so it cannot coerce input.
				childSchema, _ = prefix[index].(map[string]any)
			}
			normalized, childChanged := normalizeNativeArgumentValue(child, childSchema, depth+1)
			if childChanged {
				typed[index], changed = normalized, true
			}
		}
	}
	return value, changed
}

// The native structured tool response uses protobuf doubles. Do not turn a
// string into a collection if JSON -> float64 -> JSON would change a number:
// Cursor could execute a different ID than the one retained in provider history.
func nativeCollectionNumbersRoundTrip(value any, depth int) bool {
	if depth >= 64 {
		return false
	}
	switch value := value.(type) {
	case json.Number:
		literal := value.String()
		// Bound exact-decimal comparison work for adversarial number spellings.
		if len(literal) > 1024 {
			return false
		}
		if i := strings.IndexAny(literal, "eE"); i >= 0 {
			exponent, err := strconv.ParseInt(literal[i+1:], 10, 32)
			if err != nil || exponent < -1000 || exponent > 1000 {
				return false
			}
		}
		number, err := value.Float64()
		if err != nil {
			return false
		}
		encoded, err := json.Marshal(number)
		if err != nil {
			return false
		}
		original, ok := new(big.Rat).SetString(literal)
		roundTrip, roundTripOK := new(big.Rat).SetString(string(encoded))
		return ok && roundTripOK && original.Cmp(roundTrip) == 0
	case map[string]any:
		for _, child := range value {
			if !nativeCollectionNumbersRoundTrip(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range value {
			if !nativeCollectionNumbersRoundTrip(child, depth+1) {
				return false
			}
		}
	}
	return true
}
