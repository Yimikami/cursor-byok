package cursor

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const reactiveStorageKey = "src.vs.platform.reactivestorage.browser.reactiveStorageServiceImpl.persistentStorage.applicationUser"

// fakeProSessionJWT identifies the local-only token written by older releases.
// It is retained only to restore/clear legacy state and block upstream use.
// New sessions preserve real Cursor credentials and never create this token.
//
// Payload: {"sub":"fake-cursor-local-user","email":"cursor@ai.com",
//
//	"type":"session","iss":"cursor-client",
//	"scope":"openid profile email","exp":4070908800}
//
// Extracted byte-for-byte from the working closed-source app (Linux v0.0.4).
const fakeProSessionJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmYWtlLWN1cnNvci1sb2NhbC11c2VyIiwiZW1haWwiOiJjdXJzb3JAYWkuY29tIiwidHlwZSI6InNlc3Npb24iLCJpc3MiOiJjdXJzb3ItY2xpZW50Iiwic2NvcGUiOiJvcGVuaWQgcHJvZmlsZSBlbWFpbCIsImV4cCI6NDA3MDkwODgwMH0.fake-local-state-token"

const fakeProEmail = "cursor@ai.com"

// fakeAuthKeys lists the auth fields owned by older releases. Keep the list
// exact so migration restores only our previous overrides, leaving other
// user settings untouched.
var fakeAuthKeys = []string{
	"cursorAuth/accessToken",
	"cursorAuth/refreshToken",
	"cursorAuth/cachedEmail",
	"cursorAuth/cachedSignUpType",
	"cursorAuth/stripeMembershipType",
}

var stateMu sync.Mutex

// IsLocalSessionToken recognizes this application's local-only fallback.
// It must never be forwarded as a genuine credential to Cursor's services.
func IsLocalSessionToken(token string) bool {
	return strings.TrimSpace(token) == fakeProSessionJWT
}

// PrepareCursorAuth preserves every field of an existing Cursor account and
// requires a real sign-in. Older local-session backups are restored solely as
// a migration; new sessions never fabricate an account or subscription.
// Call only from explicit Start: a denied macOS Keychain read may request
// access once. Automatic recovery and request guards use noninteractive paths.
func PrepareCursorAuth(backupPath string) error {
	return prepareCursorAuthWithPermission(func() error {
		return PrepareCursorAuthWithoutInteraction(backupPath)
	}, RequestCursorKeychainAccess)
}

// PrepareCursorAuthWithoutInteraction performs the same account validation and
// legacy migration without displaying a Keychain dialog. A lifecycle caller can
// share one explicit permission attempt across recovery and this preflight.
func PrepareCursorAuthWithoutInteraction(backupPath string) error {
	return prepareCursorAuthAt(stateDBPath(), backupPath)
}

func prepareCursorAuthWithPermission(prepare, requestAccess func() error) error {
	var permission CursorKeychainPermissionAttempt
	return permission.Run(prepare, requestAccess)
}

// CursorKeychainPermissionAttempt limits one explicit Start action to one
// permission request, even when both legacy recovery and preflight need access.
// Use only for explicit user actions, never requests or automatic recovery.
// The zero value is ready to use; its caller must serialize calls to Run.
type CursorKeychainPermissionAttempt struct{ requested, granted bool }

func (p *CursorKeychainPermissionAttempt) Run(prepare, requestAccess func() error) error {
	err := prepare()
	if !cursorKeychainPermissionDenied(err) {
		return err
	}
	if p.requested {
		if p.granted {
			return cursorKeychainRetryError(err)
		}
		return err
	}
	// Only explicit access-denied/interaction-required statuses justify UI.
	// Missing items, malformed ciphertext and unavailable builds cannot be
	// repaired by asking for permission, and must not trigger a prompt.
	p.requested = true
	if err := requestAccess(); err != nil {
		return fmt.Errorf("Cursor Keychain access was not granted; existing state retained: %w", err)
	}
	p.granted = true
	// Never loop: denial or another failure on this retry remains visible.
	return cursorKeychainRetryError(prepare())
}

func cursorKeychainPermissionDenied(err error) bool {
	var keychainErr *CursorKeychainAccessError
	return errors.As(err, &keychainErr) && keychainErr != nil && (keychainErr.Status == -25293 || keychainErr.Status == -25308 || keychainErr.Status == -25315)
}

func cursorKeychainRetryError(err error) error {
	if !cursorKeychainPermissionDenied(err) {
		return err
	}
	return fmt.Errorf("Cursor Keychain access is still unavailable after approval; ongoing account ownership checks require Always Allow for cursor-byok's access to Cursor Safe Storage. Start again and choose Always Allow in the macOS dialog: %w", err)
}

