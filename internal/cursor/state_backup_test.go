package cursor

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStateBackupPublishesCompleteSnapshotWithoutReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.json")
	want := map[string]string{"model": "original", "unrelated": "keep"}
	err := writeStateBackupWithPublish(path, want, func(temporary, destination string) error {
		if filepath.Dir(temporary) != dir || destination != path {
			t.Fatal("temporary snapshot is not in the destination directory")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("final path was visible before publication: %v", err)
		}
		data, err := os.ReadFile(temporary)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.Unmarshal(data, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("published snapshot is incomplete: got=%v err=%v", got, err)
		}
		return os.Link(temporary, destination)
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStateBackup(path, map[string]string{"model": "later"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing snapshot was not protected: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(first) {
		t.Fatal("repeated start replaced the first snapshot", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "backup.json" {
		t.Fatalf("temporary files leaked: entries=%v err=%v", entries, err)
	}
}

func TestStateBackupPublishFailureCleansTemporaryFile(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(map[bool]string{false: "link-failure", true: "concurrent-first-snapshot"}[collision], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "backup.json")
			err := writeStateBackupWithPublish(path, map[string]string{"model": "second"}, func(temporary, destination string) error {
				if collision {
					if err := os.WriteFile(destination, []byte(`{"model":"first"}`), 0o600); err != nil {
						t.Fatal(err)
					}
					return os.Link(temporary, destination)
				}
				// Inject a real filesystem publication error, after the complete
				// temporary snapshot was written and closed.
				return os.Link(temporary, filepath.Join(dir, "missing", "backup.json"))
			})
			if err == nil {
				t.Fatal("expected publication failure")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if collision {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != `{"model":"first"}` || len(entries) != 1 {
					t.Fatalf("collision changed first snapshot or leaked temp file: entries=%v err=%v", entries, err)
				}
			} else if len(entries) != 0 {
				t.Fatalf("publication failure leaked files: %v", entries)
			}
		})
	}
}

func TestStateBackupCrashBeforePublicationLeavesNoSidecar(t *testing.T) {
	const helperEnv = "CURSOR_BYOK_TEST_BACKUP_CRASH_PATH"
	if path := os.Getenv(helperEnv); path != "" {
		_ = writeStateBackupWithPublish(path, map[string]string{"model": "original"}, func(string, string) error {
			os.Exit(87)
			return nil
		})
		os.Exit(88)
	}
	path := filepath.Join(t.TempDir(), "backup.json")
	command := exec.Command(os.Args[0], "-test.run=^TestStateBackupCrashBeforePublicationLeavesNoSidecar$")
	command.Env = append(os.Environ(), helperEnv+"="+path)
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 87 {
		t.Fatalf("crash injection did not run: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("crashed write exposed an unfinished final snapshot: %v", err)
	}
	if err := writeStateBackup(path, map[string]string{"model": "retry"}); err != nil {
		t.Fatalf("crashed temporary file blocked the next snapshot: %v", err)
	}
	data, err := os.ReadFile(path)
	var got map[string]string
	if err != nil || json.Unmarshal(data, &got) != nil || got["model"] != "retry" {
		t.Fatalf("retry snapshot is invalid: got=%v err=%v", got, err)
	}
}
