package cursor

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestLegacyCloudModelPinRepairRequiresExactKnownSignature(t *testing.T) {
	for _, variant := range []string{"exact", "native-default", "mixed-fallback", "unknown-id", "different-composer", "missing-best-of"} {
		t.Run(variant, func(t *testing.T) {
			const id = "configured-local-adapter"
			composer := map[string]any{"modelName": id}
			cloud := map[string]any{"defaultModel": id, "fallbackModels": []any{id}, "bestOfNDefaultModels": []any{id}, "customChoice": "preserve"}
			switch variant {
			case "native-default":
				cloud["defaultModel"] = "native-cloud-model"
			case "mixed-fallback":
				cloud["fallbackModels"] = []any{id, "native-fallback"}
			case "unknown-id":
				cloud["defaultModel"] = "previous-unknown-adapter"
				cloud["fallbackModels"] = []any{"previous-unknown-adapter"}
				cloud["bestOfNDefaultModels"] = []any{"previous-unknown-adapter"}
			case "different-composer":
				composer["modelName"] = "native-local-choice"
			case "missing-best-of":
				delete(cloud, "bestOfNDefaultModels")
			}
			original := map[string]any{"aiSettings": map[string]any{"modelConfig": map[string]any{"composer": composer}}, "featureModelConfigs": map[string]any{"backgroundComposer": cloud}}
			raw, _ := json.Marshal(original)
			db, path, backup := testCursorDB(t, map[string]string{reactiveStorageKey: string(raw)})
			if err := forceModelSelectionAt(path, backup, id); err != nil {
				t.Fatal(err)
			}
			want := cloud
			if variant == "exact" {
				want = map[string]any{"customChoice": "preserve"}
			}
			for _, stop := range []bool{false, true} {
				if stop {
					if err := restoreModelSelectionAt(path, backup); err != nil {
						t.Fatal(err)
					}
				}
				got := readTestPreferences(t, db)["featureModelConfigs"].(map[string]any)["backgroundComposer"]
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("cloud choices changed unexpectedly (stop=%v): got=%#v want=%#v", stop, got, want)
				}
			}
		})
	}
}
