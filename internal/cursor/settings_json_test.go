package cursor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Keep the earlier deletion-subset fixtures on the consolidated JSONC editor.
func editSettingsValues(data []byte, updates map[string]any, removals []string) ([]byte, error) {
	changes := make(map[string]settingsChange)
	for key, value := range updates {
		changes[key] = settingsChange{present: true, value: value}
	}
	for _, key := range removals {
		changes[key] = settingsChange{}
	}
	return editSettings(data, changes)
}

func TestDecodeCursorJSONCSettings(t *testing.T) {
	data := []byte(`{
		// Cursor allows comments in its settings editor.
		"http.proxy": "http://127.0.0.1:7890",
		"literal": "/* not a comment */ // neither is this ,}",
		"escaped": "a quote: \" and slash: \\",
		"nested": {"list": [1, 2, /* trailing */ ],},
		"enabled": true, // comment after the final value
	}`)
	before := append([]byte(nil), data...)
	settings, err := decodeSettings(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"http.proxy": "http://127.0.0.1:7890",
		"literal":    "/* not a comment */ // neither is this ,}",
		"escaped":    "a quote: \" and slash: \\",
		"nested":     map[string]any{"list": []any{float64(1), float64(2)}},
		"enabled":    true,
	}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("settings=%#v, want %#v", settings, want)
	}
	if !reflect.DeepEqual(data, before) {
		t.Fatal("decoder changed input")
	}
}

func TestEditSettingsPreservesJSONCAndUnrelatedValues(t *testing.T) {
	input := append([]byte{0xef, 0xbb, 0xbf}, []byte("{\r\n"+
		"  // keep the proxy explanation\r\n"+
		"  \"http.proxy\": \"old\", /* inline explanation */\r\n"+
		"  \"editor\": {\"list\": [1, /* nested */ 2,],},\r\n"+
		"  \"literal\": \"// string,} /* string */\",\r\n"+
		"  // end of settings\r\n}\r\n")...)
	before := bytes.Clone(input)
	result, err := editSettingsValues(input, map[string]any{"http.proxy": "new", "http.proxySupport": "on"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input, before) || !bytes.HasPrefix(result, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("input or BOM changed")
	}
	for _, retained := range []string{
		"// keep the proxy explanation\r\n", "/* inline explanation */\r\n",
		`"editor": {"list": [1, /* nested */ 2,],},`,
		`"literal": "// string,} /* string */",`, "// end of settings\r\n",
	} {
		if !bytes.Contains(result, []byte(retained)) {
			t.Fatalf("lost unchanged text %q in %s", retained, result)
		}
	}
	if bytes.Contains(bytes.ReplaceAll(result, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("introduced different line endings")
	}
	m, err := decodeSettings(result)
	if err != nil || m["http.proxy"] != "new" || m["http.proxySupport"] != "on" {
		t.Fatalf("settings=%#v err=%v", m, err)
	}
}

func TestEditSettingsRemovalAndInsertionCommaCases(t *testing.T) {
	for _, input := range []string{
		`{ "a": 1, /* first */ "b": 2, "c": 3 }`,
		`{ "a": 1, "b": 2, "c": 3, /* trailing */ }`,
	} {
		for mask := 0; mask < 8; mask++ {
			for _, insert := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/%t", input, mask, insert), func(t *testing.T) {
					updates := map[string]any{}
					if insert {
						updates["new"] = true
					}
					var removals []string
					for i, key := range []string{"a", "b", "c"} {
						if mask&(1<<i) != 0 {
							removals = append(removals, key)
						}
					}
					result, err := editSettingsValues([]byte(input), updates, removals)
					if err != nil {
						t.Fatal(err)
					}
					m, err := decodeSettings(result)
					if err != nil {
						t.Fatal(err)
					}
					for _, key := range removals {
						if _, ok := m[key]; ok {
							t.Fatalf("removed property %s remains in %s", key, result)
						}
					}
					if insert && m["new"] != true {
						t.Fatalf("missing insertion: %s", result)
					}
					if !strings.Contains(string(result), "/*") {
						t.Fatalf("lost comment: %s", result)
					}
				})
			}
		}
	}
}

