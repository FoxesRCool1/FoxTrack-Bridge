// Settings > Timelapse: the on/off switch and its options, then a gallery of
// the videos the Bridge has built. Served by /api/timelapse (timelapse_routes.go).

const TL_UNDO_MS = 8000;
const TL_REFRESH_MS = 10000;

const tl = {
    section: null,
    data: null,        // last answer from GET /api/timelapse
    loadSeq: 0,
    filled: false,     // the form holds the saved settings
    pending: null,     // { id, timer }: a delete waiting out its undo window
};

function tlStyle() {
    if (document.getElementById('tl-style')) return;
    const s = document.createElement('style');
    s.id = 'tl-style';
    s.textContent = `
#settings-timelapse .tl-grid, #settings-timelapse .tl-gallery { display: grid; grid-template-columns: minmax(0, 1fr); gap: .75rem; }
@media (min-width: 640px) { #settings-timelapse .tl-grid, #settings-timelapse .tl-gallery { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
#settings-timelapse .tl-card { min-width: 0; overflow: hidden; border: 1px solid var(--border); border-radius: 12px; background: var(--panel-2); }
#settings-timelapse .tl-media { display: block; width: 100%; aspect-ratio: 16 / 9; object-fit: contain; background: var(--bg); }
#settings-timelapse .tl-body { padding: .75rem; }
#settings-timelapse .tl-check { display: flex; align-items: center; gap: .75rem; min-height: 2.75rem; font-size: .875rem; cursor: pointer; }
#settings-timelapse .tl-check input { width: 1.125rem; height: 1.125rem; flex-shrink: 0; }
#settings-timelapse .tl-num { font-variant-numeric: tabular-nums; }
#settings-timelapse .tl-skel { border: 1px solid var(--border); border-radius: 12px; background: var(--panel-2); overflow: hidden; }
#settings-timelapse .tl-skel-media { aspect-ratio: 16 / 9; background: var(--panel); }
#settings-timelapse .tl-skel-line { height: .75rem; margin: .75rem; border-radius: 6px; background: var(--panel); }
@media (max-width: 639px), (pointer: coarse) {
  #settings-timelapse .field { min-height: 2.75rem; }
  #settings-timelapse .tl-btn { min-height: 2.75rem; padding: 0 1rem; font-size: .8125rem; }
}
#settings-timelapse #tlEnabled { position: relative; }
#settings-timelapse #tlEnabled::after { content: ''; position: absolute; inset: -.75rem -.5rem; }
#tlUndo { position: fixed; left: 1rem; right: 1rem; bottom: 1rem; z-index: 200; display: flex; justify-content: center; pointer-events: none; }
#tlUndo > div { pointer-events: auto; display: flex; align-items: center; gap: .75rem; max-width: 28rem; width: 100%; justify-content: space-between; }
#tlUndo .tl-btn { min-height: 2.75rem; padding: 0 1rem; font-size: .8125rem; }
`;
    document.head.appendChild(s);
}

function tlFmtLength(sec) {
    sec = Math.max(0, Math.round(sec || 0));
    return Math.floor(sec / 60) + ':' + String(sec % 60).padStart(2, '0');
}

function tlFmtSize(bytes) {
    if (bytes >= 1048576) return (bytes / 1048576).toFixed(1) + ' MB';
    return Math.max(1, Math.round(bytes / 1024)) + ' KB';
}

function tlFmtDate(unix) {
    return new Date(unix * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
}

function tlFileName(m) {
    const base = (m.printer_name + ' ' + (m.file || '').replace(/\.[^.]*$/, '') + ' ' + new Date(m.end * 1000).toISOString().slice(0, 10))
        .replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '');
    return (base || 'timelapse') + '.' + m.format;
}

function tlSkeleton() {
    const card = '<div class="tl-skel" aria-hidden="true"><div class="pulse tl-skel-media"></div><div class="pulse tl-skel-line" style="width:55%"></div><div class="pulse tl-skel-line" style="width:35%"></div></div>';
    return '<div class="tl-gallery" role="status" aria-label="Loading timelapses">' + card + card + '</div>';
}

