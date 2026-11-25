package cursor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsApplyRevertPreservesJSONCAndSessionChanges(t *testing.T) {
	dir := t.TempDir()
	settingsFile := filepath.Join(dir, "settings.json")
	backupFile := filepath.Join(dir, "backup.json")
	original := "\xef\xbb\xbf{\r\n" +
		"  // Enable the work proxy only at the office.\r\n" +
		"  // \"http.proxy\": \"http://office.example:8888\",\r\n" +
		"  \"http.proxy\" /* keep key note */ : /* keep value note */ \"http:\\/\\/corp.example:8080\", // original proxy\r\n" +
		"  \"editor.fontSize\": 14,\r\n" +
		"  \"custom\": {\"number\": 9007199254740993, \"nested\": [1e3, /* nested note */ 2,],},\r\n" +
		"  /* final note */\r\n} // file note\r\n"
	if err := os.WriteFile(settingsFile, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := applyCursorTweaksAt(settingsFile, backupFile, "127.0.0.1:18080"); err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(active, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("BOM lost")
	}
	// Emulate an unrelated setting edit and a new comment while the proxy runs.
	active = bytes.Replace(active, []byte(`"editor.fontSize": 14`), []byte(`"editor.fontSize": 17`), 1)
	active = bytes.Replace(active, []byte(`"http.proxyStrictSSL": true`), []byte(`"http.proxyStrictSSL" /* added during session */: true`), 1)
	if err := os.WriteFile(settingsFile, active, 0600); err != nil {
		t.Fatal(err)
	}
	if err := revertCursorTweaksAt(settingsFile, backupFile); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"// Enable the work proxy only at the office.",
		`// "http.proxy": "http://office.example:8888",`,
		`"http.proxy" /* keep key note */ : /* keep value note */ "http:\/\/corp.example:8080", // original proxy`,
		`"editor.fontSize": 17`,
		`"custom": {"number": 9007199254740993, "nested": [1e3, /* nested note */ 2,],},`,
		"/* final note */", "// file note", "/* added during session */",
	} {
		if !bytes.Contains(restored, []byte(text)) {
			t.Fatalf("lost %q:\n%s", text, restored)
		}
	}
	values, err := decodeSettings(restored)
	if err != nil {
		t.Fatal(err)
	}
	if values["http.proxy"] != "http://corp.example:8080" {
		t.Fatalf("proxy not restored: %#v", values["http.proxy"])
	}
	for key := range cursorTweakChanges("") {
		if key != "http.proxy" {
			if _, ok := values[key]; ok {
				t.Fatalf("injected %q survived revert", key)
			}
		}
	}
	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Fatalf("backup not consumed: %v", err)
	}
}

func TestSettingsEditInsertionAndRemovalBoundaries(t *testing.T) {
	for _, input := range []string{`{}`, `{"keep":1}`, `{"keep":1,}`, "{ // note\n}", "{\n  \"keep\": 1 // final note\n}", "{\r\n\t\"keep\": [1, /* nested */ 2,],\r\n}\r\n"} {
		t.Run(input, func(t *testing.T) {
			edits := map[string]settingsChange{"http.proxy": {present: true, value: "http://localhost:18080"}}
			active, err := editSettings([]byte(input), edits)
			if err != nil {
				t.Fatal(err)
			}
			values, err := decodeSettings(active)
			if err != nil || values["http.proxy"] != "http://localhost:18080" {
				t.Fatalf("active=%s err=%v", active, err)
			}
			restored, err := editSettings(active, map[string]settingsChange{"http.proxy": {present: false}})
			if err != nil {
				t.Fatal(err)
			}
			values, err = decodeSettings(restored)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := values["http.proxy"]; ok {
				t.Fatal("property not removed")
			}
			for _, comment := range []string{"// note", "// final note", "/* nested */"} {
				if strings.Contains(input, comment) && !bytes.Contains(restored, []byte(comment)) {
					t.Fatalf("lost comment %s", comment)
				}
			}
		})
	}
}

func TestSettingsEditExistingValueRoundTripIsExact(t *testing.T) {
	original := []byte("{\n  /* note */ \"http.proxy\" : \"http:\\/\\/corp\" /* note after value */,\n  \"untouched\": 1e+03,\n}\n")
	doc, err := parseSettingsDocument(original)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]settingsChange{"http.proxy": {present: true, value: "new"}}
	raw, err := doc.rawValues(changes)
	if err != nil {
		t.Fatal(err)
	}
	active, err := editSettings(original, changes)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := editSettings(active, map[string]settingsChange{"http.proxy": {present: true, raw: raw["http.proxy"]}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("round trip changed text:\n%s", restored)
	}
}

func TestSettingsApplyRejectsDuplicateManagedKeyWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	settingsFile := filepath.Join(dir, "settings.json")
	backupFile := filepath.Join(dir, "backup.json")
	original := []byte(`{"http.proxy":"first","http.proxy":"second"}`)
	if err := os.WriteFile(settingsFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := applyCursorTweaksAt(settingsFile, backupFile, "localhost:18080"); err == nil {
		t.Fatal("ambiguous duplicate accepted")
	}
	after, err := os.ReadFile(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("invalid document changed")
	}
	if _, err := os.Stat(backupFile); !os.IsNotExist(err) {
		t.Fatalf("unexpected backup: %v", err)
	}
}

func TestSettingsEditRejectsCorruptOriginalValue(t *testing.T) {
	_, err := editSettings([]byte(`{"http.proxy":"local","unrelated":"keep"}`), map[string]settingsChange{
		"http.proxy": {present: true, raw: `"restored", "unrelated":"changed"`},
	})
	if err == nil {
		t.Fatal("corrupt backup injected an unrelated property")
	}
}

func TestSettingsApplyAcceptsCompactDocumentWithoutTrailingComma(t *testing.T) {
	dir := t.TempDir()
	path, backup := filepath.Join(dir, "settings.json"), filepath.Join(dir, "backup.json")
	if err := os.WriteFile(path, []byte(`{"editor.fontSize":14}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := applyCursorTweaksAt(path, backup, "127.0.0.1:18080"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := decodeSettings(data)
	if err != nil || settings["editor.fontSize"] != float64(14) || settings["http.proxy"] != "http://127.0.0.1:18080" {
		t.Fatalf("invalid compact settings edit: %s, %v", data, err)
	}
	if err := revertCursorTweaksAt(path, backup); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsRawRestoreDoesNotDuplicateMovedValueComments(t *testing.T) {
	for _, original := range []string{
		`{"http.proxy": {"old": /* keep */ true}, "other": 1}`,
		"{\"http.proxy\": {\"old\": true // keep\n}, \"other\": 1}",
	} {
		doc, err := parseSettingsDocument([]byte(original))
		if err != nil {
			t.Fatal(err)
		}
		changes := map[string]settingsChange{"http.proxy": {present: true, value: "temporary"}}
		raw, err := doc.rawValues(changes)
		if err != nil {
			t.Fatal(err)
		}
		active, err := editSettings([]byte(original), changes)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := editSettings(active, map[string]settingsChange{"http.proxy": {present: true, raw: raw["http.proxy"]}})
		if err != nil || string(restored) != original {
			t.Fatalf("value comment round trip changed document: %s, %v", restored, err)
		}
	}
}
