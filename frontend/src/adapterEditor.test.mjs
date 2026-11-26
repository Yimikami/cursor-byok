import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';

const compile = source => ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } }).outputText;
const helpers = await import(`data:text/javascript;base64,${Buffer.from(compile(readFileSync(new URL('./adapterProbe.ts', import.meta.url), 'utf8'))).toString('base64')}`);
// Exercise the actual component handlers while a backend probe is pending.
const component = readFileSync(new URL('./components/ProxyDashboard.vue', import.meta.url), 'utf8').split('<script setup lang="ts">')[1].split('</script>')[0];
const ast = ts.createSourceFile('dashboard.ts', component, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
const handlers = ast.statements.filter(node => ts.isFunctionDeclaration(node) && ['openEditor', 'cancelEditor', 'saveEditor', 'duplicate', 'removeAdapter'].includes(node.name?.text)).map(node => node.getText(ast)).join('\n');
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
  const api = new Function(...Object.keys(scope), compile(handlers) + '\nreturn {openEditor, cancelEditor, saveEditor, duplicate, removeAdapter};')(...Object.values(scope));
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

test('review choices distinguish identical model names and survive reordering', () => {
  const declaration = ast.statements.find(node => ts.isVariableStatement(node) && node.declarationList.declarations.some(d => d.name.getText(ast) === 'reviewModelOptions'));
  assert.ok(declaration);
  const host = ast.statements.find(node => ts.isFunctionDeclaration(node) && node.name?.text === 'shortHost');
  const getOptions = new Function('computed', 'cfg', compile(host.getText(ast) + '\n' + declaration.getText(ast)) + '; return reviewModelOptions;');
  const first = {...adapter(), id:'first', displayName:'Provider A'};
  const second = {...adapter(), id:'second', displayName:'Provider B'};
  const options = rows => getOptions(fn => fn(), ref({modelAdapters:rows}));
  assert.deepEqual(options([first,second]).map(x => x.value), ['adapter:first','adapter:second']);
  assert.equal(options([second,first])[0].value,'adapter:second');
  assert.notEqual(options([first,second])[0].label,options([first,second])[1].label);
  first.displayName = second.displayName = '';
  assert.notEqual(options([first,second])[0].label,options([first,second])[1].label);
  assert.match(options([first,second])[0].label, /example.test/);
  const missing = getOptions(fn => fn(), ref({modelAdapters:[second], reviewModelID:'adapter:first'}));
  assert.equal(missing[0].value, 'adapter:first');
  assert.match(missing[0].label, /unavailable/);
});

test('duplicating an adapter creates a distinct selectable identity', async () => {
  const f = fixture();
  f.cfg.value.modelAdapters[0].id = 'original';
  await f.duplicate(0);
  assert.notEqual(f.cfg.value.modelAdapters[1].id,'original');
  assert.ok(f.cfg.value.modelAdapters[1].id);
  assert.equal(f.cfg.value.modelAdapters[1].modelID,f.cfg.value.modelAdapters[0].modelID);
});

test('provider changes during a probe stay open and unsaved', async () => {
  const f = fixture();f.openEditor(0);
  const pending = f.saveEditor(true);await Promise.resolve();
  const saved = {...f.cfg.value.modelAdapters[0]};
  f.editorProvider.value = 'anthropic';
  f.finishProbe({...saved,lastTestResult:'ok'});await pending;
  assert.equal(f.currentView.value,'editor');
  assert.equal(f.editorProvider.value,'anthropic');
  assert.equal(f.cfg.value.modelAdapters[0].type,'openai');
});

test('deleting an explicit review adapter keeps its selection unavailable', async () => {
  for (const selected of ['adapter:first-id', 'first']) {
    const f = fixture();
    f.cfg.value.modelAdapters[0].id = 'first-id';
    f.cfg.value.modelAdapters.push({...adapter(),id:'second-id'});
    f.cfg.value.reviewModelID = selected;
    await f.removeAdapter(0);
    assert.equal(f.cfg.value.reviewModelID, 'adapter:first-id');
    assert.equal(f.writes.at(-1).reviewModelID, 'adapter:first-id');
  }
});
