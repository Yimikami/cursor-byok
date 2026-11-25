package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "cursor-byok/internal/protocodec/gen/agent/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMcpCurrentRequestSchemaWinsAndFallbackFillsMissingMetadata(t *testing.T) {
	makeSchema := func(property string) *structpb.Value {
		properties := map[string]any{}
		if property != "" {
			properties[property] = map[string]any{"type": "string"}
		}
		value, err := structpb.NewValue(map[string]any{"type": "object", "properties": properties, "additionalProperties": false})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, tt := range []struct {
		name          string
		requestSchema *structpb.Value
		fileProperty  string
		wantProperty  string
	}{
		{"current rich schema", makeSchema("current_argument"), "file_argument", "current_argument"},
		{"current file schema beats run", nil, "file_argument", "file_argument"},
		{"null rich schema uses current file", structpb.NewNullValue(), "file_argument", "file_argument"},
		{"run fills missing request schema", nil, "", "old_argument"},
		{"explicit current no-argument schema wins", makeSchema(""), "file_argument", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			definition := filepath.Join(t.TempDir(), "lookup.json")
			if tt.fileProperty != "" {
				schema, _ := makeSchema(tt.fileProperty).MarshalJSON()
				body, err := json.Marshal(map[string]any{"description": "FILE_DESCRIPTION", "inputSchema": json.RawMessage(schema)})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(definition, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			request := &agentv1.RequestContext{
				Tools: []*agentv1.McpToolDefinition{{ProviderIdentifier: "provider-id", ToolName: "lookup", Description: "CURRENT_DESCRIPTION", InputSchema: tt.requestSchema}},
				McpFileSystemOptions: &agentv1.McpFileSystemOptions{McpDescriptors: []*agentv1.McpDescriptor{{
					ServerIdentifier: "provider-id", ServerName: "Friendly server", Tools: []*agentv1.McpToolDescriptor{{ToolName: "lookup", DefinitionPath: &definition}},
				}}},
			}
			sess := &Session{
				Action: &agentv1.ConversationAction{Action: &agentv1.ConversationAction_UserMessageAction{UserMessageAction: &agentv1.UserMessageAction{RequestContext: request}}},
				Run: &agentv1.AgentRunRequest{McpTools: &agentv1.McpTools{McpTools: []*agentv1.McpToolDefinition{{
					ProviderIdentifier: "provider-id", ToolName: "lookup", Description: "STALE_DESCRIPTION", InputSchema: makeSchema("old_argument"),
				}}}},
			}
			tools := openAIToolsForRequest(sess)
			if len(sess.McpMap) != 1 {
				t.Fatalf("duplicate tools retained: %#v", sess.McpMap)
			}
			found := false
			for _, tool := range tools {
				var fn struct {
					Name, Description string
					Parameters        struct {
						Properties           map[string]any
						AdditionalProperties bool
					}
				}
				if err := json.Unmarshal(tool.Function, &fn); err != nil {
					t.Fatal(err)
				}
				ref := sess.McpMap[fn.Name]
				if ref == nil {
					continue
				}
				found = true
				if ref.ServerName != "Friendly server" || ref.ServerID != "provider-id" || !strings.Contains(fn.Description, "CURRENT_DESCRIPTION") || strings.Contains(fn.Description, "STALE_DESCRIPTION") {
					t.Fatalf("wrong metadata precedence: %s, %+v", tool.Function, ref)
				}
				if fn.Parameters.AdditionalProperties {
					t.Fatal("lost schema constraints")
				}
				if tt.wantProperty == "" {
					if len(fn.Parameters.Properties) != 0 {
						t.Fatalf("no-argument schema was overwritten: %s", tool.Function)
					}
				} else if _, ok := fn.Parameters.Properties[tt.wantProperty]; !ok || len(fn.Parameters.Properties) != 1 {
					t.Fatalf("expected %s schema: %s", tt.wantProperty, tool.Function)
				}
			}
			if !found {
				t.Fatal("MCP tool was not exposed")
			}
		})
	}
}
