package relay

import (
	"reflect"
	"testing"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

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
