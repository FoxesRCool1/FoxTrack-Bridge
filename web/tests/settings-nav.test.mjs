// The Settings nav and the FoxTrack chip in connected mode: with a token the
// standalone features fold into "More" and FoxTrack comes first; the chip says
// "connected" only once FoxTrack has accepted a report. Runs the real
// functions cut out of ui.html, as config-guard.test.mjs does.
//
// Run: node --test web/tests/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const html = readFileSync(new URL('../ui.html', import.meta.url), 'utf8');

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

function extractConst(name) {
  const start = html.indexOf(`const ${name} = `);
  assert.ok(start >= 0, `${name} not found in ui.html`);
  const end = html.indexOf(';\n', start);
  return html.slice(start, end + 1);
}

// Every inline script on the page must at least parse: a stray quote in one
// handler takes the whole dashboard down.
test('inline scripts parse', () => {
  const scripts = [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)];
  assert.ok(scripts.length >= 1, 'no inline script found');
  for (const [, src] of scripts) new vm.Script(src);
});

const TABS = [
  { id: 'cloud', label: 'Bambu Cloud', group: 'Connect', order: 50 },
  { id: 'foxtrack', label: 'FoxTrack', group: 'Connect', order: 10 },
  { id: 'virtual', label: 'Virtual printer', group: 'Connect', order: 85 },
  { id: 'assistant', label: 'Assistant', group: 'Bridge', order: 60 },
  { id: 'general', label: 'General', group: 'Bridge', order: 90 },
  { id: 'notifications', label: 'Notifications', group: 'Printing', order: 10 },
  { id: 'timelapse', label: 'Timelapse', group: 'Printing', order: 20 },
  { id: 'maintenance', label: 'Maintenance', group: 'Printing', order: 30 },
  { id: 'costs', label: 'Costs', group: 'Printing', order: 40 },
  { id: 'homeassistant', label: 'Home Assistant', group: 'Connect', order: 20 },
  { id: 'spoolman', label: 'Spoolman', group: 'Connect', order: 30 },
  { id: 'metrics', label: 'Prometheus', group: 'Connect', order: 40 },
  { id: 'security', label: 'Security', group: 'Bridge', order: 10 },
];

function navGroups(linked) {
  const ctx = {};
  vm.createContext(ctx);
  vm.runInContext(
    extractConst('SETTINGS_GROUPS') + extractConst('STANDALONE_TABS') + extractFunction('settingsNavGroups')
      + `; result = JSON.stringify(settingsNavGroups(${JSON.stringify(TABS)}, ${linked}));`,
    ctx,
  );
  return JSON.parse(ctx.result); // plain host objects: strict deepEqual rejects the vm context's Array
}

test('with a FoxTrack token the nav is short: FoxTrack first, standalone features under More', () => {
  const groups = navGroups(true);
  assert.deepEqual(groups.map(g => g.group), ['Connect', 'Bridge', 'More']);
  assert.deepEqual(groups[0].tabs.map(t => t.id), ['foxtrack', 'cloud']);
  assert.deepEqual(groups[1].tabs.map(t => t.id), ['security', 'general']);
  const more = groups[2];
  assert.equal(more.more, true);
  assert.deepEqual(
    more.tabs.map(t => t.id).sort(),
    ['assistant', 'costs', 'homeassistant', 'maintenance', 'metrics', 'notifications', 'spoolman', 'timelapse', 'virtual'],
  );
});

test('without a token the nav is the full three groups and nothing is folded', () => {
  const groups = navGroups(false);
  assert.deepEqual(groups.map(g => g.group), ['Printing', 'Connect', 'Bridge']);
  assert.ok(groups.every(g => !g.more));
  assert.equal(groups.flatMap(g => g.tabs).length, TABS.length);
});

function mode({ linked, check, delivered, problem }) {
  const ctx = {
    foxtrackTokenSet: linked, legacyKeySet: false,
    foxtrackCheckState: check, relayDelivered: delivered, relayProblem: problem,
  };
  vm.createContext(ctx);
  vm.runInContext(extractFunction('foxtrackLinked') + extractFunction('foxtrackMode') + '; result = foxtrackMode();', ctx);
  return ctx.result;
}

test('the chip says connected only when the check passed and a report was accepted', () => {
  assert.equal(mode({ linked: false, check: null, delivered: false, problem: false }), 'standalone');
  assert.equal(mode({ linked: true, check: null, delivered: false, problem: false }), 'checking');
  assert.equal(mode({ linked: true, check: 'unknown', delivered: true, problem: false }), 'unreachable');
  assert.equal(mode({ linked: true, check: 'ok', delivered: false, problem: false }), 'waiting');
  assert.equal(mode({ linked: true, check: 'ok', delivered: true, problem: false }), 'connected');
  assert.equal(mode({ linked: true, check: 'ok', delivered: true, problem: true }), 'attention');
  assert.equal(mode({ linked: true, check: 'bad_token', delivered: true, problem: false }), 'attention');
});

test('the unlink request carries only the clear flag', () => {
  const src = extractFunction('unlinkFoxTrack');
  const body = src.match(/body:\s*JSON\.stringify\(([^)]*)\)/);
  assert.ok(body, 'no JSON body in unlinkFoxTrack');
  assert.match(body[1], /clear_foxtrack_token:\s*true/);
  assert.doesNotMatch(body[1], /printers/);
});
