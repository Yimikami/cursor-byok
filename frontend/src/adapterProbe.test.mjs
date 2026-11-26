import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import ts from 'typescript';

// Use the project's existing compiler; no extra browser/test runtime is needed.
const source = readFileSync(new URL('./adapterProbe.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } });
const { captureAdapterProbe, applyAdapterProbeResult } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const adapter = () => ({ displayName: 'Fixture', type: 'openai', baseURL: 'https://example.test', apiKey: 'fixture', modelID: 'first', notes: '' });

test('a delayed probe never restores edited, removed, or replaced rows', async () => {
  for (const edit of [
    rows => { rows[0].modelID = 'edited'; },
    rows => { rows[0].apiKey = 'changed'; },
    rows => { rows.splice(0, 1); },
    rows => { rows[0] = { ...rows[0], modelID: 'replacement' }; },
    rows => { rows[0].supportedThinkingLevels = ['high']; },
  ]) {
    const rows = [adapter()];
    const probe = captureAdapterProbe(rows[0]);
    const result = { ...rows[0], lastTestResult: 'ok', lastTestedAt: 123 };
    let complete;
    const pending = new Promise(resolve => { complete = resolve; })
      .then(value => applyAdapterProbeResult(rows[0], probe, value));
    edit(rows);
    const edited = JSON.stringify(rows);
    complete(result);
    assert.equal(await pending, false);
    assert.equal(JSON.stringify(rows), edited);
  }
});

test('an unchanged row receives only probe metadata despite omitted empty fields', () => {
  const row = adapter();
  const probe = captureAdapterProbe(row);
  const { notes, ...result } = row;
  assert.equal(applyAdapterProbeResult(row, probe, { ...result, lastTestResult: 'ok', lastTestedAt: 123 }), true);
  assert.equal(row.notes, '');
  assert.equal(row.lastTestResult, 'ok');
  assert.equal(row.lastTestedAt, 123);
});

test('a result from a different server-side adapter cannot update the row', () => {
  const row = adapter();
  assert.equal(applyAdapterProbeResult(row, captureAdapterProbe(row), { ...row, modelID: 'different', lastTestResult: 'ok' }), false);
  assert.equal(row.lastTestResult, undefined);
});
