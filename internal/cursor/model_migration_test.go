package cursor

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestModelSelectionMigratesOnlyRecognizableLegacyCloudPins(t *testing.T) {
	const oldID = "0123456789abcdef"
	const activeID = "fedcba9876543210"
	for _, tc := range []struct {
		name, cloud, wantCloud string
	}{
		{"old-inactive-adapter", `{"defaultModel":"0123456789abcdef","fallbackModels":["0123456789abcdef"],"bestOfNDefaultModels":["0123456789abcdef"]}`, ""},
		{"preserves-custom-sibling", `{"defaultModel":"0123456789abcdef","fallbackModels":["0123456789abcdef"],"bestOfNDefaultModels":["0123456789abcdef"],"custom":"keep"}`, `{"custom":"keep"}`},
		{"native-model", `{"defaultModel":"native-cloud","fallbackModels":["native-cloud"],"bestOfNDefaultModels":["native-cloud"]}`, "same"},
		{"unknown-hex-id", `{"defaultModel":"1111111111111111","fallbackModels":["1111111111111111"],"bestOfNDefaultModels":["1111111111111111"]}`, "same"},
		{"mixed-fallback", `{"defaultModel":"0123456789abcdef","fallbackModels":["native-cloud"],"bestOfNDefaultModels":["0123456789abcdef"]}`, "same"},
		{"multiple-fallbacks", `{"defaultModel":"0123456789abcdef","fallbackModels":["0123456789abcdef","native-cloud"],"bestOfNDefaultModels":["0123456789abcdef"]}`, "same"},
		{"partial-config", `{"defaultModel":"0123456789abcdef","fallbackModels":["0123456789abcdef"]}`, "same"},
		{"unexpected-config", `{"defaultModel":"0123456789abcdef","fallbackModels":[{}],"bestOfNDefaultModels":["0123456789abcdef"]}`, "same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := `{"featureModelConfigs":{"backgroundComposer":` + tc.cloud + `},"aiSettings":{"modelConfig":{"background-composer":{"modelName":"native-selected","custom":true}}},"unrelated":"keep"}`
			db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: original, "unrelated-row": "keep"})
			want, err := decodeApplicationUser([]byte(original))
			if err != nil {
				t.Fatal(err)
			}
			features := want["featureModelConfigs"].(map[string]any)
			switch tc.wantCloud {
			case "":
				delete(features, "backgroundComposer")
			case "same":
			default:
				var cloud map[string]any
				if err := json.Unmarshal([]byte(tc.wantCloud), &cloud); err != nil {
					t.Fatal(err)
				}
				features["backgroundComposer"] = cloud
			}
			// The old pin can belong to an adapter other than today's active one.
			// A second start/stop also proves the migration does not replay it.
			for range 2 {
				if err := forceModelSelectionAt(path, backup, activeID, activeID, oldID); err != nil {
					t.Fatal(err)
				}
				current := readTestPreferences(t, db)
				if !reflect.DeepEqual(current["featureModelConfigs"].(map[string]any)["backgroundComposer"], features["backgroundComposer"]) {
					t.Fatal("incorrect cloud preference migration during Start")
				}
				if err := restoreModelSelectionAt(path, backup); err != nil {
					t.Fatal(err)
				}
				if got := readTestPreferences(t, db); !reflect.DeepEqual(got, want) {
					t.Fatalf("Stop changed native preferences or restored a legacy pin: got=%#v want=%#v", got, want)
				}
			}
			if readTestItems(t, db)["unrelated-row"] != "keep" {
				t.Fatal("unrelated SQLite row changed")
			}
		})
	}
}