function tlMount(section) {
    tl.section = section;
    tlStyle();
    section.innerHTML = `
<div class="flex items-start justify-between gap-3">
    <h2 class="font-heading text-xl font-medium leading-tight">Timelapse</h2>
    <button id="tlEnabled" type="button" class="switch shrink-0 mt-1" role="switch" aria-checked="false" aria-label="Record timelapses"><span class="switch-thumb"></span></button>
</div>
<p class="mt-1.5 text-sm text-text-2">Takes a picture from each printer's camera while it prints and turns them into a video when the print ends. The pictures and videos stay on this machine.</p>
<p id="tlFFmpegNote" class="hidden mt-3 text-xs leading-relaxed text-warning">ffmpeg isn't installed, so timelapses are saved as animated GIFs: bigger files and lower quality. Cameras on X1, H2 and P2S printers need ffmpeg to show at all. Install it (see the README) and restart the Bridge.</p>

<div class="mt-5 tl-grid">
    <div>
        <div class="label-row">
            <label for="tlInterval" class="field-label">Picture every (seconds)</label>
            ${tip('A picture every 30 seconds turns a 3 hour print into about 12 seconds of video. Shorter gaps look smoother and use more disk space. From 5 to 3600.', 'About the interval')}
        </div>
        <input id="tlInterval" type="number" inputmode="numeric" min="5" max="3600" step="1" class="field tl-num" value="30">
    </div>
    <div>
        <div class="label-row">
            <label for="tlKeep" class="field-label">Keep videos for (days)</label>
            ${tip('Older videos are deleted automatically. 0 keeps them until you delete them yourself.', 'About keeping videos')}
        </div>
        <input id="tlKeep" type="number" inputmode="numeric" min="0" max="3650" step="1" class="field tl-num" value="30">
    </div>
</div>

<div class="mt-4">
    <p class="field-label">Printers</p>
    <label class="tl-check"><input type="checkbox" id="tlAll"> All printers with a camera</label>
    <div id="tlPrinters" class="hidden"></div>
</div>

<p id="tlError" class="hidden text-xs text-danger mt-4" role="alert"></p>
<button id="tlRetry" type="button" class="hidden btn-outline btn-xs tl-btn mt-2" data-tl-retry>Try again</button>
<div class="mt-5 flex items-center justify-end gap-3">
    <span id="tlStatus" class="text-xs text-text-2" role="status" aria-live="polite"></span>
    <button id="tlSave" type="button" class="btn-primary btn-lg tl-btn">Save</button>
</div>

<div class="mt-8 border-t border-border pt-5">
    <h3 class="font-heading text-lg font-medium leading-tight">Recorded timelapses</h3>
    <p id="tlLive" class="hidden mt-1.5 text-xs text-text-2 tl-num" role="status" aria-live="polite"></p>
    <div id="tlGallery" class="mt-4">${tlSkeleton()}</div>
</div>`;

    document.getElementById('tlEnabled').addEventListener('click', () => setSwitch('tlEnabled', !getSwitch('tlEnabled')));
    document.getElementById('tlAll').addEventListener('change', tlShowPrinterList);
    document.getElementById('tlSave').addEventListener('click', tlSave);
    section.addEventListener('click', (e) => {
        const del = e.target.closest('[data-tl-delete]');
        if (del) tlDelete(del.dataset.tlDelete);
        if (e.target.closest('[data-tl-retry]')) tlLoad(false);
    });
    window.addEventListener('pagehide', tlFlushDelete);
    tlLock(true);
    setInterval(() => {
        if (!document.hidden && tl.section.offsetParent !== null) tlLoad(false);
    }, TL_REFRESH_MS);
    tlLoad(true);
}

function tlOnShow() {
    // Redraw the list in case printers were added; keep what is ticked, or the
    // saved choice if the list was empty when the form was filled.
    const box = document.getElementById('tlPrinters');
    tlRenderPrinters(box.querySelector('input') ? tlSelectedIds() : (tl.data ? tl.data.printers : []));
    tlLoad(false);
}

// The printers that can record, as checkboxes. The saved choice is kept
// across redraws.
function tlRenderPrinters(selected) {
    const box = document.getElementById('tlPrinters');
    if (!box) return;
    const list = (typeof configuredPrinters !== 'undefined' ? configuredPrinters : []).filter(p => !p.camera_hidden);
    box.innerHTML = list.length
        ? list.map(p => '<label class="tl-check"><input type="checkbox" value="' + escHtml(p.id) + '"' + (selected.includes(p.id) ? ' checked' : '') + (tl.filled ? '' : ' disabled') + '> <span class="min-w-0 truncate" data-no-i18n>' + escHtml(p.name) + '</span></label>').join('')
        : '<p class="text-xs text-text-2 py-2">No printer has its camera shown on the dashboard.</p>';
}

// Until the saved settings have loaded, Save would post the defaults over them.
function tlLock(on) {
    ['tlSave', 'tlEnabled', 'tlInterval', 'tlKeep', 'tlAll'].forEach(id => { document.getElementById(id).disabled = on; });
    document.querySelectorAll('#tlPrinters input').forEach(i => { i.disabled = on; });
}

function tlSelectedIds() {
    const box = document.getElementById('tlPrinters');
    return box ? Array.from(box.querySelectorAll('input:checked')).map(i => i.value) : [];
}

