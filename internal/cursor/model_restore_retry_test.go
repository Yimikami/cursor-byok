package cursor

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestModelRestoreRemovalRetryPreservesNewPreferences(t *testing.T) {
	original := `{"aiSettings":{"modelConfig":{"composer":{"modelName":"native-original"}}},"unrelated":"keep"}`
	db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: original})
	if err := forceModelSelectionAt(path, backup, "byok"); err != nil {
		t.Fatal(err)
	}
	removalFailure := errors.New("injected sidecar removal denial")
	err := restoreModelSelectionWithRemoveAt(path, backup, func(string) error { return removalFailure })
	if !errors.Is(err, removalFailure) {
		t.Fatalf("expected removal failure, got %v", err)
	}
	want, _ := decodeApplicationUser([]byte(original))
	if got := readTestPreferences(t, db); !reflect.DeepEqual(got, want) {
		t.Fatal("first restore did not commit preferences")
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("failed removal lost the sidecar", err)
	}
	// A later Cursor session may change the same owned field, not just a
	// sibling. A fresh restore call must honor the durable completion marker.
	want["aiSettings"].(map[string]any)["modelConfig"].(map[string]any)["composer"].(map[string]any)["modelName"] = "new-native-selection"
	writeTestPreferences(t, db, want)
	if err := restoreModelSelectionAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestPreferences(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("removal retry replayed stale preferences: %#v", got)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("retry did not remove sidecar: %v", err)
	}
	// Repeated sessions can have byte-identical snapshots. A new pin must
	// invalidate the previous completion marker transactionally.
	for range 2 {
		if err := forceModelSelectionAt(path, backup, "byok"); err != nil {
			t.Fatal(err)
		}
		if err := restoreModelSelectionAt(path, backup); err != nil {
			t.Fatal(err)
		}
		if got := readTestPreferences(t, db); !reflect.DeepEqual(got, want) {
			t.Fatal("completion from a previous session skipped a new restore")
		}
	}
}

func TestModelRestoreCompletionFailureRollsBackPreferences(t *testing.T) {
	db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: `{"unrelated":"keep"}`})
	if err := forceModelSelectionAt(path, backup, "byok"); err != nil {
		t.Fatal(err)
	}
	before := readTestItems(t, db)
	if _, err := db.Exec("CREATE TRIGGER deny_completion BEFORE INSERT ON ItemTable WHEN NEW.key = 'cursor-byok/modelRestoreCompleted' BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := restoreModelSelectionAt(path, backup); err == nil {
		t.Fatal("expected completion marker failure")
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, before) {
		t.Fatal("marker failure committed a partial restore")
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("failed transaction removed its backup", err)
	}
	if _, err := db.Exec("DROP TRIGGER deny_completion"); err != nil {
		t.Fatal(err)
	}
	if err := restoreModelSelectionAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestPreferences(t, db); !reflect.DeepEqual(got, map[string]any{"unrelated": "keep"}) {
		t.Fatalf("retry failed to restore preferences: %#v", got)
	}
}
