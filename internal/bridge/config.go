package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
