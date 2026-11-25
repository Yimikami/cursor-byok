package cursor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type settingsProperty struct {
	key                                   string
	keyStart, valueStart, valueEnd, comma int
}

type settingsDocument struct {
	data, uncommented []byte
	values            map[string]any
	properties        []settingsProperty
	close             int
}

// Keep token positions in the original JSONC document. Normalization is used
// for validation only; writes never serialize the unrelated settings map.
func parseSettingsDocument(data []byte) (*settingsDocument, error) {
	values, err := decodeSettings(data)
	if err != nil {
		return nil, err
	}
	plain, err := stripSettingsComments(data)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeSettingsJSON(data)
	if err != nil {
		return nil, err
	}
	doc := &settingsDocument{data: data, uncommented: plain, values: values}
	skip := func(i int) int {
		for i < len(plain) && settingsSpace(plain[i]) {
			i++
		}
		return i
	}
	i := skip(0) + 1 // root opening brace, already validated
	for {
		i = skip(i)
		if plain[i] == '}' {
			doc.close = i
			return doc, nil
		}
		prop := settingsProperty{keyStart: i, comma: -1}
		decoder := json.NewDecoder(bytes.NewReader(normalized[i:]))
		if err := decoder.Decode(&prop.key); err != nil {
			return nil, err
		}
		i = skip(i + int(decoder.InputOffset()))
		prop.valueStart = skip(i + 1) // colon
		decoder = json.NewDecoder(bytes.NewReader(normalized[prop.valueStart:]))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		prop.valueEnd = prop.valueStart + int(decoder.InputOffset())
		i = skip(prop.valueEnd)
		if plain[i] == ',' {
			prop.comma = i
			i++
		}
		doc.properties = append(doc.properties, prop)
	}
}

func settingsSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }

type settingsChange struct {
	present bool
	value   any
	raw     string // original value spelling, when available in the backup
}

type settingsEdit struct {
	start, end int
	text       string
}

// editSettings changes only named top-level properties. Comments, key order,
// BOM, unrelated numeric spellings, and values edited during a session survive.
func editSettings(data []byte, changes map[string]settingsChange) ([]byte, error) {
	doc, err := parseSettingsDocument(data)
	if err != nil {
		return nil, err
	}
	var edits []settingsEdit
	seen := make(map[string]bool)
	lastKept := -1
	for i, prop := range doc.properties {
		change, changed := changes[prop.key]
		if !changed {
			lastKept = i
			continue
		}
		if seen[prop.key] {
			return nil, fmt.Errorf("duplicate managed setting %q; resolve it before starting", prop.key)
		}
		seen[prop.key] = true
		if change.present {
			value, err := encodedSettingsChange(change)
			if err != nil {
				return nil, err
			}
			edits = append(edits, settingsEdit{prop.valueStart, prop.valueEnd, value})
			lastKept = i
		} else {
			// Preserve trivia even when a user added comments to an injected key.
			// Comment bytes are whitespace in uncommented, while token bytes are not.
			var trivia []byte
			for j := prop.keyStart; j < prop.valueEnd; j++ {
				if settingsSpace(doc.uncommented[j]) {
					trivia = append(trivia, doc.data[j])
				}
			}
			edits = append(edits, settingsEdit{prop.keyStart, prop.valueEnd, string(trivia)})
			if prop.comma >= 0 {
				edits = append(edits, settingsEdit{prop.comma, prop.comma + 1, ""})
			}
		}
	}
	var missing []string
	for key, change := range changes {
		if change.present && !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		if lastKept >= 0 && doc.properties[lastKept].comma < 0 {
			end := doc.properties[lastKept].valueEnd
			edits = append(edits, settingsEdit{end, end, ","})
		}
		newline := "\n"
		if bytes.Contains(data, []byte("\r\n")) {
			newline = "\r\n"
		}
		indent := "  "
		if len(doc.properties) > 0 {
			keyStart := doc.properties[0].keyStart
			lineStart := bytes.LastIndexByte(data[:keyStart], '\n') + 1
			if prefix := string(data[lineStart:keyStart]); strings.TrimSpace(prefix) == "" && prefix != "" {
				indent = prefix
			}
		}
		insertAt := doc.close
		lineStart := bytes.LastIndexByte(data[:doc.close], '\n') + 1
		if len(bytes.TrimSpace(data[lineStart:doc.close])) == 0 {
			insertAt = lineStart
		}
		var out strings.Builder
		if insertAt == 0 || data[insertAt-1] != '\n' {
			out.WriteString(newline)
		}
		for i, key := range missing {
			if i > 0 {
				out.WriteString("," + newline)
			}
			value, err := encodedSettingsChange(changes[key])
			if err != nil {
				return nil, err
			}
			name, _ := json.Marshal(key)
			out.WriteString(indent + string(name) + ": " + value)
		}
		out.WriteString(newline)
		edits = append(edits, settingsEdit{insertAt, insertAt, out.String()})
	}
	// Apply from the end so every span remains relative to the original file.
	// For an insertion at a replacement boundary, insert first.
	for i, j := 0, len(edits)-1; i < j; i, j = i+1, j-1 {
		edits[i], edits[j] = edits[j], edits[i]
	}
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start == edits[j].start {
			return edits[i].end < edits[j].end
		}
		return edits[i].start > edits[j].start
	})
	out := bytes.Clone(data)
	for _, edit := range edits {
		next := make([]byte, 0, len(out)-(edit.end-edit.start)+len(edit.text))
		next = append(next, out[:edit.start]...)
		next = append(next, edit.text...)
		next = append(next, out[edit.end:]...)
		out = next
	}
	if _, err := decodeSettings(out); err != nil {
		return nil, fmt.Errorf("edit settings: %w", err)
	}
	return out, nil
}

func encodedSettingsChange(change settingsChange) (string, error) {
	if change.raw != "" {
		// A damaged backup must not inject additional object properties.
		normalized, err := normalizeSettingsJSON([]byte(change.raw))
		if err != nil {
			return "", fmt.Errorf("invalid original setting value: %w", err)
		}
		var value any
		if err := json.Unmarshal(normalized, &value); err != nil {
			return "", fmt.Errorf("invalid original setting value: %w", err)
		}
		return change.raw, nil
	}
	value, err := json.Marshal(change.value)
	return string(value), err
}

func (d *settingsDocument) rawValues(keys map[string]settingsChange) (map[string]string, error) {
	out := make(map[string]string)
	for _, prop := range d.properties {
		if _, ok := keys[prop.key]; !ok {
			continue
		}
		if _, duplicate := out[prop.key]; duplicate {
			return nil, fmt.Errorf("duplicate managed setting %q; resolve it before starting", prop.key)
		}
		out[prop.key] = string(d.data[prop.valueStart:prop.valueEnd])
	}
	return out, nil
}
