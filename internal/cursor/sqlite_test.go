package cursor

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareCursorAuthPreservesRealAccountWithoutBackup(t *testing.T) {
	original := map[string]string{
		"cursorAuth/accessToken": "real-session", "cursorAuth/refreshToken": "real-refresh",
		"cursorAuth/cachedEmail": "user@example.test", "cursorAuth/cachedSignUpType": "Google",
		"cursorAuth/stripeMembershipType": "free", "unrelated": "keep",
	}
	db, path, backup := testCursorDB(t, original)
	if _, err := db.Exec("CREATE TRIGGER deny_any_auth_write BEFORE INSERT ON ItemTable BEGIN SELECT RAISE(ABORT, 'must not write'); END"); err != nil {
		t.Fatal(err)
	}
	if err := prepareCursorAuthAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, original) {
		t.Fatalf("changed real account: got=%#v want=%#v", got, original)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("real account should not need an auth backup: %v", err)
	}
}

func TestPrepareCursorAuthMigratesLegacyBackup(t *testing.T) {
	original := map[string]string{
		"cursorAuth/accessToken": "real-session", "cursorAuth/refreshToken": "real-refresh",
		"cursorAuth/stripeMembershipType": "free", "unrelated": "keep",
	}
	db, path, backup := testCursorDB(t, original)
	if err := injectFakeProUserAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := prepareCursorAuthAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, original) {
		t.Fatalf("legacy account not restored: %#v", got)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("completed legacy backup retained: %v", err)
	}
}

func TestPrepareCursorAuthRequiresSignInWithoutCreatingFakeState(t *testing.T) {
	for _, legacyFake := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacyFake), func(t *testing.T) {
			original := map[string]string{"unrelated": "keep"}
			if legacyFake {
				for _, key := range fakeAuthKeys {
					original[key] = "legacy-owned-value"
				}
				original["cursorAuth/accessToken"] = fakeProSessionJWT
			}
			db, path, backup := testCursorDB(t, original)
			err := prepareCursorAuthAt(path, backup)
			if err == nil || !strings.Contains(err.Error(), "sign in to Cursor") {
				t.Fatalf("expected actionable sign-in error, got %v", err)
			}
			if got := readTestItems(t, db); !reflect.DeepEqual(got, map[string]string{"unrelated": "keep"}) {
				t.Fatalf("did not preserve signed-out state: %#v", got)
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Fatalf("signed-out state created a backup: %v", err)
			}
		})
	}
}

func TestLocalSessionRecognition(t *testing.T) {
	if !IsLocalSessionToken(fakeProSessionJWT) || !IsLocalSessionToken(" "+fakeProSessionJWT+" ") {
		t.Fatal("legacy local credential was not recognized")
	}
	for _, token := range []string{"", "genuine-token", fakeProSessionJWT + "-different"} {
		if IsLocalSessionToken(token) {
			t.Fatalf("nonlocal credential recognized: %q", token)
		}
	}
}

func testCursorDB(t *testing.T, values map[string]string) (*sql.DB, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	for key, value := range values {
		if _, err := db.Exec("INSERT INTO ItemTable VALUES (?, ?)", key, value); err != nil {
			t.Fatal(err)
		}
	}
	return db, path, filepath.Join(dir, "backup.json")
}

