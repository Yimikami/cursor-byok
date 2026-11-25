'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const { configDirectory, loadState, selectModel } = require('./storage');

test('selection changes only the separate file and survives reload', async t => {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'byok-picker-'));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const privateConfig = '{"apiKey":"fixture-secret","unrelated":"preserve"}';
  await fs.writeFile(path.join(dir, 'config.json'), privateConfig);
  const catalogPath = path.join(dir, 'models-public.json');
  await fs.writeFile(catalogPath, JSON.stringify({ version: 1, models: [{ id: 'one' }, { id: 'two' }] }));
  assert.equal((await loadState(dir)).selected, undefined);
  await selectModel(dir, 'two');
  assert.equal((await loadState(dir)).selected.id, 'two');
  await selectModel(dir, 'one');
  assert.equal((await loadState(dir)).selected.id, 'one');
  assert.equal(await fs.readFile(path.join(dir, 'config.json'), 'utf8'), privateConfig);
  await assert.rejects(selectModel(dir, 'unconfigured'));
  assert.equal((await loadState(dir)).selected.id, 'one');
  await selectModel(dir, '');
  assert.equal((await loadState(dir)).selected, undefined);
  await fs.writeFile(path.join(dir, 'cursor-model-selection.json'), JSON.stringify({ version: 1, modelID: 'removed' }));
  assert.equal((await loadState(dir)).missing, true);
  await fs.writeFile(path.join(dir, 'cursor-model-selection.json'), ' '.repeat(4097));
  await assert.rejects(loadState(dir));
  assert.equal((await fs.readdir(dir)).some(name => name.endsWith('.tmp')), false);
});

test('configuration location follows the local app on each host', () => {
  assert.equal(configDirectory('win32', { APPDATA: '/roaming' }, '/home'), path.join('/roaming', 'cursor-byok'));
  assert.equal(configDirectory('darwin', {}, '/home'), path.join('/home', 'Library', 'Application Support', 'cursor-byok'));
  assert.equal(configDirectory('linux', { XDG_CONFIG_HOME: '/custom' }, '/home'), path.join('/custom', 'cursor-byok'));
});
