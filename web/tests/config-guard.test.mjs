// Guards the dashboard half of the v2.2.0 config-wipe fix (see CLAUDE.md,
// "Config compatibility"): a Settings save must never carry a "printers"
// key, so it can never replace the saved printer list. The server half is
// tested in cmd/foxtrack-bridge/server_config_guard_test.go.
//
// Run: node --test web/tests/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const html = readFileSync(new URL('../ui.html', import.meta.url), 'utf8');

// Cuts one top-level `function name(...) { ... }` out of the page by brace
// matching, so the test runs the real code, not a copy of it.
function extractFunction(name) {
  const start = html.indexOf(`function ${name}(`);
  assert.ok(start >= 0, `${name} not found in ui.html`);
  let depth = 0;
  for (let i = html.indexOf('{', html.indexOf(')', start)); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  assert.fail(`unbalanced braces in ${name}`);
}

function runPayload({ configEverLoaded }) {
  const ctx = {
    configEverLoaded,
    autoUpdateEnabled: true,
    document: { getElementById: () => ({ value: '  key  ', checked: true }) },
  };
  vm.createContext(ctx);
  vm.runInContext(extractFunction('buildSettingsPayload') + '; result = buildSettingsPayload();', ctx);
  return ctx.result;
}

test('Settings payload never carries printers', () => {
  for (const configEverLoaded of [true, false]) {
    const p = runPayload({ configEverLoaded });
    assert.equal('printers' in p, false, JSON.stringify(p));
    assert.equal('confirm_clear_printers' in p, false);
  }
});

test('auto_update is left out until the config has loaded once', () => {
  assert.equal('auto_update' in runPayload({ configEverLoaded: false }), false);
  assert.equal(runPayload({ configEverLoaded: true }).auto_update, true);
});

test('no POST to /api/config in the dashboard sends a printer list', () => {
  const sources = [html];
  const featureDir = new URL('../features/', import.meta.url);
  for (const f of ['05-command-menu.js', '10-notifications.js', '90-security.js']) {
    try { sources.push(readFileSync(new URL(f, featureDir), 'utf8')); } catch { /* optional */ }
  }
  const call = /apiFetch\(\s*'\/api\/config'\s*,\s*\{[^}]*method\s*:\s*'POST'[^\n]*/g;
  let n = 0;
  for (const src of sources) {
    for (const m of src.matchAll(call)) {
      n++;
      assert.doesNotMatch(m[0], /printers/, `config POST sends printers: ${m[0]}`);
    }
  }
  assert.ok(n >= 2, `expected the Settings and auto-update saves, found ${n} config POSTs`);
});
