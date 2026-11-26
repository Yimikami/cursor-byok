package relay

import (
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// Cursor 3.32.4 fetches this after AvailableModels. An upstream Auto default
// can replace the BYOK selection even though the injected catalog contains
// only configured adapters. Keep both default variants inside that catalog.
// next_default_set_date="0" does not schedule a later forced selection reset.
func buildGetDefaultModelResponse(adapters []AdapterInfo) []byte {
	if len(adapters) == 0 {
		return nil
	}
	id := adapters[0].StableID()
	out := appendString(nil, 1, id) // model
	out = appendString(out, 2, id)  // thinking_model
	out = appendBool(out, 3, false) // max_mode
	return appendString(out, 4, "0")
}

func appendMessage(dst []byte, field protowire.Number, body []byte) []byte {
	dst = protowire.AppendTag(dst, field, protowire.BytesType)
	return protowire.AppendBytes(dst, body)
}

// The shipped schema defines field 1 as nudge_date and repeated field 3 as
// models_with_no_default_switch. Disable scheduled model nudges and preserve
// every configured BYOK choice, not just the highest-priority adapter.
func buildGetDefaultModelNudgeDataResponse(adapters []AdapterInfo) []byte {
	if len(adapters) == 0 {
		return nil
	}
	out := make([]byte, 0, 32)
	out = appendString(out, 1, "0")
	seen := map[string]bool{}
	for _, adapter := range adapters {
		id := adapter.StableID()
		if adapter.ModelID != "" && !seen[id] {
			out = appendString(out, 3, id)
			seen[id] = true
		}
	}
	return out
}

// buildAvailableModelsResponse hand-rolls the protobuf bytes for an
// AvailableModelsResponse that mirrors the closed-source working app's
// 360-byte payload exactly, but with one entry per BYOK adapter.
//
// The shape was reverse-engineered from a captured response. Cursor's chat
// picker is sensitive to which fields are present and which aren't; the
// generated proto types we ship cover only fields documented in the public
// burpheart extract. Anything beyond that (fields 12, 13, 38) is appended
// as raw varints with the same values the working app sends.
func buildAvailableModelsResponse(adapters []AdapterInfo) []byte {
	buf := make([]byte, 0, 512)

	for _, a := range adapters {
		if a.ModelID == "" {
			continue
		}
		hexID := a.StableID()
		display := a.DisplayName
		if display == "" {
			display = a.ModelID
		}
		// AvailableModel (repeated, field 2)
		modelBytes := buildAvailableModel(hexID, display, a.SupportedThinkingLevels)
		buf = protowire.AppendTag(buf, protowire.Number(2), protowire.BytesType)
		buf = protowire.AppendVarint(buf, uint64(len(modelBytes)))
		buf = append(buf, modelBytes...)
	}

	if len(adapters) == 0 {
		return buf
	}
	first := adapters[0]
	hexID := first.StableID()

	// FeatureModelConfig fields for locally executed features:
	//   4 composer_model_config        (default + fallback + best_of_n)
	//   5 cmd_k_model_config           (default + fallback)
	// Field 6 (cloud background composer) is deliberately omitted: a local
	// BYOK identifier is not a model supported by Cursor's cloud backend.
	//   7 plan_execution_model_config  (default + fallback)
	//   8 spec_model_config            (default only)
	//   9 deep_search_model_config     (default only)
	//  10 quick_agent_model_config     (default only)
	buf = appendFeatureCfg(buf, 4, hexID, true, true)
	buf = appendFeatureCfg(buf, 5, hexID, true, false)
	buf = appendFeatureCfg(buf, 7, hexID, true, false)
	buf = appendFeatureCfg(buf, 8, hexID, false, false)
	buf = appendFeatureCfg(buf, 9, hexID, false, false)
	buf = appendFeatureCfg(buf, 10, hexID, false, false)
	// Cursor 3.32 separates routed models (Auto) from named models. Our
	// locally configured adapters belong in the native named-model list.
	// Mirror its private-inference display configuration: hide routed view
	// and omit the button back to it, while retaining native search/selection.
	// AvailableModelsResponse.display_configuration = field 15;
	// routed_model_view_config.hide_routed_model_view = fields 1 / 5;
	// named_models_view_config.named_view_to_routed_model_view_no_button = 2 / 3.
	display := appendMessage(nil, 1, appendBool(nil, 5, true))
	display = appendMessage(display, 2, appendMessage(nil, 3, nil))
	buf = appendMessage(buf, 15, display)

	// Fields 12 (= 2400000) and 13 (= 2). Unknown semantics but the working
	// app emits these every time. Replicating them keeps Cursor's picker
	// from flipping into the "missing data, hide BYOK" branch. The exact
	// value 2,400,000 was decoded from a captured working-app response
	// (varint bytes 80 BE 92 01).
	buf = protowire.AppendTag(buf, protowire.Number(12), protowire.VarintType)
	buf = protowire.AppendVarint(buf, 2400000)
	buf = protowire.AppendTag(buf, protowire.Number(13), protowire.VarintType)
	buf = protowire.AppendVarint(buf, 2)

	return buf
}

// buildAvailableModel constructs one AvailableModel sub-message body matching
// the working app's exact field selection.
func buildAvailableModel(hexID, display string, supportedThinkingLevels []string) []byte {
	out := make([]byte, 0, 128)
	// 1: name (bytes) = hexID
	out = appendString(out, 1, hexID)
	// 2: default_on (varint) = true
	out = appendBool(out, 2, true)
	// 5: supports_agent (varint) = true
	out = appendBool(out, 5, true)
	// 6: degradation_status (varint) = 0 (UNSPECIFIED)
	out = protowire.AppendTag(out, protowire.Number(6), protowire.VarintType)
	out = protowire.AppendVarint(out, 0)
	// 8: tooltip_data — a TooltipData sub-message containing only field 7
	// (tertiary_text) = "Notes". Working app sets this verbatim.
	tooltip := append([]byte(nil), 0x3a, 0x05)
	tooltip = append(tooltip, []byte("Notes")...)
	out = protowire.AppendTag(out, protowire.Number(8), protowire.BytesType)
	out = protowire.AppendVarint(out, uint64(len(tooltip)))
	out = append(out, tooltip...)
	// 9: supports_thinking (varint) = true
	out = appendBool(out, 9, true)
	// 10: supports_images (varint) = true
	out = appendBool(out, 10, true)
	// 14: supports_max_mode (varint) = true
	out = appendBool(out, 14, true)
	// 17: client_display_name (bytes) = display
	out = appendString(out, 17, display)
	// 18: server_model_name (bytes) = hexID (NOT the user's modelID!)
	out = appendString(out, 18, hexID)
	// 19: supports_non_max_mode (varint) = true
	out = appendBool(out, 19, true)
	// 20: tooltip_data_for_max_mode — same Notes tooltip
	out = protowire.AppendTag(out, protowire.Number(20), protowire.BytesType)
	out = protowire.AppendVarint(out, uint64(len(tooltip)))
	out = append(out, tooltip...)
	// 21: is_recommended_for_background_composer = false (still emit it)
	out = appendBool(out, 21, false)
	// 22: supports_plan_mode (varint) = true
	out = appendBool(out, 22, true)
	// 24: inputbox_short_model_name (bytes) = display
	out = appendString(out, 24, display)
	// 25: supports_sandboxing (varint) = true
	out = appendBool(out, 25, true)
	// 38: named_model_section_index = 1 (Cursor 3.32.4); this is a picker
	// section index, not an entitlement or BYOK authorization flag.
	out = appendBool(out, 38, true)

	if len(supportedThinkingLevels) > 0 {
		out = appendThinkingConfig(out, hexID, display, supportedThinkingLevels)
	}

	return out
}

func thinkingLevelDisplayName(lvl string) string {
	if lvl == "" {
		return ""
	}
	if lvl == "xhigh" {
		return "Extra High"
	} else if lvl == "none" {
		return "None"
	}
	return strings.ToUpper(lvl[:1]) + lvl[1:]
}

func appendThinkingConfig(out []byte, hexID, display string, supportedThinkingLevels []string) []byte {
	// Tag 29: parameter_definitions (repeated ModelParameterDefinition)
	paramDef := make([]byte, 0, 128)
	paramDef = appendString(paramDef, 1, "reasoning")
	paramDef = appendString(paramDef, 2, "Reasoning Effort")

	// 4: EnumParameterDefinition
	enumDef := make([]byte, 0, 64)
	for _, lvl := range supportedThinkingLevels {
		valDef := make([]byte, 0, 32)
		valDef = appendString(valDef, 1, lvl)
		valDef = appendString(valDef, 2, thinkingLevelDisplayName(lvl))

		enumDef = protowire.AppendTag(enumDef, protowire.Number(1), protowire.BytesType)
		enumDef = protowire.AppendVarint(enumDef, uint64(len(valDef)))
		enumDef = append(enumDef, valDef...)
	}

	// ModelParameterType has enum_parameter as tag 2
	paramType := make([]byte, 0, 64)
	paramType = protowire.AppendTag(paramType, protowire.Number(2), protowire.BytesType)
	paramType = protowire.AppendVarint(paramType, uint64(len(enumDef)))
	paramType = append(paramType, enumDef...)

	// ModelParameterDefinition has parameter_type as tag 4
	paramDef = protowire.AppendTag(paramDef, protowire.Number(4), protowire.BytesType)
	paramDef = protowire.AppendVarint(paramDef, uint64(len(paramType)))
	paramDef = append(paramDef, paramType...)

	out = protowire.AppendTag(out, protowire.Number(29), protowire.BytesType)
	out = protowire.AppendVarint(out, uint64(len(paramDef)))
	out = append(out, paramDef...)

	bestIdx := 0
	if len(supportedThinkingLevels) > 0 {
		order := map[string]int{"none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5}
		bestRank := 99
		for i, lvl := range supportedThinkingLevels {
			r, ok := order[lvl]
			if !ok {
				r = 99
			}
			if r < bestRank {
				bestRank = r
				bestIdx = i
			}
		}
	}

	// Tag 30: variants (repeated ModelVariantConfig)
	for i, lvl := range supportedThinkingLevels {
		variant := make([]byte, 0, 64)

		// 1: parameter_values (repeated RequestedModel.ModelParameterValue)
		paramVal := make([]byte, 0, 32)
		paramVal = appendString(paramVal, 1, "reasoning")
		paramVal = appendString(paramVal, 2, lvl)

		variant = protowire.AppendTag(variant, protowire.Number(1), protowire.BytesType)
		variant = protowire.AppendVarint(variant, uint64(len(paramVal)))
		variant = append(variant, paramVal...)

		// 2: display_name
		variantDisplayName := display
		if lvl != "none" {
			variantDisplayName = display + " (" + thinkingLevelDisplayName(lvl) + ")"
		}
		variant = appendString(variant, 2, variantDisplayName)

		// 3: is_max_mode
		variant = appendBool(variant, 3, false)

		// 4: is_default_max_config
		variant = appendBool(variant, 4, false)

		// 5: is_default_non_max_config
		variant = appendBool(variant, 5, i == bestIdx)

		out = protowire.AppendTag(out, protowire.Number(30), protowire.BytesType)
		out = protowire.AppendVarint(out, uint64(len(variant)))
		out = append(out, variant...)
	}

	return out
}

// appendFeatureCfg appends a FeatureModelConfig sub-message to buf at the
// outer field number `field`. The model_id is replicated into default_model,
// fallback_models[0], and (when withBestOfN) best_of_n_default_models[0].
func appendFeatureCfg(buf []byte, field int, hexID string, withFallback, withBestOfN bool) []byte {
	cfg := make([]byte, 0, 64)
	// 1: default_model (bytes)
	cfg = appendString(cfg, 1, hexID)
	if withFallback {
		// 2: fallback_models (repeated bytes)
		cfg = appendString(cfg, 2, hexID)
	}
	if withBestOfN {
		// 3: best_of_n_default_models (repeated bytes)
		cfg = appendString(cfg, 3, hexID)
	}
	buf = protowire.AppendTag(buf, protowire.Number(field), protowire.BytesType)
	buf = protowire.AppendVarint(buf, uint64(len(cfg)))
	return append(buf, cfg...)
}

func appendString(buf []byte, field int, value string) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(field), protowire.BytesType)
	buf = protowire.AppendString(buf, value)
	return buf
}

func appendBool(buf []byte, field int, v bool) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(field), protowire.VarintType)
	if v {
		return protowire.AppendVarint(buf, 1)
	}
	return protowire.AppendVarint(buf, 0)
}