function tlShowPrinterList() {
    document.getElementById('tlPrinters').classList.toggle('hidden', document.getElementById('tlAll').checked);
}

// withForm fills the settings from the server; leave it off for a refresh, so
// a half-edited form is not overwritten.
async function tlLoad(withForm) {
    const seq = ++tl.loadSeq;
    let data;
    try {
        data = await apiFetch('/api/timelapse');
    } catch (e) {
        if (seq !== tl.loadSeq) return;
        if (!tl.filled) {
            const err = document.getElementById('tlError');
            err.textContent = "Couldn't load these settings: " + e.message;
            err.classList.remove('hidden');
            document.getElementById('tlRetry').classList.remove('hidden');
        }
        if (!tl.data) {
            document.getElementById('tlGallery').dataset.sig = '';
            document.getElementById('tlGallery').innerHTML = '<p class="text-sm text-danger">Couldn\'t load timelapses: ' + escHtml(e.message)
                + ' <button type="button" class="btn-outline btn-xs tl-btn ml-2" data-tl-retry>Try again</button></p>';
        }
        return;
    }
    if (seq !== tl.loadSeq) return;
    tl.data = data;
    if (withForm || !tl.filled) {
        tlFillForm(data);
        tl.filled = true;
        tlLock(false);
        document.getElementById('tlError').classList.add('hidden');
        document.getElementById('tlRetry').classList.add('hidden');
    }
    document.getElementById('tlFFmpegNote').classList.toggle('hidden', !!data.ffmpeg);
    tlRenderLive(data);
    tlRenderGallery();
}

function tlFillForm(d) {
    setSwitch('tlEnabled', d.enabled);
    document.getElementById('tlInterval').value = d.interval_sec;
    document.getElementById('tlKeep').value = d.keep_days;
    const all = !d.printers.length;
    document.getElementById('tlAll').checked = all;
    tlRenderPrinters(d.printers);
    tlShowPrinterList();
}

function tlRenderLive(d) {
    const el = document.getElementById('tlLive');
    const parts = (d.recording || []).map(r => '<span data-no-i18n>' + escHtml(r.printer_name) + '</span>: ' + r.frames + (r.frames === 1 ? ' picture' : ' pictures') + ' so far');
    if (d.building) parts.push('Building ' + d.building + (d.building === 1 ? ' video' : ' videos') + '…');
    el.innerHTML = parts.join(' · ');
    el.classList.toggle('hidden', !parts.length);
}

function tlCard(m) {
    const url = '/api/timelapse/file/' + encodeURIComponent(m.id);
    const label = 'Timelapse of ' + (m.file || 'a print') + ' on ' + m.printer_name;
    const media = m.format === 'gif'
        ? '<img class="tl-media" src="' + url + '" alt="' + escHtml(label) + '" loading="lazy">'
        : '<video class="tl-media" controls preload="none" playsinline aria-label="' + escHtml(label) + '"' + (m.poster ? ' poster="' + url + '?poster=1"' : '') + ' src="' + url + '"></video>';
    const resultClass = m.result === 'failed' ? 'text-danger' : m.result === 'cancelled' ? 'text-warning' : 'text-text-2';
    return '<article class="tl-card">' + media + '<div class="tl-body">'
        + '<div class="flex items-start justify-between gap-2">'
        + '<div class="min-w-0"><p class="text-sm font-medium truncate" data-no-i18n>' + escHtml(m.printer_name) + '</p>'
        + '<p class="text-xs text-text-2 truncate"' + (m.file ? ' data-no-i18n' : '') + '>' + escHtml(m.file || 'Unnamed print') + '</p></div>'
        + '<span class="shrink-0 inline-flex items-center rounded-full border border-border px-2 py-0.5 text-[10px] ' + resultClass + '">' + escHtml(m.result) + '</span></div>'
        + '<p class="mt-1.5 text-xs text-text-2 tl-num">' + escHtml(tlFmtDate(m.end)) + ' · ' + tlFmtLength(m.duration_sec) + ' · ' + tlFmtSize(m.size) + '</p>'
        + '<div class="mt-3 flex items-center gap-2">'
        + '<a class="btn-outline btn-xs tl-btn" href="' + url + '" download="' + escHtml(tlFileName(m)) + '">Download</a>'
        + '<button type="button" class="btn-danger btn-xs tl-btn" data-tl-delete="' + escHtml(m.id) + '" aria-label="Delete the timelapse of ' + escHtml(m.file || 'this print') + ' on ' + escHtml(m.printer_name) + '">Delete</button>'
        + '</div></div></article>';
}

