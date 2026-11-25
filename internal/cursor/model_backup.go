package cursor

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const modelRestoreCompletedKey = "cursor-byok/modelRestoreCompleted"

type modelPreferenceBackup struct {
	Version        int               `json:"version"`
	Values         []modelPreference `json:"values"`
	CreatedObjects [][]string        `json:"createdObjects,omitempty"`
}

type modelPreference struct {
	Path    []string        `json:"path"`
	Present bool            `json:"present"`
	Value   json.RawMessage `json:"value,omitempty"`
}

func legacyModelPreferencePaths(includeBackground bool) [][]string {
	var paths [][]string
	features := []string{"cmd-k", "composer", "composer-ensemble", "plan-execution", "spec", "deep-search", "quick-agent"}
	for _, feature := range features {
		for _, key := range []string{"modelName", "maxMode", "selectedModels"} {
			paths = append(paths, []string{"aiSettings", "modelConfig", feature, key})
		}
		paths = append(paths, []string{"aiSettings", "previousModelBeforeDefault", feature})
	}
	if includeBackground {
		for _, key := range []string{"modelName", "maxMode", "selectedModels"} {
			paths = append(paths, []string{"aiSettings", "modelConfig", "background-composer", key})
		}
	}
	for _, key := range []string{"modelOverrideEnabled", "modelsWithNoDefaultSwitch", "modelDefaultSwitchOnNewChat"} {
		paths = append(paths, []string{"aiSettings", key})
	}
	for _, feature := range []string{"composer", "cmdK", "backgroundComposer", "planExecution", "spec", "deepSearch", "quickAgent"} {
		for _, key := range []string{"defaultModel", "fallbackModels", "bestOfNDefaultModels"} {
			paths = append(paths, []string{"featureModelConfigs", feature, key})
		}
	}
	return paths
}

func captureModelPreferences(d map[string]any) (modelPreferenceBackup, error) {
	bk := modelPreferenceBackup{Version: 2}
	created := map[string]bool{}
	for _, path := range modelPreferencePaths() {
		entry := modelPreference{Path: path}
		parent := d
		for i, key := range path[:len(path)-1] {
			value, exists := parent[key]
			if !exists {
				for length := i + 1; length < len(path); length++ {
					prefix := path[:length]
					id := strings.Join(prefix, "/")
					if !created[id] {
						created[id] = true
						bk.CreatedObjects = append(bk.CreatedObjects, prefix)
					}
				}
				parent = nil
				break
			}
			var ok bool
			parent, ok = value.(map[string]any)
			if !ok {
				return bk, fmt.Errorf("model preference %s must be an object", strings.Join(path[:i+1], "/"))
			}
		}
		if parent != nil {
			value, exists := parent[path[len(path)-1]]
			entry.Present = exists
			if exists {
				var err error
				entry.Value, err = json.Marshal(value)
				if err != nil {
					return bk, err
				}
			}
		}
		bk.Values = append(bk.Values, entry)
	}
	return bk, nil
}

