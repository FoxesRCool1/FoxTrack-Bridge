// Spoolman: report the filament each print used to your own Spoolman server,
// so its spool weights stay right. The server is on your network; nothing goes
// to FoxTrack or anywhere else.
const sp = { root: null, cfg: null, spools: null, spoolsError: '' };
const SP_EXTERNAL = 254; // the external spool's slot number

const spStyle = document.createElement('style');
spStyle.textContent = `
#settings-spoolman .sp-row { display: grid; grid-template-columns: 1fr; gap: .5rem; padding: .75rem 0; border-top: 1px solid var(--border); }
@media (min-width: 640px) { #settings-spoolman .sp-row { grid-template-columns: 11rem 1fr; align-items: center; } }
#settings-spoolman .sp-slot { display: flex; align-items: center; gap: .5rem; min-width: 0; font-size: .875rem; }
#settings-spoolman .sp-dot { width: .875rem; height: .875rem; border-radius: 9999px; border: 1px solid var(--border); flex-shrink: 0; background: transparent; }
#settings-spoolman .sp-hint { font-size: .75rem; color: var(--text-2); margin-top: .25rem; }
#settings-spoolman .sp-url { display: flex; gap: .5rem; }
#settings-spoolman .sp-log { padding: .5rem 0; border-top: 1px solid var(--border); font-size: .8125rem; }
#settings-spoolman .sp-log-line { display: flex; align-items: baseline; justify-content: space-between; gap: .75rem; }
#settings-spoolman .sp-num { font-variant-numeric: tabular-nums; }
#settings-spoolman .sp-chip { flex-shrink: 0; font-size: .6875rem; padding: .125rem .5rem; border-radius: 9999px; border: 1px solid var(--border); color: var(--text-2); }
#settings-spoolman .sp-error .sp-chip { border-color: color-mix(in srgb, var(--danger) 60%, transparent); color: var(--danger); }
#settings-spoolman .sp-retrying .sp-chip { border-color: color-mix(in srgb, var(--warning) 60%, transparent); color: var(--warning); }
#settings-spoolman .sp-skel { height: 2.5rem; border-radius: var(--radius); background: var(--panel-2); margin-top: .5rem; }
@media (max-width: 640px) {
  #settings-spoolman .field, #settings-spoolman .btn-primary, #settings-spoolman .btn-outline { min-height: 2.75rem; }
}`;
document.head.appendChild(spStyle);

const spIsBambu = p => !(p.moonraker_url || p.prusalink_url || p.octoprint_url || p.home_assistant_url);
const spKey = (printerId, slot) => printerId + '/' + slot;
const spSpoolText = s => '#' + s.id + ' ' + [s.vendor, s.name].filter(Boolean).join(' ')
    + (s.material ? ' · ' + s.material : '') + ' · ' + Math.round(s.remaining_g || 0) + ' g left';
const spColor = hex => (/^[0-9a-fA-F]{6}$/.test(hex || '') ? '#' + hex : '');

// The slots to show for a printer: what the printer reports now, plus any
// slot that is mapped already so a saved choice never disappears.
function spSlots(p) {
    if (!spIsBambu(p)) return [{ slot: 0, label: 'Spool' }];
    const live = (typeof liveStatus !== 'undefined' && liveStatus[p.id]) || {};
    const out = (live.ams || []).map(s => ({ slot: s.slot, label: 'Slot ' + (s.slot + 1), material: s.material, color: s.color, tag: s.tag_uid }));
    const ext = live.external_spool || {};
    out.push({ slot: SP_EXTERNAL, label: 'External spool', material: ext.material, color: ext.color, tag: ext.tag_uid });
    for (const key of Object.keys(sp.cfg.slots)) {
        const [id, n] = key.split('/');
        if (id === p.id && !out.some(s => String(s.slot) === n)) out.push({ slot: Number(n), label: 'Slot ' + (Number(n) + 1) });
    }
    return out;
}

function spSelectHtml(key, current, label) {
    const spools = sp.spools || [];
    const known = spools.some(s => s.id === current);
    let opts = '<option value="0">Not tracked</option>';
    if (current && !known) opts += '<option value="' + current + '" selected>Spool #' + current + '</option>';
    opts += spools.map(s => '<option data-no-i18n value="' + s.id + '"' + (s.id === current ? ' selected' : '') + '>' + escHtml(spSpoolText(s)) + '</option>').join('');
    return '<div class="select-wrap"><select class="field" data-sp-slot="' + escHtml(key) + '" aria-label="Spool for ' + escHtml(label) + '">' + opts + '</select><i class="fas fa-chevron-down"></i></div>';
}

