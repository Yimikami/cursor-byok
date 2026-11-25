package agent

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// Validate complete provider batches before either the native inference bridge
// or the legacy agent loop can execute any of their calls.
func validateProviderToolCalls(calls []PendingToolCall) error {
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
			return errors.New("provider tool call is missing its ID or name")
		}
		if seen[call.ID] {
			return fmt.Errorf("provider returned duplicate tool call ID %q", call.ID)
		}
		seen[call.ID] = true
		args := strings.TrimSpace(call.Arguments)
		if args == "" {
			args = "{}"
		}
		var object structpb.Struct
		if err := protojson.Unmarshal([]byte(args), &object); err != nil {
			return fmt.Errorf("provider tool %s returned invalid object arguments: %w", call.Name, err)
		}
	}
	return nil
}
