# Cursor BYOK Model Picker

Choose a configured BYOK model inside Cursor with **Cursor BYOK: Select Model**
in the command palette, or click **BYOK** in the status bar.

Open the companion cursor-byok app and save your provider models first. Keep its
proxy running while using BYOK inference. This extension reads only the public
model catalog and writes a separate model-selection file; it never reads API
keys, Cursor credentials, or workspace files, and makes no network requests.

The selection applies globally to **local Agent inference**, including its
subagents, from the next model invocation. It survives editor/app restarts.
Commit/review-specific models remain independent. Choose **Use Cursor selection
/ BYOK default** to remove the override.

Cursor 3.32.4 can lock its own Auto menu behind its account plan. This extension
provides a separate BYOK picker; it does not unlock that menu, change your
subscription, or provide Cursor-hosted cloud/search/image services.

Build/install from the repository:

```sh
cd editor-extension
npx @vscode/vsce package --no-dependencies
cursor --install-extension cursor-byok-model-picker-0.1.0.vsix
```