func readTestItems(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query("SELECT key, value FROM ItemTable")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func readTestPreferences(t *testing.T, db *sql.DB) map[string]any {
	t.Helper()
	d, err := decodeApplicationUser([]byte(readTestItems(t, db)[reactiveStorageKey]))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func writeTestPreferences(t *testing.T, db *sql.DB, d map[string]any) {
	t.Helper()
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE ItemTable SET value = ? WHERE key = ?", string(data), reactiveStorageKey); err != nil {
		t.Fatal(err)
	}
}

func TestModelSelectionRestoresOnlyOwnedFields(t *testing.T) {
	original := `{
		"unrelated": {"largeNumber": 9007199254740993},
		"aiSettings": {
			"modelConfig": {
				"composer": {"modelName":"original","maxMode":true,"selectedModels":[{"modelId":"original","parameters":["high"]}],"temperature":0.2},
				"other-feature": {"modelName":"leave-alone"},
				"background-composer": {"modelName":"background-original","custom":true}
			},
			"previousModelBeforeDefault": {"composer":"previous","other-feature":"keep"},
			"modelOverrideEnabled":["original"], "modelsWithNoDefaultSwitch":null,
			"modelDefaultSwitchOnNewChat":true, "theme":"dark"
		},
		"featureModelConfigs": {
			"composer":{"defaultModel":"original","fallbackModels":["fallback"],"bestOfNDefaultModels":["best"],"vendorHint":"keep"},
			"otherFeature":{"defaultModel":"untouched"}
		}
	}`
	db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: original, "unrelated-row": "keep"})
	if err := forceModelSelectionAt(path, backup, "byok-one"); err != nil {
		t.Fatal(err)
	}
	firstBackup, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := forceModelSelectionAt(path, backup, "byok-two"); err != nil {
		t.Fatal(err)
	}
	secondBackup, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBackup) != string(secondBackup) {
		t.Fatal("second start replaced original model snapshot")
	}

	current := readTestPreferences(t, db)
	ai := current["aiSettings"].(map[string]any)
	composer := ai["modelConfig"].(map[string]any)["composer"].(map[string]any)
	if composer["temperature"] != json.Number("0.2") || composer["modelName"] != "byok-two" {
		t.Fatalf("pinning clobbered extra fields: %#v", composer)
	}
	applyUnrelatedEdits := func(d map[string]any) {
		d["newPreference"] = "created while running"
		ai := d["aiSettings"].(map[string]any)
		ai["theme"] = "light"
		composer := ai["modelConfig"].(map[string]any)["composer"].(map[string]any)
		composer["temperature"] = json.Number("0.7")
		composer["newCustomField"] = "keep this too"
		d["featureModelConfigs"].(map[string]any)["composer"].(map[string]any)["vendorHint"] = "updated"
	}
	applyUnrelatedEdits(current)
	writeTestPreferences(t, db, current)
	if err := restoreModelSelectionAt(path, backup); err != nil {
		t.Fatal(err)
	}
	want, err := decodeApplicationUser([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	applyUnrelatedEdits(want)
	if got := readTestPreferences(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored preferences:\n%#v\nwant:\n%#v", got, want)
	}
	if readTestItems(t, db)["unrelated-row"] != "keep" {
		t.Fatal("unrelated SQLite row changed")
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup not removed: %v", err)
	}
}

func TestModelPinNeverOwnsCloudBackgroundPreferences(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%v", present), func(t *testing.T) {
			original := map[string]any{"aiSettings": map[string]any{"modelConfig": map[string]any{}}, "featureModelConfigs": map[string]any{}}
			background := map[string]any{"modelName": "native-cloud-model", "maxMode": true, "selectedModels": []any{map[string]any{"modelId": "native-cloud-model"}}}
			config := map[string]any{"defaultModel": "native-cloud-model", "fallbackModels": []any{"native-fallback"}, "bestOfNDefaultModels": []any{"native-cloud-model"}}
			if present {
				original["aiSettings"].(map[string]any)["modelConfig"].(map[string]any)["background-composer"] = background
				original["featureModelConfigs"].(map[string]any)["backgroundComposer"] = config
			}
			raw, _ := json.Marshal(original)
			db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: string(raw)})
			if err := forceModelSelectionAt(path, backup, "local-byok"); err != nil {
				t.Fatal(err)
			}
			current := readTestPreferences(t, db)
			mc := current["aiSettings"].(map[string]any)["modelConfig"].(map[string]any)
			fmc := current["featureModelConfigs"].(map[string]any)
			if present {
				if !reflect.DeepEqual(mc["background-composer"], background) || !reflect.DeepEqual(fmc["backgroundComposer"], config) {
					t.Fatal("start changed native cloud preferences")
				}
			} else if mc["background-composer"] != nil || fmc["backgroundComposer"] != nil {
				t.Fatal("start created native cloud preferences")
			}
			bk, err := readModelBackup(backup)
			if err != nil || bk.Version != 2 {
				t.Fatalf("invalid local-only backup: version=%d err=%v", bk.Version, err)
			}
			for _, entry := range bk.Values {
				if strings.Contains(strings.Join(entry.Path, "/"), "background") {
					t.Fatal("new backup owns cloud preferences")
				}
			}
			// Native cloud preference changes while BYOK runs must survive Stop.
			mc["background-composer"] = map[string]any{"modelName": "new-native-cloud-model"}
			fmc["backgroundComposer"] = map[string]any{"defaultModel": "new-native-cloud-model"}
			writeTestPreferences(t, db, current)
			if err := restoreModelSelectionAt(path, backup); err != nil {
				t.Fatal(err)
			}
			current = readTestPreferences(t, db)
			if current["aiSettings"].(map[string]any)["modelConfig"].(map[string]any)["background-composer"].(map[string]any)["modelName"] != "new-native-cloud-model" || current["featureModelConfigs"].(map[string]any)["backgroundComposer"].(map[string]any)["defaultModel"] != "new-native-cloud-model" {
				t.Fatal("stop overwrote a native cloud preference change")
			}
		})
	}
}

