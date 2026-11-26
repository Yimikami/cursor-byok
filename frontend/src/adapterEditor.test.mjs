import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';

const compile = source => ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } }).outputText;
const helpers = await import(`data:text/javascript;base64,${Buffer.from(compile(readFileSync(new URL('./adapterProbe.ts', import.meta.url), 'utf8'))).toString('base64')}`);
// Exercise the actual component handlers while a backend probe is pending.
const component = readFileSync(new URL('./components/ProxyDashboard.vue', import.meta.url), 'utf8').split('<script setup lang="ts">')[1].split('</script>')[0];
const ast = ts.createSourceFile('dashboard.ts', component, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const handlers = ast.statements.filter(node => ts.isFunctionDeclaration(node) && ['openEditor', 'cancelEditor', 'saveEditor'].includes(node.name?.text)).map(node => node.getText(ast)).join('\n');
const ref = value => ({ value });
const adapter = () => ({ displayName: 'Fixture', type: 'openai', baseURL: 'https://example.test', apiKey: 'fixture', modelID: 'first', supportedThinkingLevels: ['low'], notes: '' });

function fixture() {
  let finishProbe;
  const probe = new Promise(resolve => { finishProbe = resolve; });
  const writes = [];
  const cfg = ref({ modelAdapters: [adapter()] });
  const state = { cfg, editorModel: ref(null), editorIndex: ref(-1), editorProvider: ref('openai'), providerTab: ref('openai'), showApiKey: ref(false), currentView: ref('models'), editorTestRunning: ref(false) };
  const persistConfig = async () => { writes.push(JSON.parse(JSON.stringify(cfg.value))); };
  const scope = { ...state, ...helpers, persistConfig, ProxyService: { TestAdapter: () => probe } };
  const api = new Function(...Object.keys(scope), compile(handlers) + '\nreturn {openEditor, cancelEditor, saveEditor};')(...Object.values(scope));
  return { ...state, ...api, writes, persistConfig, finishProbe };
}

for (const index of [0, -1]) {
  test(`canceling edits during Save and test cannot leak into a later save (${index})`, async () => {
    const f = fixture();
    f.openEditor(index);
    f.editorModel.value.modelID = 'saved';
    const pending = f.saveEditor(true);
    await Promise.resolve();
    const saved = JSON.parse(JSON.stringify(f.cfg.value.modelAdapters.at(-1)));
    f.editorModel.value.modelID = 'canceled-draft';
    f.editorModel.value.supportedThinkingLevels.push('high');
    f.cancelEditor();
    f.finishProbe({ ...saved, lastTestResult: 'ok', lastTestedAt: 123 });
    await pending;
    await f.persistConfig();
    assert.equal(f.writes.at(-1).modelAdapters.at(-1).modelID, 'saved');
    assert.deepEqual(f.writes.at(-1).modelAdapters.at(-1).supportedThinkingLevels, saved.supportedThinkingLevels);
    assert.equal(f.currentView.value, 'models');
  });
}

test('a completed probe keeps newer unsaved edits open', async () => {
  const f = fixture();
  f.openEditor(0);
  const pending = f.saveEditor(true);
  await Promise.resolve();
  const saved = JSON.parse(JSON.stringify(f.cfg.value.modelAdapters[0]));
  f.editorModel.value.notes = 'not saved yet';
  f.finishProbe({ ...saved, lastTestResult: 'ok' });
  await pending;
  assert.equal(f.currentView.value, 'editor');
  assert.equal(f.editorModel.value.notes, 'not saved yet');
  assert.equal(f.cfg.value.modelAdapters[0].notes, '');
  assert.equal(f.cfg.value.modelAdapters[0].lastTestResult, 'ok');
});

test('an unchanged saved editor closes after the probe', async () => {
  const f = fixture();
  f.openEditor(0);
  const pending = f.saveEditor(true);
  await Promise.resolve();
  f.finishProbe({ ...f.cfg.value.modelAdapters[0], lastTestResult: 'ok' });
  await pending;
  assert.equal(f.currentView.value, 'models');
});