function spMappingHtml() {
    const printers = (typeof configuredPrinters !== 'undefined' && configuredPrinters) || [];
    if (!printers.length) return '<p class="text-sm text-text-2">Add a printer on the Printers page to choose its spools.</p>';
    const rows = printers.map(p => {
        const head = '<h4 class="text-sm font-medium mt-5" data-no-i18n>' + escHtml(p.name) + '</h4>';
        return head + spSlots(p).map(s => {
            const key = spKey(p.id, s.slot);
            const current = sp.cfg.slots[key] || 0;
            const chosen = (sp.spools || []).find(x => x.id === current);
            const swatch = spColor(chosen ? chosen.color : s.color);
            let hint = '';
            if (!current && s.tag && (sp.spools || []).some(x => x.tag && x.tag.toLowerCase() === String(s.tag).toLowerCase())) hint = 'Matched by the spool’s tag when a print ends.';
            else if (!current && s.material) hint = 'The printer reports ' + escHtml(s.material) + ' here.';
            return '<div class="sp-row"><div class="sp-slot"><span class="sp-dot"' + (swatch ? ' style="background:' + swatch + '"' : '') + '></span><span class="truncate">' + escHtml(s.label) + '</span></div>'
                + '<div>' + spSelectHtml(key, current, p.name + ', ' + s.label) + (hint ? '<p class="sp-hint">' + hint + '</p>' : '') + '</div></div>';
        }).join('');
    }).join('');
    return rows;
}

