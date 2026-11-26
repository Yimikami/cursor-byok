package relay

import (
	"bytes"
	"reflect"
	"testing"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// This fixture is Cursor 3.32.4's named-only private-inference display
// configuration. The older generated response schema does not include field
// 15, so verify its wire contract independently of our encoding helpers.
func TestNativeCatalogOpensNamedModelsInsteadOfAutoOnlyView(t *testing.T) {
	body := buildAvailableModelsResponse([]AdapterInfo{{ModelID: "first"}, {ModelID: "second"}})
	for len(body) > 0 {
		field, kind, n := protowire.ConsumeTag(body)
		if n < 0 {
			t.Fatal("invalid catalog tag")
		}
		body = body[n:]
		if field == 15 {
			if kind != protowire.BytesType {
				t.Fatal("invalid native display configuration type")
			}
			value, n := protowire.ConsumeBytes(body)
			if n < 0 || !bytes.Equal(value, []byte{0x0a, 0x02, 0x28, 0x01, 0x12, 0x02, 0x1a, 0x00}) {
				t.Fatalf("native picker would not open its named model list: %x", value)
			}
			return
		}
		n = protowire.ConsumeFieldValue(field, kind, body)
		if n < 0 {
			t.Fatal("invalid catalog field")
		}
		body = body[n:]
	}
	t.Fatal("native catalog lacks named-model display configuration")
}

func TestDefaultModelStaysInConfiguredCatalog(t *testing.T) {
	adapters := []AdapterInfo{{ModelID: "active-provider-model"}, {ModelID: "second-model"}}
	gateway := NewGateway()
	gateway.SetAdapterProvider(func() []AdapterInfo { return adapters })
	for _, first := range []int{0, 1} {
		if first == 1 {
			adapters[0], adapters[1] = adapters[1], adapters[0]
		}
		var defaults aiserverv1.GetDefaultModelResponse
		if err := proto.Unmarshal(gateway.SyntheticPath("/aiserver.v1.AiService/GetDefaultModel"), &defaults); err != nil {
			t.Fatal(err)
		}
		id := adapters[0].StableID()
		if defaults.GetModel() != id || defaults.GetThinkingModel() != id || defaults.GetMaxMode() || defaults.GetNextDefaultSetDate() != "0" {
			t.Fatalf("default escaped active BYOK catalog: %v", &defaults)
		}
		var catalog aiserverv1.AvailableModelsResponse
		if err := proto.Unmarshal(gateway.SyntheticPath("/aiserver.v1.AiService/AvailableModels"), &catalog); err != nil {
			t.Fatal(err)
		}
		features := []*aiserverv1.AvailableModelsResponse_FeatureModelConfig{
			catalog.GetComposerModelConfig(), catalog.GetCmdKModelConfig(),
			catalog.GetPlanExecutionModelConfig(), catalog.GetSpecModelConfig(), catalog.GetDeepSearchModelConfig(),
			catalog.GetQuickAgentModelConfig(),
		}
		for i, config := range features {
			if config.GetDefaultModel() != id {
				t.Fatalf("feature %d changed default: %v", i, config)
			}
		}
		if catalog.GetBackgroundComposerModelConfig() != nil {
			t.Fatal("local catalog advertised a BYOK model as a cloud background default")
		}
		if gateway.SyntheticPath("/aiserver.v1.AiService/GetUser") != nil {
			t.Fatal("model defaults must not fabricate an account response")
		}
	}
}

func TestDefaultModelNudgesPreserveEveryConfiguredChoice(t *testing.T) {
	adapters := []AdapterInfo{{ModelID: "first"}, {ModelID: "second"}, {ModelID: "first"}}
	var response aiserverv1.GetDefaultModelNudgeDataResponse
	if err := proto.Unmarshal(buildGetDefaultModelNudgeDataResponse(adapters), &response); err != nil {
		t.Fatal(err)
	}
	if response.GetNudgeDate() != "0" || response.GetShouldDefaultSwitchOnNewChat() {
		t.Fatalf("model selection can be reset by a nudge: %v", &response)
	}
	wanted := []string{adapters[0].StableID(), adapters[1].StableID()}
	if !reflect.DeepEqual(response.GetModelsWithNoDefaultSwitch(), wanted) {
		t.Fatalf("missing protected selection: %v", response.GetModelsWithNoDefaultSwitch())
	}
}

func TestDefaultModelWithoutAdaptersDoesNotFabricateSelection(t *testing.T) {
	gateway := NewGateway()
	gateway.SetAdapterProvider(func() []AdapterInfo { return nil })
	if gateway.SyntheticPath("/aiserver.v1.AiService/GetDefaultModel") != nil {
		t.Fatal("returned a default without a configured model")
	}
	original, err := proto.Marshal(&aiserverv1.GetDefaultModelResponse{Model: "native-model"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rewriteGetDefaultModel(original, nil)
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatal("changed default without a configured adapter", err)
	}
}