func prepareCursorAuthAt(dbPath, backupPath string) error {
	if err := restoreFakeProUserAt(dbPath, backupPath); err != nil {
		return err
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
	var token string
	err = tx.QueryRow("SELECT value FROM ItemTable WHERE key = ?", "cursorAuth/accessToken").Scan(&token)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read current Cursor account: %w", err)
	}
	// The request guard must be able to compare the decrypted identity before
	// routing is enabled. Plain tokens pass through unchanged; unavailable
	// safeStorage support fails here instead of causing every request to 401.
	decoded, err := decodeCursorAuthToken(dbPath, token)
	if err != nil {
		return fmt.Errorf("cannot identify Cursor account; existing state retained: %w", err)
	}
	token = decoded
	if IsLocalSessionToken(token) {
		// An unbacked legacy fake is not an account we can recover. Remove
		// only its known owned fields so Cursor can perform a normal sign-in.
		for _, key := range fakeAuthKeys {
			if _, err := tx.Exec("DELETE FROM ItemTable WHERE key = ?", key); err != nil {
				return fmt.Errorf("remove legacy local session: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	} else if strings.TrimSpace(token) != "" {
		return nil
	}
	return errors.New("sign in to Cursor with your real account, then quit Cursor and start the service again")
}

// RestoreFakeProUser restores only the auth keys owned by this service. The
// entire restore is transactional, and its backup is retained on any failure.
func RestoreFakeProUser(backupPath string) error {
	return restoreFakeProUserAt(stateDBPath(), backupPath)
}

func restoreFakeProUserAt(dbPath, backupPath string) error {
	return restoreFakeProUser(dbPath, backupPath, os.Remove)
}

func restoreFakeProUser(dbPath, backupPath string, removeBackup func(string) error) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if backupPath == "" {
		return nil
	}
	backup, err := readAuthBackup(backupPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
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
	current, err := readCurrentAuth(tx)
	if err != nil {
		return err
	}
	owned := false
	// A completed restore followed by a failed sidecar deletion must never
	// replay old credentials over a later login (or logout). Only the injected
	// session owns this backup. The transaction also prevents partial restores.
	// Cursor now migrates these tokens to safeStorage after startup. Compare
	// the decoded value, never mistake an encrypted fake token for a new login.
	if !sameAuthValues(current, backup) {
		token := ""
		if value := current[fakeAuthKeys[0]]; value != nil {
			token, err = decodeCursorAuthToken(dbPath, *value)
			if err != nil {
				return fmt.Errorf("cannot identify current Cursor session; auth backup retained: %w", err)
			}
		}
		owned = IsLocalSessionToken(token)
		// Earlier nontransactional versions could restore the original access
		// token before failing on another row. Complete that exact-account repair.
		if original := backup[fakeAuthKeys[0]]; original != nil && current[fakeAuthKeys[0]] != nil && *original == *current[fakeAuthKeys[0]] {
			owned = true
		}
		if email := current[fakeAuthKeys[2]]; !owned && email != nil && *email == fakeProEmail {
			return fmt.Errorf("current Cursor session changed but still has the temporary account marker; auth backup retained")
		}
	}
	if owned {
		for _, key := range fakeAuthKeys {
			val := backup[key]
			if val == nil {
				if _, err := tx.Exec("DELETE FROM ItemTable WHERE key = ?", key); err != nil {
					return fmt.Errorf("restore delete %s: %w", key, err)
				}
				continue
			}
			if _, err := tx.Exec(
				"INSERT INTO ItemTable(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
				key, *val,
			); err != nil {
				return fmt.Errorf("restore upsert %s: %w", key, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := removeBackup(backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove restored auth backup: %w", err)
	}
	return nil
}

func readCurrentAuth(tx *sql.Tx) (map[string]*string, error) {
	values := make(map[string]*string, len(fakeAuthKeys))
	for _, key := range fakeAuthKeys {
		var value string
		switch err := tx.QueryRow("SELECT value FROM ItemTable WHERE key = ?", key).Scan(&value); err {
		case nil:
			values[key] = &value
		case sql.ErrNoRows:
			values[key] = nil
		default:
			return nil, fmt.Errorf("read current Cursor auth: %w", err)
		}
	}
	return values, nil
}

func sameAuthValues(current, backup map[string]*string) bool {
	for _, key := range fakeAuthKeys {
		a, b := current[key], backup[key]
		if (a == nil) != (b == nil) || (a != nil && *a != *b) {
			return false
		}
	}
	return true
}

func readAuthBackup(path string) (map[string]*string, error) {
	if path == "" {
		return nil, errors.New("auth backup path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var backup map[string]*string
	if err := json.Unmarshal(data, &backup); err != nil {
		return nil, fmt.Errorf("parse auth backup: %w", err)
	}
	if len(backup) != len(fakeAuthKeys) {
		return nil, errors.New("auth backup has unexpected or missing keys")
	}
	for _, key := range fakeAuthKeys {
		if _, ok := backup[key]; !ok {
			return nil, fmt.Errorf("auth backup is missing %s", key)
		}
	}
	return backup, nil
}

func openCursorDB(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("could not resolve Cursor state.vscdb path")
	}
	if info, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open Cursor state.vscdb (open Cursor once to initialize it): %w", err)
	} else if info.IsDir() {
		return nil, errors.New("Cursor state.vscdb is a directory")
	}
	return sql.Open("sqlite", path+"?_pragma=busy_timeout=2000")
}

// Persist and sync the complete backup before publishing its final name.
// A no-replace hard link preserves the first snapshot across repeated starts;
// a crash during the write can leave only an ignored temporary file.
func writeStateBackup(path string, value any) error {
	return writeStateBackupWithPublish(path, value, os.Link)
}

func writeStateBackupWithPublish(path string, value any, publish func(string, string) error) error {
	if path == "" {
		return errors.New("backup path is required")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("persist backup: %w", err)
	}
	if err := publish(temporary, path); err != nil {
		return fmt.Errorf("publish backup: %w", err)
	}
	// Directory fsync makes the published link durable before SQLite changes.
	// Windows does not support File.Sync on an open directory handle.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		syncErr := dir.Sync()
		if err := errors.Join(syncErr, dir.Close()); err != nil {
			return fmt.Errorf("sync backup directory: %w", err)
		}
	}
	return nil
}

func removeStateBackup(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove restored backup: %w", err)
	}
	return nil
}

func decodeApplicationUser(data []byte) (map[string]any, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("applicationUser JSON parse: %w", err)
	}
	if value == nil {
		return nil, errors.New("applicationUser must be a JSON object")
	}
	// Unmarshal validates that there is no trailing JSON without converting
	// numbers in the result, preserving unrelated large integer preferences.
	if !json.Valid(data) {
		return nil, errors.New("applicationUser is not valid JSON")
	}
	return value, nil
}

func rewriteAiSettings(d map[string]any, hexID string) {
	ai, _ := d["aiSettings"].(map[string]any)
	if ai == nil {
		ai = map[string]any{}
		d["aiSettings"] = ai
	}

	mc, _ := ai["modelConfig"].(map[string]any)
	if mc == nil {
		mc = map[string]any{}
		ai["modelConfig"] = mc
	}

	// Pin only locally executed features. Cloud background agents retain
	// their native account-supported selections, including an absent setting.
	features := []string{"cmd-k", "composer", "composer-ensemble", "plan-execution", "spec", "deep-search", "quick-agent"}
	for _, f := range features {
		entry := ensurePreferenceObject(mc, f)
		entry["modelName"] = hexID
		entry["maxMode"] = false
		entry["selectedModels"] = nil
		if f == "composer" {
			entry["selectedModels"] = []any{
				map[string]any{
					"modelId":    hexID,
					"parameters": []any{},
				},
			}
		}
		mc[f] = entry
	}

	ai["modelOverrideEnabled"] = []any{}
	ai["modelsWithNoDefaultSwitch"] = []any{hexID}
	ai["modelDefaultSwitchOnNewChat"] = false

	prev, _ := ai["previousModelBeforeDefault"].(map[string]any)
	if prev == nil {
		prev = map[string]any{}
		ai["previousModelBeforeDefault"] = prev
	}
	for _, f := range []string{"cmd-k", "composer", "composer-ensemble", "plan-execution", "spec", "deep-search", "quick-agent"} {
		prev[f] = hexID
	}
}

func rewriteFeatureModelConfigs(d map[string]any, hexID string) {
	fmc, _ := d["featureModelConfigs"].(map[string]any)
	if fmc == nil {
		fmc = map[string]any{}
		d["featureModelConfigs"] = fmc
	}
	hexList := []any{hexID}
	pinFull := func(name string, withBestOfN bool) {
		entry := ensurePreferenceObject(fmc, name)
		entry["defaultModel"] = hexID
		entry["fallbackModels"] = hexList
		if withBestOfN {
			entry["bestOfNDefaultModels"] = hexList
		} else {
			entry["bestOfNDefaultModels"] = []any{}
		}
		fmc[name] = entry
	}
	pinDefaultOnly := func(name string) {
		entry := ensurePreferenceObject(fmc, name)
		entry["defaultModel"] = hexID
		entry["fallbackModels"] = []any{}
		entry["bestOfNDefaultModels"] = []any{}
	}
	pinFull("composer", true)
	pinFull("cmdK", false)
	pinFull("planExecution", false)
	pinDefaultOnly("spec")
	pinDefaultOnly("deepSearch")
	pinDefaultOnly("quickAgent")
}

func stateDBPath() string {
	if root, set := cursorDataDirOverride(); set {
		if root == "" {
			return ""
		}
		return filepath.Join(root, "User", "globalStorage", "state.vscdb")
	}
	switch runtime.GOOS {
	case "windows":
		root := os.Getenv("APPDATA")
		if root == "" {
			return ""
		}
		return filepath.Join(root, "Cursor", "User", "globalStorage", "state.vscdb")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
}
