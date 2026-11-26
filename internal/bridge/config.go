package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
)

// All config readers and writers share this lock, including the routing
// callbacks that do not have a ProxyService receiver. Keep it separate from
// the lifecycle lock so saving never waits for a proxy shutdown or a probe.
var configMu sync.RWMutex

// writeConfigLocked publishes a complete snapshot without truncating the live
// file. The same-directory temporary file is private because it contains keys.
// Callers hold configMu, including across read/modify/write transactions. This
// also closes local read handles before replacement on Windows.
func writeConfigLocked(dir string, cfg UserConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, "config.json"))
}

func (s *ProxyService) saveAdapterTestResult(index int, tested, result ModelAdapterConfig) error {
	configMu.Lock()
	defer configMu.Unlock()
	cfg, err := readConfigLocked(s.cfgDir)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(cfg.ModelAdapters) {
		return nil // The user removed the adapter while its network probe ran.
	}
	current := cfg.ModelAdapters[index]
	current.LastTestedAt, tested.LastTestedAt = 0, 0
	current.LastTestResult, tested.LastTestResult = "", ""
	if !reflect.DeepEqual(current, tested) {
		return nil // Do not report a stale probe against an edited/reordered row.
	}
	cfg.ModelAdapters[index].LastTestedAt = result.LastTestedAt
	cfg.ModelAdapters[index].LastTestResult = result.LastTestResult
	return writeConfigLocked(s.cfgDir, cfg)
}

// Adapter identity belongs to the configuration, not the provider model name.
// It survives rename/reorder/key rotation and distinguishes identical model IDs
// on different accounts/providers without putting a credential in a selector.
func ensureAdapterIDs(cfg *UserConfig) (bool, error) {
	changed := false
	seen := map[string]bool{}
	for i := range cfg.ModelAdapters {
		a := &cfg.ModelAdapters[i]
		if a.ID == "" || seen[a.ID] {
			var raw [16]byte
			if _, err := rand.Read(raw[:]); err != nil {
				return false, err
			}
			a.ID = hex.EncodeToString(raw[:])
			changed = true
		}
		seen[a.ID] = true
	}
	// Preserve the previously effective adapter when migrating the old model-only selection.
	if cfg.ReviewModelID != "" && !strings.HasPrefix(cfg.ReviewModelID, "adapter:") {
		requested := strings.TrimSpace(cfg.ReviewModelID)
		adapters := adapterListFromConfig(*cfg)
		if len(adapters) > 0 {
			selected := adapters[0]
			for _, a := range adapters {
				if strings.EqualFold(requested, a.ModelID) || strings.EqualFold(requested, a.DisplayName) || strings.EqualFold(requested, a.StableID()) {
					selected = a
					break
				}
			}
			cfg.ReviewModelID = "adapter:" + selected.ConfigID
			changed = true
		}
	}
	return changed, nil
}