func TestEditSettingsDuplicateKeysAndCommentsInsideRemovedValue(t *testing.T) {
	input := []byte(`{"proxy": 1, "proxy": {"old": /* retain */ true}, "other": 3}`)
	if _, err := editSettingsValues(input, map[string]any{"proxy": "new"}, nil); err == nil {
		t.Fatal("ambiguous duplicate managed key accepted")
	}
	input = []byte(`{"proxy": {"old": /* retain */ true}, "other": 3}`)
	result, err := editSettingsValues(input, nil, []string{"proxy"})
	if err != nil || !bytes.Contains(result, []byte("/* retain */")) {
		t.Fatalf("result=%s err=%v", result, err)
	}
	m, _ := decodeSettings(result)
	if len(m) != 1 || m["other"] != float64(3) {
		t.Fatalf("unexpected settings: %#v", m)
	}
}

func TestEditSettingsRepeatedCyclesDoNotGrowDocument(t *testing.T) {
	original := []byte("{\n  // kept\n  \"editor.fontSize\": 14,\n}\n")
	current := bytes.Clone(original)
	for i := 0; i < 5; i++ {
		var err error
		current, err = editSettingsValues(current, map[string]any{"http.proxy": "temporary", "http.proxySupport": "on"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		current, err = editSettingsValues(current, nil, []string{"http.proxy", "http.proxySupport"})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(current, original) {
			t.Fatalf("cycle %d changed original document: %s", i, current)
		}
	}
}

func TestEditSettingsRetainsLineCommentsInsideReplacedValue(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		input := []byte("{\"proxy\": {\"old\": true // retained" + newline + "}}")
		result, err := editSettingsValues(input, map[string]any{"proxy": "new"}, nil)
		if err != nil || !bytes.Contains(result, []byte("// retained"+newline)) {
			t.Fatalf("result=%s err=%v", result, err)
		}
	}
}

func TestCursorTweaksPreserveCommentsAndEditsMadeWhileRunning(t *testing.T) {
	profile := t.TempDir()
	for _, key := range []string{"APPDATA", "HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		t.Setenv(key, profile)
	}
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte("{\n// original comment\n\"http.proxy\": \"old-proxy\",\n\"editor.fontSize\": 14,\n}\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCursorTweaks("127.0.0.1:18080"); err != nil {
		t.Fatal(err)
	}
	during, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	during = bytes.Replace(during, []byte(`"editor.fontSize": 14`), []byte("\"editor.fontSize\": 18 /* edited live */"), 1)
	if err := os.WriteFile(path, during, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RevertCursorTweaks(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := decodeSettings(after)
	if err != nil || len(settings) != 2 || settings["http.proxy"] != "old-proxy" || settings["editor.fontSize"] != float64(18) {
		t.Fatalf("settings=%#v err=%v", settings, err)
	}
	if !bytes.Contains(after, []byte("// original comment")) || !bytes.Contains(after, []byte("/* edited live */")) {
		t.Fatalf("lost user comments: %s", after)
	}
	if _, err := os.Stat(backupPath()); !os.IsNotExist(err) {
		t.Fatalf("backup was not consumed: %v", err)
	}
}

func TestDecodeSettingsAcceptsBOM(t *testing.T) {
	settings, err := decodeSettings(append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"editor.fontSize": 14,}`)...))
	if err != nil || settings["editor.fontSize"] != float64(14) {
		t.Fatalf("settings=%#v err=%v", settings, err)
	}
}

func TestDecodeSettingsRejectsMalformedDocument(t *testing.T) {
	for _, data := range []string{
		`{"name":"unfinished`, `{"name":1 /* unfinished`, `{"name":1} garbage`,
		`{"name":1 "other":2}`, `{"name":1,,}`, `{,}`, `{"list":[,]}`, `null`, `[]`,
	} {
		t.Run(data, func(t *testing.T) {
			if _, err := decodeSettings([]byte(data)); err == nil {
				t.Fatal("accepted malformed settings")
			}
		})
	}
}