func modelPreferencePaths() [][]string {
	var paths [][]string
	for _, path := range legacyModelPreferencePaths(false) {
		if path[0] == "featureModelConfigs" && path[1] == "backgroundComposer" {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

func readModelBackup(path string) (modelPreferenceBackup, error) {
	var bk modelPreferenceBackup
	if path == "" {
		return bk, errors.New("model backup path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return bk, err
	}
	if err := json.Unmarshal(data, &bk); err != nil {
		return bk, fmt.Errorf("parse model backup: %w", err)
	}
	if bk.Version != 1 && bk.Version != 2 {
		return bk, errors.New("unsupported model backup version")
	}
	allowedPaths, requiredPaths := modelPreferencePaths(), modelPreferencePaths()
	if bk.Version == 1 {
		// Earlier snapshots owned cloud preferences. Restore those old writes
		// during migration, but never capture them in a new local-only session.
		allowedPaths, requiredPaths = legacyModelPreferencePaths(true), legacyModelPreferencePaths(false)
	}
	allowed, required, parents := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, path := range allowedPaths {
		allowed[strings.Join(path, "/")] = true
		for length := 1; length < len(path); length++ {
			parents[strings.Join(path[:length], "/")] = true
		}
	}
	for _, path := range requiredPaths {
		required[strings.Join(path, "/")] = true
	}
	seen := map[string]bool{}
	for _, entry := range bk.Values {
		id := strings.Join(entry.Path, "/")
		if !validModelPathSegments(entry.Path) || !allowed[id] || seen[id] {
			return bk, fmt.Errorf("unexpected or duplicate model backup path %q", id)
		}
		if entry.Present && !json.Valid(entry.Value) || !entry.Present && len(entry.Value) != 0 {
			return bk, fmt.Errorf("invalid backed-up value for %s", id)
		}
		seen[id] = true
	}
	for id := range required {
		if !seen[id] {
			return bk, fmt.Errorf("model backup is missing %s", id)
		}
	}
	if len(seen) != len(required) && len(seen) != len(allowed) {
		return bk, errors.New("incomplete background model backup")
	}
	seen = map[string]bool{}
	for _, path := range bk.CreatedObjects {
		id := strings.Join(path, "/")
		if !validModelPathSegments(path) || !parents[id] || seen[id] {
			return bk, fmt.Errorf("invalid created model object %q", id)
		}
		seen[id] = true
	}
	return bk, nil
}

func validModelPathSegments(path []string) bool {
	for _, part := range path {
		if part == "" || strings.Contains(part, "/") {
			return false
		}
	}
	return len(path) > 0
}

// ForceModelSelection snapshots only the preference fields it changes. The
// optional path keeps callers using the normal user config directory working.
func ForceModelSelection(byokHexID string, backupPaths ...string) error {
	var path string
	if len(backupPaths) > 0 {
		path = backupPaths[0]
	} else {
		dir, err := configDir()
		if err != nil {
			return err
		}
		path = filepath.Join(dir, ProfileBackupName("cursor-model-backup.json"))
	}
	return forceModelSelectionAt(stateDBPath(), path, byokHexID, byokHexID)
}

// ForceModelSelectionForAdapters also repairs the exact cloud-model pin left
// by releases that predated model backups. Only IDs belonging to currently
// configured adapters are eligible; native or mixed cloud choices survive.
func ForceModelSelectionForAdapters(byokHexID, backupPath string, knownAdapterIDs []string) error {
	return forceModelSelectionAt(stateDBPath(), backupPath, byokHexID, knownAdapterIDs...)
}

func clearLegacyCloudModelPin(d map[string]any, knownAdapterIDs []string) {
	features, _ := d["featureModelConfigs"].(map[string]any)
	cloud, _ := features["backgroundComposer"].(map[string]any)
	id, _ := cloud["defaultModel"].(string)
	known := false
	for _, candidate := range knownAdapterIDs {
		if id != "" && id == candidate {
			known = true
			break
		}
	}
	if !known {
		return
	}
	// Require the prior local composer pin too, so a deliberate cloud-only
	// setting is not misclassified as the old application's global override.
	ai, _ := d["aiSettings"].(map[string]any)
	models, _ := ai["modelConfig"].(map[string]any)
	composer, _ := models["composer"].(map[string]any)
	if composer["modelName"] != id {
		return
	}
	for _, key := range []string{"fallbackModels", "bestOfNDefaultModels"} {
		models, ok := cloud[key].([]any)
		if !ok || len(models) != 1 || models[0] != id {
			return
		}
	}
	for _, key := range []string{"defaultModel", "fallbackModels", "bestOfNDefaultModels"} {
		delete(cloud, key)
	}
	if len(cloud) == 0 {
		delete(features, "backgroundComposer")
	}
}

func forceModelSelectionAt(dbPath, backupPath, byokHexID string, knownAdapterIDs ...string) error {
	if len(knownAdapterIDs) == 0 {
		knownAdapterIDs = []string{byokHexID}
	}
	if byokHexID == "" {
		return nil
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	db, err := openCursorDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	if err := tx.QueryRow("SELECT value FROM ItemTable WHERE key = ?", reactiveStorageKey).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	d, err := decodeApplicationUser(raw)
	if err != nil {
		return err
	}
	// The old release had no cloud preference backup to restore. Remove only
	// its recognizable local-ID triple before capturing today's local fields.
	clearLegacyCloudModelPin(d, knownAdapterIDs)
	snapshot, err := captureModelPreferences(d)
	if err != nil {
		return err
	}
	if _, err := readModelBackup(backupPath); errors.Is(err, os.ErrNotExist) {
		if err := writeStateBackup(backupPath, snapshot); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	rewriteAiSettings(d, byokHexID)
	rewriteFeatureModelConfigs(d, byokHexID)
	if err := writeApplicationUser(tx, d); err != nil {
		return err
	}
	// Invalidate completion in the same transaction as the new override,
	// including when two sessions happen to have identical snapshots.
	if _, err := tx.Exec("DELETE FROM ItemTable WHERE key = ?", modelRestoreCompletedKey); err != nil {
		return err
	}
	return tx.Commit()
}

func RestoreModelSelection(backupPath string) error {
	return restoreModelSelectionAt(stateDBPath(), backupPath)
}

func restoreModelSelectionAt(dbPath, backupPath string) error {
	return restoreModelSelectionWithRemoveAt(dbPath, backupPath, removeStateBackup)
}

func restoreModelSelectionWithRemoveAt(dbPath, backupPath string, removeBackup func(string) error) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	bk, err := readModelBackup(backupPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(bk)
	if err != nil {
		return err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(encoded))
	db, err := openCursorDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var completed string
	if err := tx.QueryRow("SELECT value FROM ItemTable WHERE key = ?", modelRestoreCompletedKey).Scan(&completed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if completed == fingerprint {
		// The previous transaction restored preferences successfully. A
		// retained sidecar only needs deletion; never overwrite later edits.
		if err := tx.Commit(); err != nil {
			return err
		}
		return removeBackup(backupPath)
	}
	var raw []byte
	if err := tx.QueryRow("SELECT value FROM ItemTable WHERE key = ?", reactiveStorageKey).Scan(&raw); err != nil {
		return err
	}
	d, err := decodeApplicationUser(raw)
	if err != nil {
		return err
	}
	for _, entry := range bk.Values {
		parent := d
		for _, key := range entry.Path[:len(entry.Path)-1] {
			value, exists := parent[key]
			if !exists {
				if !entry.Present {
					parent = nil
					break
				}
				value = map[string]any{}
				parent[key] = value
			}
			var ok bool
			parent, ok = value.(map[string]any)
			if !ok {
				return fmt.Errorf("cannot restore model preference %s: parent is no longer an object", strings.Join(entry.Path, "/"))
			}
		}
		if parent == nil {
			continue
		}
		key := entry.Path[len(entry.Path)-1]
		if entry.Present {
			// RawMessage keeps saved numeric values exact when re-encoded.
			parent[key] = entry.Value
		} else {
			delete(parent, key)
		}
	}
	// Only prune containers this service created, and only when empty: a
	// preference added by Cursor while BYOK was running must survive Stop.
	sort.SliceStable(bk.CreatedObjects, func(i, j int) bool { return len(bk.CreatedObjects[i]) > len(bk.CreatedObjects[j]) })
	for _, path := range bk.CreatedObjects {
		parent := d
		for _, key := range path[:len(path)-1] {
			parent, _ = parent[key].(map[string]any)
		}
		key := path[len(path)-1]
		if value, ok := parent[key].(map[string]any); ok && len(value) == 0 {
			delete(parent, key)
		}
	}
	if err := writeApplicationUser(tx, d); err != nil {
		return err
	}
	// Completion must commit atomically with the restored preferences. A
	// failed commit retries both; a failed sidecar removal retries only that.
	if _, err := tx.Exec("INSERT INTO ItemTable(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", modelRestoreCompletedKey, fingerprint); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return removeBackup(backupPath)
}

func writeApplicationUser(tx *sql.Tx, d map[string]any) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE ItemTable SET value = ? WHERE key = ?", string(data), reactiveStorageKey)
	return err
}

func ensurePreferenceObject(parent map[string]any, key string) map[string]any {
	value, _ := parent[key].(map[string]any)
	if value == nil {
		value = map[string]any{}
		parent[key] = value
	}
	return value
}
