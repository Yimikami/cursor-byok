package cursor

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestNativePickerBYOKModeRestoresOriginalPreferenceAndPreservesAccount(t *testing.T) {
	for _, value := range []string{"absent", "false", "true", "null"} {
		t.Run(value, func(t *testing.T) {
			original := map[string]any{"unrelated": "keep"}
			if value != "absent" {
				var v any
				if err := json.Unmarshal([]byte(value), &v); err != nil {
					t.Fatal(err)
				}
				original["useOpenAIKey"] = v
			}
			raw, _ := json.Marshal(original)
			db, path, backup := testCursorDB(t, map[string]string{
				reactiveStorageKey:                string(raw),
				"cursorAuth/accessToken":          "genuine-session-fixture",
				"cursorAuth/refreshToken":         "genuine-refresh-fixture",
				"cursorAuth/cachedEmail":          "native@example.test",
				"cursorAuth/stripeMembershipType": "free",
			})
			before := readTestItems(t, db)
			for _, model := range []string{"first-byok-model", "second-byok-model"} {
				if err := forceModelSelectionAt(path, backup, model); err != nil {
					t.Fatal(err)
				}
				if got := readTestPreferences(t, db)["useOpenAIKey"]; got != true {
					t.Fatalf("BYOK picker disabled: %v", got)
				}
				for key, want := range before {
					if key != reactiveStorageKey && readTestItems(t, db)[key] != want {
						t.Fatalf("changed account field %s", key)
					}
				}
			}
			if err := restoreModelSelectionAt(path, backup); err != nil {
				t.Fatal(err)
			}
			if got := readTestPreferences(t, db); !reflect.DeepEqual(got, original) {
				t.Fatalf("preference not restored: %#v", got)
			}
		})
	}
}

func TestV2ModelBackupDoesNotOwnNativeBYOKMode(t *testing.T) {
	db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: `{"useOpenAIKey":false}`})
	if err := forceModelSelectionAt(path, backup, "local-byok"); err != nil {
		t.Fatal(err)
	}
	bk, err := readModelBackup(backup)
	if err != nil {
		t.Fatal(err)
	}
	bk.Version = 2
	bk.Values = bk.Values[:len(modelPreferencePathsV2())]
	raw, _ := json.Marshal(bk)
	if err := os.WriteFile(backup, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before := readTestItems(t, db)
	if err := forceModelSelectionAt(path, backup, "another-byok"); err == nil {
		t.Fatal("new field mutated without a matching backup")
	}
	if !reflect.DeepEqual(before, readTestItems(t, db)) {
		t.Fatal("failed migration changed state")
	}
	if err := restoreModelSelectionAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestPreferences(t, db); !reflect.DeepEqual(got, map[string]any{"useOpenAIKey": true}) {
		t.Fatalf("v2 restoration changed unowned BYOK preference: %#v", got)
	}
}

func TestNativePickerClearsStaleCatalogLockAndRestoresItOnStop(t *testing.T) {
	const raw = `{"modelPickerDisplayConfiguration":{"lockedSelection":{"modelId":"grok-4.7"},"futureField":"keep"},"lockedModelPickerSelection":{"modelId":"grok-4.7"}}`
	db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: raw})
	if err := forceModelSelectionAt(path, backup, "local-byok"); err != nil {
		t.Fatal(err)
	}
	current := readTestPreferences(t, db)
	if _, ok := current["modelPickerDisplayConfiguration"]; ok {
		t.Fatal("stale display configuration can retain a native catalog lock")
	}
	if _, ok := current["lockedModelPickerSelection"]; ok {
		t.Fatal("stale native model selection remains")
	}
	current["modelPickerDisplayConfiguration"] = map[string]any{"routedModelViewConfig": map[string]any{"hideRoutedModelView": true}}
	writeTestPreferences(t, db, current)
	if err := restoreModelSelectionAt(path, backup); err != nil {
		t.Fatal(err)
	}
	want, err := decodeApplicationUser([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(readTestPreferences(t, db), want) {
		t.Fatal("original native picker configuration was not restored")
	}
}

func TestNativePickerDoesNotReactivateDisabledSavedKeys(t *testing.T) {
	for _, key := range []string{"cursorAuth/openAIKey", "secret://cursorAuth/openAIKey"} {
		for _, enabled := range []bool{false, true} {
			t.Run(key+map[bool]string{false: "/disabled", true: "/enabled"}[enabled], func(t *testing.T) {
				raw, _ := json.Marshal(map[string]any{"useOpenAIKey": enabled})
				db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: string(raw), key: "opaque-saved-key-fixture"})
				before := readTestItems(t, db)
				err := forceModelSelectionAt(path, backup, "local-byok")
				if enabled {
					if err != nil {
						t.Fatal(err)
					}
					if got := readTestItems(t, db)[key]; got != before[key] {
						t.Fatal("modified native credential")
					}
					if err := restoreModelSelectionAt(path, backup); err != nil {
						t.Fatal(err)
					}
				} else {
					if err == nil {
						t.Fatal("reactivated disabled native key")
					}
					if !reflect.DeepEqual(before, readTestItems(t, db)) {
						t.Fatal("failed preflight mutated preferences")
					}
					if _, err := os.Stat(backup); !os.IsNotExist(err) {
						t.Fatal("failed preflight created a backup")
					}
				}
			})
		}
	}
}
