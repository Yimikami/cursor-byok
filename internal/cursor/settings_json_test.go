package cursor

import (
	"reflect"
	"testing"
)

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
