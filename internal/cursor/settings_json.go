package cursor

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Cursor settings use JSON with comments and trailing commas (JSONC). Strip
// only those extensions, leaving string contents and byte offsets intact so
// the standard decoder can still reject malformed or truncated documents.
func decodeSettings(data []byte) (map[string]any, error) {
	b := bytes.Clone(data)
	if bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}) {
		copy(b[:3], "   ")
	}
	inString, escaped := false, false
	for i := 0; i < len(b); i++ {
		if inString {
			if escaped {
				escaped = false
			} else if b[i] == '\\' {
				escaped = true
			} else if b[i] == '"' {
				inString = false
			}
			continue
		}
		if b[i] == '"' {
			inString = true
			continue
		}
		if b[i] != '/' || i+1 >= len(b) {
			continue
		}
		switch b[i+1] {
		case '/':
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				b[i] = ' '
				i++
			}
		case '*':
			b[i], b[i+1] = ' ', ' '
			i += 2
			closed := false
			for i < len(b) {
				if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
					b[i], b[i+1] = ' ', ' '
					i++
					closed = true
					break
				}
				if b[i] != '\n' && b[i] != '\r' {
					b[i] = ' '
				}
				i++
			}
			if !closed {
				return nil, errors.New("unterminated block comment")
			}
		}
	}
	inString, escaped = false, false
	for i := 0; i < len(b); i++ {
		if inString {
			if escaped {
				escaped = false
			} else if b[i] == '\\' {
				escaped = true
			} else if b[i] == '"' {
				inString = false
			}
			continue
		}
		if b[i] == '"' {
			inString = true
			continue
		}
		if b[i] == ',' {
			previous := i - 1
			for previous >= 0 && (b[previous] == ' ' || b[previous] == '\t' || b[previous] == '\r' || b[previous] == '\n') {
				previous--
			}
			if previous < 0 || b[previous] == '{' || b[previous] == '[' || b[previous] == ',' || b[previous] == ':' {
				continue
			}
			j := i + 1
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\r' || b[j] == '\n') {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				b[i] = ' '
			}
		}
	}
	var settings map[string]any
	if err := json.Unmarshal(b, &settings); err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, errors.New("settings must be a JSON object")
	}
	return settings, nil
}
