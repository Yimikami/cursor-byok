package agent

import "testing"

// Regression for the actual Cursor error: "The server sent no per-mode tool
// decisions ... which Plan, Ask and Debug turns need." The client maps Ask to
// the string "chat"; sending an "ask" row would still fail its mode lookup.
func TestInferenceRunReadyProvidesCurrentCursorModes(t *testing.T) {
	ready := providerTestFields(t, inferenceRunReady(nil, AdapterTarget{Model: "test-model", StableID: "test-id"}))
	metadata := providerTestFields(t, ready.lastBytes(4))
	rows := map[string]providerWireFields{}
	for _, raw := range metadata.bytes(46) {
		row := providerTestFields(t, raw)
		id := string(row.lastBytes(1))
		if _, exists := rows[id]; exists {
			t.Fatalf("duplicate mode %q", id)
		}
		rows[id] = row
	}
	for _, mode := range []string{"agent", "chat", "plan", "debug", "triage", "multitask"} {
		row, ok := rows[mode]
		if !ok {
			t.Fatalf("Cursor cannot start mode %q without its RunReady row", mode)
		}
		if row.varint(2) != 1 {
			t.Errorf("mode %q cannot ask clarifying questions", mode)
		}
		targets := providerTestFields(t, row.lastBytes(3))
		for _, raw := range targets.bytes(1) {
			target := string(raw)
			if _, ok := rows[target]; !ok || target == mode {
				t.Errorf("invalid transition %s -> %s", mode, target)
			}
		}
	}
	if _, wrongID := rows["ask"]; wrongID {
		t.Fatal("Ask must use the client wire id chat")
	}
	question := providerTestFields(t, metadata.lastBytes(48))
	if string(question.lastBytes(1)) != "AskQuestion" || question.varint(2) != 0 {
		t.Fatal("AskQuestion must retain its native name without rejecting the first question")
	}
	if metadata.varint(47) != 1 {
		t.Fatal("current Plan prompts must be selected")
	}
}