func TestLegacyModelBackupStillRestoresOldCloudOverride(t *testing.T) {
	original := `{"featureModelConfigs":{"backgroundComposer":{"defaultModel":"native-cloud","fallbackModels":["native-fallback"],"bestOfNDefaultModels":[]}}}`
	db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: original})
	if err := forceModelSelectionAt(path, backup, "local-byok"); err != nil {
		t.Fatal(err)
	}
	bk, err := readModelBackup(backup)
	if err != nil {
		t.Fatal(err)
	}
	bk.Version = 1
	old, _ := decodeApplicationUser([]byte(original))
	cloud := old["featureModelConfigs"].(map[string]any)["backgroundComposer"].(map[string]any)
	for _, key := range []string{"defaultModel", "fallbackModels", "bestOfNDefaultModels"} {
		value, _ := json.Marshal(cloud[key])
		bk.Values = append(bk.Values, modelPreference{Path: []string{"featureModelConfigs", "backgroundComposer", key}, Present: true, Value: value})
	}
	raw, _ := json.Marshal(bk)
	if err := os.WriteFile(backup, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	current := readTestPreferences(t, db)
	current["featureModelConfigs"].(map[string]any)["backgroundComposer"] = map[string]any{"defaultModel": "old-local-byok", "fallbackModels": []any{"old-local-byok"}, "bestOfNDefaultModels": []any{"old-local-byok"}}
	writeTestPreferences(t, db, current)
	if err := restoreModelSelectionAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestPreferences(t, db); !reflect.DeepEqual(got, old) {
		t.Fatalf("legacy cloud override not restored: %#v", got)
	}
}

func TestModelRestorePrunesOnlyEmptyCreatedObjects(t *testing.T) {
	for _, addSibling := range []bool{false, true} {
		t.Run(map[bool]string{false: "prune-empty", true: "retain-new-sibling"}[addSibling], func(t *testing.T) {
			db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: `{"unrelated":true}`})
			if err := forceModelSelectionAt(path, backup, "byok"); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"unrelated": true}
			if addSibling {
				current := readTestPreferences(t, db)
				current["aiSettings"].(map[string]any)["modelConfig"].(map[string]any)["composer"].(map[string]any)["custom"] = "keep"
				writeTestPreferences(t, db, current)
				want["aiSettings"] = map[string]any{"modelConfig": map[string]any{"composer": map[string]any{"custom": "keep"}}}
			}
			if err := restoreModelSelectionAt(path, backup); err != nil {
				t.Fatal(err)
			}
			if got := readTestPreferences(t, db); !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%#v want=%#v", got, want)
			}
		})
	}
}

func TestModelRestoreFailureRetainsBackupAndCanRetry(t *testing.T) {
	db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: `{"aiSettings":{"theme":"dark"}}`})
	if err := forceModelSelectionAt(path, backup, "byok"); err != nil {
		t.Fatal(err)
	}
	before := readTestItems(t, db)
	if _, err := db.Exec("CREATE TRIGGER deny_update BEFORE UPDATE ON ItemTable BEGIN SELECT RAISE(ABORT, 'denied'); END"); err != nil {
		t.Fatal(err)
	}
	if err := restoreModelSelectionAt(path, backup); err == nil {
		t.Fatal("expected update failure")
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, before) {
		t.Fatal("failed restore partially changed database")
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("failed restore removed backup", err)
	}
	if _, err := db.Exec("DROP TRIGGER deny_update"); err != nil {
		t.Fatal(err)
	}
	if err := restoreModelSelectionAt(path, backup); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"aiSettings": map[string]any{"theme": "dark"}}
	if got := readTestPreferences(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v", got)
	}
}

