package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cursor-byok/internal/agent"
)

func TestModelPickerCatalogAndLiveSelection(t *testing.T) {
	dir := t.TempDir()
	config := UserConfig{ActiveModelID: "second", ModelAdapters: []ModelAdapterConfig{
		{ModelID: "first", DisplayName: "First model", APIKey: "fixture-private-key", BaseURL: "https://private.invalid"},
		{ModelID: "second", DisplayName: "Second model", Type: "openai"},
	}}
	if err := publishModelPickerCatalog(dir, config); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, modelPickerCatalogFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "private") || strings.Contains(string(body), "apiKey") || strings.Contains(string(body), "baseURL") {
		t.Fatal("private configuration leaked into editor catalog")
	}
	var catalog struct {
		DefaultModelID string `json:"defaultModelID"`
		Models         []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil || catalog.DefaultModelID != "second" || len(catalog.Models) != 2 {
		t.Fatalf("catalog: %s err=%v", body, err)
	}
	// The resolver reads the file for each request, without stopping the proxy.
	for _, selection := range []string{"second", "first", "", "removed"} {
		body, _ := json.Marshal(map[string]any{"version": 1, "modelID": selection})
		if err := os.WriteFile(filepath.Join(dir, modelPickerSelectionFile), body, 0600); err != nil {
			t.Fatal(err)
		}
		targets := []agent.AdapterTarget{{Model: "first"}, {Model: "second"}}
		applyNativeModelSelection(dir, targets)
		for _, target := range targets {
			if target.NativeSelectionOverride != (target.Model == selection) {
				t.Fatalf("selection %q: %+v", selection, targets)
			}
		}
	}
	for _, invalid := range []string{`{"version":2,"modelID":"first"}`, `{"version":1,"modelID":`, strings.Repeat(" ", 4097)} {
		if err := os.WriteFile(filepath.Join(dir, modelPickerSelectionFile), []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		targets := []agent.AdapterTarget{{Model: "first"}}
		applyNativeModelSelection(dir, targets)
		if targets[0].NativeSelectionOverride {
			t.Fatal("invalid selection enabled override")
		}
	}
	// Removed adapters disappear on the next save; selection cannot invent one.
	config.ModelAdapters = config.ModelAdapters[:1]
	if err := publishModelPickerCatalog(dir, config); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, modelPickerCatalogFile))
	if strings.Contains(string(body), "second") {
		t.Fatal("removed adapter remains in catalog")
	}
}
