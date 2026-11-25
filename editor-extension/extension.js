'use strict';
const vscode = require('vscode');
const { configDirectory, loadState, selectModel } = require('./storage');

const text = value => String(value || '').replace(/[\r\n\t]/g, ' ').replace(/\$\(/g, '(').slice(0, 100);

function activate(context) {
  const directory = configDirectory();
  const status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 95);
  status.name = 'Cursor BYOK Model';
  status.command = 'cursorByok.selectModel';
  status.show();
  let closed = false;
  async function refresh() {
    try {
      const state = await loadState(directory);
      if (closed) return;
      status.text = state.selected ? `$(plug) BYOK: ${text(state.selected.label || state.selected.id)}` : '$(plug) BYOK: Default';
      status.tooltip = state.selected
        ? `Local Agent model: ${text(state.selected.id)}\nApplies globally from the next inference request while the BYOK proxy is running. Click to select a model.`
        : state.missing ? 'The previously selected model was removed. Click to choose a configured model.'
        : 'Using Cursor selection / the BYOK default. Click to choose a local Agent model.';
    } catch {
      if (closed) return;
      status.text = '$(plug) BYOK: Configure';
      status.tooltip = 'Open the cursor-byok app and save your models, then click to select one.';
    }
  }
  const command = vscode.commands.registerCommand('cursorByok.selectModel', async () => {
    try {
      const state = await loadState(directory);
      const items = state.models.map(model => ({
        label: text(model.label || model.id),
        description: model.id === state.selected?.id ? 'Selected' : text(model.provider),
        detail: text(model.id), modelID: model.id,
      }));
      items.push({ label: 'Use Cursor selection / BYOK default', detail: 'Remove the global local Agent override', modelID: '' });
      const picked = await vscode.window.showQuickPick(items, {
        title: 'Cursor BYOK — Local Agent Model',
        placeHolder: 'Applies to all local chats from their next model request',
        matchOnDetail: true, ignoreFocusOut: true,
      });
      if (!picked) return;
      await selectModel(directory, picked.modelID);
      await refresh();
      void vscode.window.showInformationMessage(picked.modelID
        ? `BYOK model: ${picked.label}. New local Agent requests will use this model.`
        : 'BYOK now follows Cursor selection / the configured default.');
    } catch {
      void vscode.window.showErrorMessage('BYOK model selection is unavailable. Open the cursor-byok app and save your models, then retry.');
    }
  });
  // Poll only these two small public metadata files. This also works when the
  // application starts after the editor or atomically replaces the catalog.
  const timer = setInterval(refresh, 3000);
  context.subscriptions.push(status, command, { dispose() { closed = true; clearInterval(timer); } });
  void refresh();
}

module.exports = { activate };