func TestModelBackupValidationPreventsUnrelatedWrites(t *testing.T) {
	db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: `{"unrelated":"original"}`})
	if err := forceModelSelectionAt(path, backup, "byok"); err != nil {
		t.Fatal(err)
	}
	bk, err := readModelBackup(backup)
	if err != nil {
		t.Fatal(err)
	}
	bk.Values[0].Path = []string{"unrelated"}
	bad, err := json.Marshal(bk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	before := readTestItems(t, db)
	if err := restoreModelSelectionAt(path, backup); err == nil {
		t.Fatal("accepted unexpected backup path")
	}
	if err := forceModelSelectionAt(path, backup, "second"); err == nil {
		t.Fatal("overrode invalid existing backup")
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, before) {
		t.Fatal("invalid backup changed database")
	}
	if got, _ := os.ReadFile(backup); string(got) != string(bad) {
		t.Fatal("invalid backup was overwritten")
	}
	// A slash inside one segment must not impersonate several allowed
	// segments and write to an unrelated top-level JSON property.
	bk.Values[0].Path = []string{"aiSettings/modelConfig", "cmd-k", "modelName"}
	bad, err = json.Marshal(bk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreModelSelectionAt(path, backup); err == nil {
		t.Fatal("accepted an ambiguous model path")
	}
}

func TestModelPinRejectsUnexpectedObjectShape(t *testing.T) {
	for _, original := range []string{`null`, `{"aiSettings":null}`, `{"aiSettings":{"modelConfig":{"composer":false}}}`} {
		db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: original})
		if err := forceModelSelectionAt(path, backup, "byok"); err == nil {
			t.Fatalf("accepted %s", original)
		}
		if got := readTestItems(t, db)[reactiveStorageKey]; got != original {
			t.Fatalf("changed unexpected settings: %s", got)
		}
		if _, err := os.Stat(backup); !os.IsNotExist(err) {
			t.Fatal("invalid shape created a backup")
		}
	}
}

func TestAuthWritesAndRestoreAreTransactional(t *testing.T) {
	original := map[string]string{"cursorAuth/accessToken": "real-token", "cursorAuth/cachedEmail": "user@example.test", "unrelated": "keep"}
	db, path, backup := testCursorDB(t, original)
	if _, err := db.Exec("CREATE TRIGGER deny_email BEFORE INSERT ON ItemTable WHEN NEW.key = 'cursorAuth/cachedEmail' BEGIN SELECT RAISE(ABORT, 'denied'); END"); err != nil {
		t.Fatal(err)
	}
	if err := injectFakeProUserAt(path, backup); err == nil {
		t.Fatal("expected auth injection failure")
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, original) {
		t.Fatalf("partial auth injection: %#v", got)
	}
	if _, err := db.Exec("DROP TRIGGER deny_email"); err != nil {
		t.Fatal(err)
	}
	if err := injectFakeProUserAt(path, backup); err != nil {
		t.Fatal(err)
	}
	firstBackup, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := injectFakeProUserAt(path, backup); err != nil {
		t.Fatal(err)
	}
	secondBackup, _ := os.ReadFile(backup)
	if string(firstBackup) != string(secondBackup) {
		t.Fatal("auth restart replaced original snapshot")
	}
	injected := readTestItems(t, db)
	if _, err := db.Exec("CREATE TRIGGER deny_delete BEFORE DELETE ON ItemTable WHEN OLD.key = 'cursorAuth/refreshToken' BEGIN SELECT RAISE(ABORT, 'denied'); END"); err != nil {
		t.Fatal(err)
	}
	if err := restoreFakeProUserAt(path, backup); err == nil {
		t.Fatal("expected auth restore failure")
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, injected) {
		t.Fatalf("partial auth restore: %#v", got)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("failed auth restore removed backup", err)
	}
	if _, err := db.Exec("DROP TRIGGER deny_delete"); err != nil {
		t.Fatal(err)
	}
	if err := restoreFakeProUserAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, original) {
		t.Fatalf("auth not restored: %#v", got)
	}
}

