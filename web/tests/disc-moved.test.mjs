// A saved Bambu found at a new address: the scan row (moved, saved_id,
// saved_name) shows in the Add printer list with one button that opens the
// saved printer's edit dialog with the found address filled in, and saves
// nothing. Runs the real functions cut out of ui.html, as the other tests do.
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

const SAVED = [
  { id: 'p1', name: 'Studio P1S', ip: '192.168.1.20', serial: '01P00A000000001' },
  { id: 'p2', name: 'Shelf A1', ip: '192.168.1.21', serial: '03W00B000000002' },
];

function setup(printers = SAVED, scan = []) {
  const calls = { edit: [], api: [] };
  const ctx = {
    configuredPrinters: printers,
    discovery: { printers: scan },
    calls,
    escHtml: s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
    fill: (text, ...v) => text.replace(/\{(\d)\}/g, (_, i) => v[i]),
    discKey: row => row.type + ':' + (row.serial || row.url || row.ip || row.name || ''),
    openEditPrinterModal: (...a) => calls.edit.push(a),
    apiFetch: (...a) => calls.api.push(a),
  };
  vm.createContext(ctx);
  vm.runInContext(['discMovedPrinter', 'discMovedRow', 'useMovedAddress'].map(extractFunction).join('\n'), ctx);
  return ctx;
}

const row = (extra = {}) => ({ type: 'bambu', serial: '01P00A000000001', ip: '192.168.1.99', added: true, moved: true, saved_id: 'p1', saved_name: 'Studio P1S', ...extra });

test('a moved row finds its saved printer by id, then by name', () => {
  const c = setup();
  assert.equal(c.discMovedPrinter(row()).id, 'p1');
  assert.equal(c.discMovedPrinter(row({ saved_id: 'gone', saved_name: 'Shelf A1', serial: '03W00B000000002' })).id, 'p2');
  assert.equal(c.discMovedPrinter(row({ saved_id: undefined })).id, 'p1');
});

test('an older server (no moved field) or an unknown printer leaves the row hidden', () => {
  const c = setup();
  assert.equal(c.discMovedPrinter(row({ moved: undefined })), null);
  assert.equal(c.discMovedPrinter(row({ saved_id: undefined, saved_name: undefined })), null);
  assert.equal(c.discMovedPrinter(row({ saved_id: 'gone', saved_name: 'Nobody' })), null);
  assert.equal(c.discMovedPrinter({ type: 'bambu', ip: '192.168.1.20', added: true }), null);
});

test('a printer already saved at the found address is not moved', () => {
  const c = setup();
  assert.equal(c.discMovedPrinter(row({ ip: '192.168.1.20' })), null);
});

test('the row shows the saved name, the new address and one button', () => {
  const c = setup();
  const out = c.discMovedRow(row());
  assert.match(out, /Studio P1S/);
  assert.match(out, /now at 192\.168\.1\.99/);
  assert.equal((out.match(/<button/g) || []).length, 1);
  assert.match(out, /Use this address/);
});

test('the saved name is escaped', () => {
  const c = setup([{ id: 'p1', name: '<b>x</b>', ip: '192.168.1.20' }]);
  assert.doesNotMatch(c.discMovedRow(row()), /<b>/);
});

test('the button opens the edit dialog with the found address and saves nothing', () => {
  const r = row();
  const c = setup(SAVED, [r]);
  c.useMovedAddress(c.discKey(r));
  assert.deepEqual(c.calls.edit, [['p1', '192.168.1.99']]);
  assert.equal(c.calls.api.length, 0);
});

test('the button does nothing for a row that is not moved', () => {
  const r = row({ moved: undefined });
  const c = setup(SAVED, [r]);
  c.useMovedAddress(c.discKey(r));
  assert.equal(c.calls.edit.length, 0);
});

test('the edit dialog takes the found address and keeps the saved one otherwise', () => {
  const src = extractFunction('openEditPrinterModal');
  assert.match(src, /openEditPrinterModal\(id, foundIp\)/);
  assert.match(src, /getElementById\('pIp'\)\.value = foundIp \|\| printer\.ip \|\| ''/);
});
