'use strict';
const fs = require('node:fs/promises');
const path = require('node:path');
const os = require('node:os');
const crypto = require('node:crypto');

function configDirectory(platform = process.platform, env = process.env, home = os.homedir()) {
  const base = platform === 'win32' ? (env.APPDATA || path.join(home, 'AppData', 'Roaming'))
    : platform === 'darwin' ? path.join(home, 'Library', 'Application Support')
    : (env.XDG_CONFIG_HOME || path.join(home, '.config'));
  return path.join(base, 'cursor-byok');
}

async function readJSON(file, limit) {
  const handle = await fs.open(file, 'r');
  try {
    const buffer = Buffer.alloc(limit + 1);
    let used = 0;
    while (used < buffer.length) {
      const { bytesRead } = await handle.read(buffer, used, buffer.length - used, null);
      if (bytesRead === 0) break;
      used += bytesRead;
    }
    if (used > limit) throw new Error('BYOK metadata file is too large');
    return JSON.parse(buffer.subarray(0, used).toString('utf8'));
  } finally { await handle.close(); }
}

async function loadState(dir) {
  const catalog = await readJSON(path.join(dir, 'models-public.json'), 1024 * 1024);
  if (catalog.version !== 1 || !Array.isArray(catalog.models)) throw new Error('Unsupported BYOK model catalog');
  const models = catalog.models.filter(m => m && typeof m.id === 'string' && m.id.length > 0);
  let selection;
  try { selection = await readJSON(path.join(dir, 'cursor-model-selection.json'), 4096); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (selection && (selection.version !== 1 || typeof selection.modelID !== 'string')) throw new Error('Invalid BYOK model selection');
  const selected = models.find(m => m.id === selection?.modelID);
  return { models, selected, missing: Boolean(selection?.modelID && !selected), defaultModelID: catalog.defaultModelID };
}

async function selectModel(dir, modelID) {
  // Re-read immediately before writing: an adapter may have been removed while
  // the picker was open. No arbitrary provider/model is introduced by this UI.
  const state = await loadState(dir);
  if (modelID !== '' && !state.models.some(m => m.id === modelID)) throw new Error('That model is no longer configured in cursor-byok');
  const body = JSON.stringify({ version: 1, modelID });
  if (Buffer.byteLength(body) > 4096) throw new Error('Model identifier is too long');
  const temp = path.join(dir, `.model-selection-${crypto.randomUUID()}.tmp`);
  try {
    await fs.writeFile(temp, body, { encoding: 'utf8', mode: 0o600, flag: 'wx' });
    await fs.rename(temp, path.join(dir, 'cursor-model-selection.json'));
  } finally { await fs.rm(temp, { force: true }); }
}

module.exports = { configDirectory, loadState, selectModel };