function spLogHtml(log) {
    if (!log.length) return '<p class="text-sm text-text-2">Nothing has been sent yet. Prints that end show up here.</p>';
    const label = { ok: 'Sent', retrying: 'Retrying', error: 'Failed' };
    return log.map(e => {
        const amount = (e.unit === 'mm' ? Math.round(e.amount / 10) / 100 + ' m' : (Math.round(e.amount * 10) / 10) + ' g');
        const when = new Date(e.time * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
        return '<div class="sp-log sp-' + escHtml(e.status) + '"><div class="sp-log-line"><span class="sp-num">' + escHtml(when) + ' · <span data-no-i18n>' + escHtml(e.printer) + '</span> · ' + escHtml(amount) + ' to spool #' + e.spool + '</span>'
            + '<span class="sp-chip">' + (label[e.status] || escHtml(e.status)) + '</span></div>'
            + (e.error ? '<p class="sp-hint">' + escHtml(e.error) + '</p>' : '') + '</div>';
    }).join('');
}

function spRenderMapping() {
    const box = sp.root.querySelector('#spMapping');
    let note = '';
    const printers = (typeof configuredPrinters !== 'undefined' && configuredPrinters) || [];
    if (printers.some(p => p.moonraker_url)) note = '<p class="sp-hint mt-3">Klipper printers: Moonraker’s own Spoolman support tracks per extrusion and is more exact. Use that if you have it.</p>';
    const err = sp.spoolsError ? '<p class="text-sm text-danger flex items-center gap-3 flex-wrap">' + escHtml(sp.spoolsError) + '<button type="button" class="btn-outline btn-xs" data-sp-act="reload">Try again</button></p>' : '';
    // Rebuilding the selects drops keyboard focus; put it back on the same slot.
    const focused = box.contains(document.activeElement) ? document.activeElement.dataset.spSlot : null;
    box.innerHTML = err + spMappingHtml() + note;
    if (focused) {
        const again = Array.from(box.querySelectorAll('[data-sp-slot]')).find(el => el.dataset.spSlot === focused);
        if (again) again.focus();
    }
}

async function spLoadSpools() {
    sp.spools = null;
    sp.spoolsError = '';
    if (!sp.cfg.url) { spRenderMapping(); return; }
    try { sp.spools = await apiFetch('/api/spoolman/spools'); }
    catch (e) { sp.spoolsError = friendlyError(e.message); }
    spRenderMapping();
}

async function spRefreshLog() {
    try {
        sp.cfg = Object.assign(sp.cfg, { log: (await apiFetch('/api/spoolman')).log });
        sp.root.querySelector('#spLog').innerHTML = spLogHtml(sp.cfg.log);
    } catch (_) { /* the list stays as it was */ }
}

async function spLoad() {
    const body = sp.root.querySelector('#spBody');
    try {
        sp.cfg = await apiFetch('/api/spoolman');
    } catch (e) {
        body.classList.add('hidden');
        sp.root.querySelector('#spLoadError').classList.remove('hidden');
        return;
    }
    sp.root.querySelector('#spLoadError').classList.add('hidden');
    body.classList.remove('hidden');
    setSwitch('spEnabled', sp.cfg.enabled);
    sp.root.querySelector('#spUrl').value = sp.cfg.url || '';
    sp.root.querySelector('#spLog').innerHTML = spLogHtml(sp.cfg.log);
    sp.root.querySelector('#spMapping').innerHTML = '<div class="sp-skel"></div><div class="sp-skel"></div>';
    await spLoadSpools();
}

async function spTest() {
    const out = sp.root.querySelector('#spTestMsg'), btn = sp.root.querySelector('#spTest');
    btn.disabled = true;
    out.textContent = 'Trying…';
    out.className = 'text-xs text-text-2 mt-1.5';
    try {
        const r = await apiFetch('/api/spoolman/test', { method: 'POST', body: JSON.stringify({ url: sp.root.querySelector('#spUrl').value.trim() }) });
        out.textContent = r.message;
        out.className = 'text-xs mt-1.5 ' + (r.ok ? 'text-text-2' : 'text-danger');
    } catch (e) {
        out.textContent = friendlyError(e.message);
        out.className = 'text-xs text-danger mt-1.5';
    }
    btn.disabled = false;
}

async function spSave() {
    const btn = sp.root.querySelector('#spSave'), err = sp.root.querySelector('#spError');
    err.classList.add('hidden');
    btn.disabled = true;
    try {
        const was = sp.cfg.url;
        const cfg = await apiFetch('/api/spoolman', { method: 'POST', body: JSON.stringify({ enabled: getSwitch('spEnabled'), url: sp.root.querySelector('#spUrl').value.trim() }) });
        sp.cfg = cfg;
        toast('Spoolman saved');
        if (cfg.url !== was || !sp.spools) await spLoadSpools();
    } catch (e) {
        err.textContent = friendlyError(e.message);
        err.classList.remove('hidden');
    }
    btn.disabled = false;
}

// A slot saves as soon as it changes; it goes back if the save fails.
async function spPick(select) {
    const key = select.dataset.spSlot, was = sp.cfg.slots[key] || 0, now = Number(select.value);
    if (now) sp.cfg.slots[key] = now; else delete sp.cfg.slots[key];
    try {
        await apiFetch('/api/spoolman', { method: 'POST', body: JSON.stringify({ slots: { [key]: now } }) });
        spRenderMapping();
    } catch (e) {
        if (was) sp.cfg.slots[key] = was; else delete sp.cfg.slots[key];
        toast(friendlyError(e.message), 'error');
        spRenderMapping();
    }
}

function spMount(section) {
    sp.root = section;
    section.innerHTML = `
<div class="flex items-start justify-between gap-3">
  <h2 class="font-heading text-xl font-medium leading-tight">Spoolman</h2>
  <button id="spEnabled" type="button" class="switch shrink-0 mt-1" role="switch" aria-checked="false" aria-label="Report filament to Spoolman" data-sp-act="toggle"><span class="switch-thumb"></span></button>
</div>
<p class="mt-1.5 text-sm text-text-2 flex items-center gap-1">After each print, the filament it used is subtracted from your spools in Spoolman.
  ${tip('Your Spoolman server is on your own network. The Bridge sends it the grams (or the length) used, per spool, and nothing else. It never goes to FoxTrack.', 'What is sent')}
</p>

<div id="spLoadError" class="hidden mt-4 text-sm text-danger flex items-center gap-3 flex-wrap">
  <span>Your Spoolman settings could not be loaded.</span>
  <button type="button" class="btn-outline btn-xs" data-sp-act="load">Try again</button>
</div>

<div id="spBody" class="hidden">
  <div class="mt-5">
    <label for="spUrl" class="field-label">Server address</label>
    <div class="sp-url">
      <input id="spUrl" type="text" class="field field-mono" autocomplete="off" placeholder="http://192.168.1.20:7912">
      <button type="button" id="spTest" class="btn-outline btn-lg shrink-0" data-sp-act="test">Test</button>
    </div>
    <p id="spTestMsg" class="text-xs text-text-2 mt-1.5" aria-live="polite"></p>
  </div>
  <p id="spError" class="hidden text-xs text-danger mt-4"></p>
  <div class="mt-4 flex items-center justify-end gap-3">
    <button type="button" id="spSave" class="btn-primary btn-lg" data-sp-act="save">Save</button>
  </div>

  <div class="mt-8">
    <h3 class="font-heading text-lg font-medium leading-tight">Spools</h3>
    <p class="mt-1 text-sm text-text-2">Pick the Spoolman spool in each slot. A slot left on Not tracked is skipped, unless its Bambu tag matches a spool.</p>
    <div id="spMapping"></div>
  </div>

  <div class="mt-8">
    <div class="flex items-center justify-between gap-3">
      <h3 class="font-heading text-lg font-medium leading-tight">Recent updates</h3>
      <button type="button" class="btn-ghost btn-xs" data-sp-act="log">Refresh</button>
    </div>
    <div id="spLog" class="mt-2" aria-live="polite"></div>
  </div>
</div>`;

    section.addEventListener('click', e => {
        const b = e.target.closest('[data-sp-act]');
        if (!b) return;
        const act = b.dataset.spAct;
        if (act === 'toggle') setSwitch('spEnabled', !getSwitch('spEnabled'));
        else if (act === 'test') spTest();
        else if (act === 'save') spSave();
        else if (act === 'reload') spLoadSpools();
        else if (act === 'load') spLoad();
        else if (act === 'log') spRefreshLog();
    });
    section.addEventListener('change', e => {
        const sel = e.target.closest('[data-sp-slot]');
        if (sel) spPick(sel);
    });
    spLoad();
}

registerSettingsTab({
    id: 'spoolman', label: 'Spoolman', group: 'Connect', order: 20,
    mount: spMount,
    onShow() { if (sp.cfg) { spRefreshLog(); spRenderMapping(); } else spLoad(); }, // trays change as filament is loaded
});
