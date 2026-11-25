package bridge

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"cursor-byok/internal/agent"
)

const modelPickerCatalogFile = "models-public.json"
const modelPickerSelectionFile = "cursor-model-selection.json"

// This is the only configuration the editor companion reads. Never publish
// credentials, provider URLs, account information or the full UserConfig.
func publishModelPickerCatalog(dir string, config UserConfig) error {
	type model struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		Provider string `json:"provider"`
	}
	catalog := struct {
		Version        int     `json:"version"`
		DefaultModelID string  `json:"defaultModelID"`
		Models         []model `json:"models"`
	}{Version: 1, Models: []model{}}
	seen := map[string]bool{}
	for _, adapter := range adapterListFromConfig(config) {
		if seen[adapter.ModelID] {
			continue
		}
		seen[adapter.ModelID] = true
		label := adapter.DisplayName
		if label == "" {
			label = adapter.ModelID
		}
		catalog.Models = append(catalog.Models, model{adapter.ModelID, label, adapter.Type})
		if catalog.DefaultModelID == "" {
			catalog.DefaultModelID = adapter.ModelID
		}
	}
	body, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, modelPickerCatalogFile)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, body) {
		return nil
	}
	file, err := os.CreateTemp(dir, ".models-public-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err = file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// The companion owns this small, secret-free selection file. It never rewrites
// config.json, so selecting a model cannot lose concurrent dashboard edits.
func applyNativeModelSelection(dir string, targets []agent.AdapterTarget) {
	file, err := os.Open(filepath.Join(dir, modelPickerSelectionFile))
	if err != nil {
		return
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(body) > 4096 {
		return
	}
	var selection struct {
		Version int    `json:"version"`
		ModelID string `json:"modelID"`
	}
	if json.Unmarshal(body, &selection) != nil || selection.Version != 1 || selection.ModelID == "" {
		return
	}
	for index := range targets {
		if targets[index].Model == selection.ModelID {
			targets[index].NativeSelectionOverride = true
			return
		}
	}
}
