package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
)

func validateExecToolArguments(pc PendingToolCall) string {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(pc.Arguments), &object); err != nil || object == nil {
		return pc.Name + ": arguments must be a JSON object"
	}
	var required []string
	switch pc.Name {
	case "Read", "Write", "Delete", "StrReplace":
		required = []string{"path"}
	case "Shell":
		required = []string{"command"}
	case "Glob":
		required = []string{"glob_pattern"}
	case "Grep":
		required = []string{"pattern"}
	case "FetchMcpResource":
		required = []string{"server", "uri"}
	}
	for _, key := range required {
		var value string
		if err := json.Unmarshal(object[key], &value); err != nil || value == "" {
			return pc.Name + ": " + key + " must be a nonempty string"
		}
	}
	if pc.Name == "Write" {
		value, ok := object["contents"]
		if !ok {
			value, ok = object["content"]
		}
		var content string
		if !ok || string(value) == "null" || json.Unmarshal(value, &content) != nil {
			return "Write: contents must be a string (empty is allowed)"
		}
	}
	if pc.Name == "Read" {
		if value, ok := object["limit"]; ok {
			var limit int32
			if json.Unmarshal(value, &limit) != nil || limit <= 0 {
				return "Read: limit must be a positive integer"
			}
		}
	}
	return ""
}

// Exec ReadArgs asks Cursor for the file; the tool's range and line-number
// options live only on ReadToolArgs and must be applied before model delivery.
func selectReadOutput(args *agentv1.ReadToolArgs, source *agentv1.ReadSuccess) *agentv1.ReadToolSuccess {
	result := &agentv1.ReadToolSuccess{Path: source.GetPath(), TotalLines: uint32(max(source.GetTotalLines(), 0)), FileSize: uint32(max(source.GetFileSize(), 0)), ExceededLimit: source.GetTruncated()}
	if data, ok := source.Output.(*agentv1.ReadSuccess_Data); ok {
		result.Output = &agentv1.ReadToolSuccess_Data{Data: data.Data}
		return result
	}
	content := source.GetContent()
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	if content == "" {
		lines = nil
	}
	start, end := 0, len(lines)
	if offset := args.GetOffset(); offset > 0 {
		start = int(offset) - 1
	} else if offset < 0 {
		start = len(lines) + int(offset)
	}
	start = max(0, min(start, len(lines)))
	if limit := args.GetLimit(); limit > 0 {
		end = min(start+int(limit), len(lines))
	}
	numbered := args == nil || args.IncludeLineNumbers == nil || args.GetIncludeLineNumbers()
	selected := append([]string(nil), lines[start:end]...)
	if numbered {
		for i := range selected {
			selected[i] = fmt.Sprintf("%d|%s", start+i+1, selected[i])
		}
	}
	result.ReadRange = &agentv1.ReadRange{StartLine: uint32(start + 1), EndLine: uint32(end)}
	result.IncludeLineNumbers = &numbered
	result.IsEmpty = content == ""
	result.Output = &agentv1.ReadToolSuccess_Content{Content: strings.Join(selected, "\n")}
	return result
}