func TestAuthBackupValidationAndMissingDatabasePreserveRecovery(t *testing.T) {
	for _, invalid := range []string{`null`, `{}`, `{"unrelated":"destroy"}`, `{"cursorAuth/accessToken":false}`} {
		db, path, backup := testCursorDB(t, map[string]string{"unrelated": "keep"})
		if err := os.WriteFile(backup, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := injectFakeProUserAt(path, backup); err == nil {
			t.Fatalf("inject accepted %s", invalid)
		}
		if err := restoreFakeProUserAt(path, backup); err == nil {
			t.Fatalf("restore accepted %s", invalid)
		}
		if got := readTestItems(t, db); !reflect.DeepEqual(got, map[string]string{"unrelated": "keep"}) {
			t.Fatalf("corrupt backup modified database: %#v", got)
		}
	}
	_, path, backup := testCursorDB(t, map[string]string{"cursorAuth/accessToken": "real"})
	if err := injectFakeProUserAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := restoreFakeProUserAt(path+"-missing", backup); err == nil {
		t.Fatal("missing database reported restored")
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("missing database removed backup", err)
	}
}

func TestAuthRecoveryPreservesIndependentLoginOrLogout(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-login", true: "new-logout"}[logout], func(t *testing.T) {
			db, path, backup := testCursorDB(t, map[string]string{"cursorAuth/accessToken": "old-token", "cursorAuth/cachedEmail": "old@example.test", "unrelated": "keep"})
			if err := injectFakeProUserAt(path, backup); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("DELETE FROM ItemTable WHERE key LIKE 'cursorAuth/%'"); err != nil {
				t.Fatal(err)
			}
			if !logout {
				if _, err := db.Exec("INSERT INTO ItemTable VALUES ('cursorAuth/accessToken', 'new-token'), ('cursorAuth/refreshToken', 'new-refresh'), ('cursorAuth/cachedEmail', 'new@example.test')"); err != nil {
					t.Fatal(err)
				}
			}
			want := readTestItems(t, db)
			if err := restoreFakeProUserAt(path, backup); err != nil {
				t.Fatal(err)
			}
			if got := readTestItems(t, db); !reflect.DeepEqual(got, want) {
				t.Fatalf("stale backup overwrote independent auth: got=%#v want=%#v", got, want)
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Fatalf("stale backup retained after preserving current auth: %v", err)
			}
		})
	}
}

func TestAuthRecoveryRepairsPartialOriginalAccountRestore(t *testing.T) {
	original := map[string]string{"cursorAuth/accessToken": "original-token", "cursorAuth/cachedEmail": "original@example.test"}
	db, path, backup := testCursorDB(t, original)
	if err := injectFakeProUserAt(path, backup); err != nil {
		t.Fatal(err)
	}
	// An older nontransactional restore could write the original access token
	// before failing on another auth row. Matching it is safe to complete.
	if _, err := db.Exec("UPDATE ItemTable SET value = 'original-token' WHERE key = 'cursorAuth/accessToken'"); err != nil {
		t.Fatal(err)
	}
	if err := restoreFakeProUserAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, original) {
		t.Fatalf("partial auth restore not repaired: %#v", got)
	}
}

// Reproduces pre-migration state using temporary databases only.
func injectFakeProUserAt(dbPath, backupPath string) error {
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

	if _, err := readAuthBackup(backupPath); errors.Is(err, os.ErrNotExist) {
		backup := map[string]*string{}
		alreadyFake := false
		for _, key := range fakeAuthKeys {
			var value string
			err := tx.QueryRow("SELECT value FROM ItemTable WHERE key = ?", key).Scan(&value)
			switch err {
			case nil:
				backup[key] = &value
				alreadyFake = alreadyFake || (key == "cursorAuth/accessToken" && value == fakeProSessionJWT)
			case sql.ErrNoRows:
				backup[key] = nil
			default:
				return fmt.Errorf("backup read %s: %w", key, err)
			}
		}
		if token := backup[fakeAuthKeys[0]]; token != nil && strings.HasPrefix(*token, cursorSafeStoragePrefix) {
			decoded, err := decodeCursorAuthToken(dbPath, *token)
			if err != nil {
				return err
			}
			alreadyFake = decoded == fakeProSessionJWT
		}
		// A legacy unbacked fake session is not a usable original account.
		if alreadyFake {
			for _, key := range fakeAuthKeys {
				backup[key] = nil
			}
		}
		if err := writeStateBackup(backupPath, backup); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	values := []string{fakeProSessionJWT, fakeProSessionJWT, fakeProEmail, "Auth", "pro"}
	for i, key := range fakeAuthKeys {
		if _, err := tx.Exec("INSERT INTO ItemTable(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, values[i]); err != nil {
			return fmt.Errorf("upsert %s: %w", key, err)
		}
	}
	return tx.Commit()
}