function tlRenderGallery() {
    const el = document.getElementById('tlGallery');
    const items = (tl.data.items || []).filter(m => !tl.pending || m.id !== tl.pending.id);
    if (!items.length) {
        el.dataset.sig = '';
        el.innerHTML = '<p class="text-sm text-text-2">' + (tl.data.enabled
            ? 'Nothing yet. A video appears here a little after a print ends, once it has at least 10 pictures.'
            : 'Nothing yet. Switch timelapse on above to record your next print.') + '</p>';
        return;
    }
    // Rebuilding the cards would stop a video that is playing, so redraw only
    // when the list changed.
    const sig = items.map(m => m.id).join(',');
    if (el.dataset.sig === sig) return;
    el.dataset.sig = sig;
    el.innerHTML = '<div class="tl-gallery">' + items.map(tlCard).join('') + '</div>';
}

async function tlSave() {
    const err = document.getElementById('tlError');
    const status = document.getElementById('tlStatus');
    const interval = parseInt(document.getElementById('tlInterval').value, 10);
    const keep = parseInt(document.getElementById('tlKeep').value, 10);
    const all = document.getElementById('tlAll').checked;
    const printers = all ? [] : tlSelectedIds();
    const fail = (msg) => { err.textContent = msg; err.classList.remove('hidden'); };
    err.classList.add('hidden');
    if (!(interval >= 5 && interval <= 3600)) return fail('Pick a picture interval from 5 to 3600 seconds.');
    if (!(keep >= 0 && keep <= 3650)) return fail('Keep videos for 0 to 3650 days. 0 keeps them all.');
    if (!all && !printers.length) return fail('Pick at least one printer, or choose all printers.');
    const btn = document.getElementById('tlSave');
    btn.disabled = true;
    status.textContent = 'Saving…';
    try {
        const d = await apiFetch('/api/timelapse', { method: 'POST', body: JSON.stringify({ enabled: getSwitch('tlEnabled'), interval_sec: interval, keep_days: keep, printers }) });
        tl.data = d;
        tlFillForm(d);
        tlRenderLive(d);
        tlRenderGallery();
        status.textContent = '';
        toast('Timelapse settings saved');
    } catch (e) {
        status.textContent = '';
        fail("Couldn't save: " + e.message);
    } finally {
        btn.disabled = false;
    }
}

// Deleting waits out an undo window: the card goes at once, the file only
// when the toast runs out.
function tlUndoBar() {
    let bar = document.getElementById('tlUndo');
    if (bar) return bar;
    bar = document.createElement('div');
    bar.id = 'tlUndo';
    bar.className = 'hidden';
    bar.setAttribute('role', 'status');
    bar.setAttribute('aria-live', 'polite');
    bar.innerHTML = '<div class="floating px-4 py-2.5 rounded-lg text-sm border bg-surface border-border text-text shadow-xl"><span>Timelapse deleted.</span><button type="button" class="btn-outline btn-xs tl-btn" id="tlUndoBtn">Undo</button></div>';
    document.body.appendChild(bar);
    bar.querySelector('#tlUndoBtn').addEventListener('click', tlUndo);
    return bar;
}

function tlDelete(id) {
    tlCommit(); // one undo at a time: an earlier delete is final now
    tl.pending = { id, timer: setTimeout(tlCommit, TL_UNDO_MS) };
    tlUndoBar().classList.remove('hidden');
    tlRenderGallery();
    document.getElementById('tlUndoBtn').focus(); // the delete button is gone; keep keyboard users in reach of Undo
}

function tlUndo() {
    if (!tl.pending) return;
    clearTimeout(tl.pending.timer);
    tl.pending = null;
    tlUndoBar().classList.add('hidden');
    tlRenderGallery();
}

async function tlCommit() {
    const p = tl.pending;
    if (!p) return;
    clearTimeout(p.timer);
    tlUndoBar().classList.add('hidden');
    try {
        await apiFetch('/api/timelapse/file/' + encodeURIComponent(p.id), { method: 'DELETE' });
    } catch (e) {
        if (!/does not exist/.test(e.message)) toast("Couldn't delete the timelapse: " + e.message, 'error');
    }
    if (tl.pending === p) tl.pending = null;
    tlLoad(false);
}

// Leaving the page inside the undo window: the delete still happens.
function tlFlushDelete() {
    if (!tl.pending) return;
    fetch('/api/timelapse/file/' + encodeURIComponent(tl.pending.id), { method: 'DELETE', keepalive: true, headers: { 'Content-Type': 'application/json' }, body: '{}' });
    tl.pending = null;
}

registerSettingsTab({ id: 'timelapse', label: 'Timelapse', group: 'Printing', order: 20, mount: tlMount, onShow: tlOnShow });
