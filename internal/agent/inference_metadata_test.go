package agent

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	inferencev1 "cursor-byok/internal/protocodec/gen/inference/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestInferencePromptMetadataNativeWire(t *testing.T) {
	metadata := localInferencePromptMetadata()
	wire, err := proto.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	// Check installed wire numbers independently of the generated descriptor.
	counts := map[protowire.Number]int{}
	for len(wire) > 0 {
		number, typ, n := protowire.ConsumeTag(wire)
		if n < 0 {
			t.Fatal("bad tag")
		}
		wire = wire[n:]
		counts[number]++
		n = protowire.ConsumeFieldValue(number, typ, wire)
		if n < 0 {
			t.Fatal("bad field")
		}
		wire = wire[n:]
	}
	if counts[1] != 1 || counts[2] != 1 || counts[46] != 6 || counts[48] != 1 {
		t.Fatalf("missing client prompt/mode wire fields: %v", counts)
	}
	if metadata.Vendor != "unknown" || metadata.PromptVersion != "latest" || metadata.UseDsv3Harness || len(metadata.FeatureFlags) != 0 {
		t.Fatal("custom provider must keep generic native prompt and pinned flags")
	}
	for _, mode := range []string{"agent", "chat", "plan", "debug", "multitask"} {
		var row *inferencev1.RunInferenceAgentModeConfig
		for _, candidate := range metadata.AgentModeConfigs {
			if candidate.ModeId == mode {
				row = candidate
			}
		}
		if row == nil || row.SwitchModeToolConfig == nil || len(row.SwitchModeToolConfig.TargetModes) != 5 {
			t.Fatalf("mode %s cannot construct native mode controls", mode)
		}
	}
}

// Opt-in installed-client contract check. It never loads the harness module:
// only four extracted pure model/mode guards run in a fresh vm without
// require/process/network. This checks the downstream client prerequisites,
// including negative controls, rather than testing only a nonnil RunReady.
func TestInstalledInferencePromptGuards(t *testing.T) {
	if os.Getenv("CURSOR_BYOK_HARNESS_ARTIFACT") == "" {
		t.Skip("set CURSOR_BYOK_HARNESS_ARTIFACT to inspect installed client guards")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protojson.Marshal(localInferencePromptMetadata())
	if err != nil {
		t.Fatal(err)
	}
	const script = `
const fs = require('node:fs');
const vm = require('node:vm');
const src = fs.readFileSync(process.env.CURSOR_BYOK_HARNESS_ARTIFACT, 'utf8');
const metadata = JSON.parse(fs.readFileSync(0, 'utf8'));
function section(start, end) {
  const a = src.indexOf(start); const b = src.indexOf(end, a + start.length);
  if (a < 0 || b < 0 || b - a > 10000) throw new Error('client guard moved: ' + start);
  return src.slice(a, b);
}
function array(name) {
  const a = src.indexOf(name + '=['); const b = src.indexOf(']', a);
  if (a < 0 || b < 0) throw new Error('client guard list moved: ' + name);
  return JSON.parse(src.slice(a + name.length + 1, b + 1));
}
const context = vm.createContext({
  metadata,
  $qr: array('$qr'), Xot: array('Xot'), Gqr: array('Gqr'),
  Qe: { UNSPECIFIED:0, AGENT:1, ASK:2, PLAN:3, DEBUG:4, TRIAGE:5, MULTITASK:6 },
  RCe: { warn() {} }, dw: Error,
});
const guards = section('function Xqr(', 'var c2=') +
  section('function fW(', 'function uB(') +
  section('function gen(', 'var Uqr=') +
  section('var Uqr=', 'function hen(') +
  section('function hen(', 'function Kot(');
vm.runInContext(guards, context, { timeout:1000 });
const check = [
  "if (!Xqr(metadata, 'custom-provider-model')) throw new Error('client rejected generic profile');",
  "if (Xqr({...metadata, vendor:'not-native'}, 'm') !== undefined) throw new Error('vendor negative control failed');",
  "if (Xqr({...metadata, promptVersion:'not-native'}, 'm') !== undefined) throw new Error('version negative control failed');",
  "for (const id of ['agent','chat','plan','debug','multitask']) {",
  "  hen(gen(metadata), fW(id), 'm', {});",
  "  let rejected = false;",
  "  try { hen(gen({...metadata, agentModeConfigs:metadata.agentModeConfigs.filter(x=>x.modeId!==id)}), fW(id), 'm', {}); }",
  "  catch { rejected = true; }",
  "  if (!rejected) throw new Error('missing mode row not rejected: ' + id);",
  "}",
  "let rejected = false;",
  "try { hen(undefined, Qe.PLAN, 'm', {}); } catch { rejected = true; }",
  "if (!rejected) throw new Error('missing mode config negative control failed');",
].join('\n');
vm.runInContext(check, context, { timeout:1000 });
process.stdout.write('installed model and mode guards passed');
`
	command := exec.Command(node, "-e", script)
	command.Stdin = bytes.NewReader(payload)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("installed prompt guard failed: %v\n%s", err, output)
	}
	t.Log(string(output))
}
