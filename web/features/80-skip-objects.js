// Skip objects: cancel single objects on the plate during a print, on Bambu
// (LAN Only + Developer Mode) and Klipper (with [exclude_object]). The card's
// "Skip objects" button (web/ui.html) calls openSkipObjects(encodedPrinterId).
// Server side: skip_routes.go, GET /api/objects/{id} and POST /api/objects/{id}/skip.

const SK = { id: '', name: '', brand: '', view: 'loading', data: null, picked: new Set(), seq: 0, ctl: null, timer: null, busy: false, error: '' };

const SK_CSS = `
#skipObjectsModal { position: fixed; inset: 0; z-index: 49; display: flex; align-items: flex-end; justify-content: center; }
#skipObjectsModal.hidden { display: none; }
#skipObjectsModal .sk-sheet { width: 100%; max-height: 92vh; max-height: 92dvh; display: flex; flex-direction: column; background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius) var(--radius) 0 0; box-shadow: 0 10px 40px color-mix(in srgb, var(--text) 18%, transparent); }
@media (min-width: 640px) {
  #skipObjectsModal { align-items: center; padding: 1rem; }
  #skipObjectsModal .sk-sheet { max-width: 34rem; border-radius: var(--radius); }
}
#skipObjectsModal .sk-head { padding: 1.25rem 1.25rem .5rem; }
#skipObjectsModal .sk-body { overflow-y: auto; padding: .25rem 1.25rem 1rem; }
#skipObjectsModal .sk-foot { display: flex; gap: .5rem; justify-content: flex-end; padding: .75rem 1.25rem 1.25rem; border-top: 1px solid var(--border); }
#skipObjectsModal .sk-foot button { min-height: 44px; font-variant-numeric: tabular-nums; }
#skipObjectsModal .sk-note { font-size: .8125rem; line-height: 1.5; color: var(--text-2); margin: 0 0 .75rem; }
#skipObjectsModal .sk-error { font-size: .8125rem; line-height: 1.5; color: var(--danger); margin: 0 1.25rem .75rem; }
#skipObjectsModal .sk-plate { border: 1px solid var(--border); background: var(--panel); border-radius: var(--radius); margin-bottom: .75rem; overflow: hidden; }
#skipObjectsModal .sk-plate svg { display: block; width: 100%; height: auto; max-height: 40vh; color: var(--text-2); }
#skipObjectsModal .sk-bed { fill: var(--surface); stroke: var(--border); stroke-width: 1; vector-effect: non-scaling-stroke; }
#skipObjectsModal .sk-shape { fill: var(--panel-2); stroke: var(--text-2); stroke-width: 1.5; vector-effect: non-scaling-stroke; cursor: pointer; }
#skipObjectsModal .sk-shape.is-cur { stroke: var(--accent); stroke-width: 2.5; fill: color-mix(in srgb, var(--accent) 25%, transparent); }
#skipObjectsModal .sk-shape.is-sel { stroke: var(--danger); stroke-width: 2.5; fill: color-mix(in srgb, var(--danger) 25%, transparent); }
#skipObjectsModal .sk-shape.is-skip { fill: url(#skHatch); stroke-dasharray: 4 3; opacity: .55; cursor: default; }
#skipObjectsModal .sk-num-svg { fill: var(--text); text-anchor: middle; dominant-baseline: central; pointer-events: none; font-variant-numeric: tabular-nums; }
#skipObjectsModal .sk-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: .375rem; }
#skipObjectsModal .sk-row { display: flex; align-items: center; gap: .75rem; min-height: 44px; padding: .375rem .75rem; border: 1px solid var(--border); border-radius: var(--radius); cursor: pointer; font-size: .875rem; }
#skipObjectsModal .sk-row:hover { background: var(--panel); }
#skipObjectsModal .sk-row.is-sel { border-color: color-mix(in srgb, var(--danger) 60%, transparent); }
#skipObjectsModal .sk-row.is-skip { cursor: default; color: var(--text-2); }
#skipObjectsModal .sk-row.is-skip:hover { background: transparent; }
#skipObjectsModal .sk-row input { width: 1.125rem; height: 1.125rem; flex-shrink: 0; accent-color: var(--accent); cursor: inherit; }
#skipObjectsModal .sk-row:focus-within { border-color: var(--accent); }
#skipObjectsModal .sk-n { flex-shrink: 0; min-width: 1.5rem; height: 1.5rem; padding: 0 .25rem; display: inline-flex; align-items: center; justify-content: center; border: 1px solid var(--border); border-radius: 9999px; font-size: .6875rem; font-variant-numeric: tabular-nums; color: var(--text-2); }
#skipObjectsModal .sk-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
#skipObjectsModal .sk-tag { flex-shrink: 0; font-size: .6875rem; padding: .125rem .5rem; border: 1px solid var(--border); border-radius: 9999px; color: var(--text-2); }
#skipObjectsModal .sk-tag.is-now { border-color: color-mix(in srgb, var(--accent) 60%, transparent); color: var(--accent); }
#skipObjectsModal .sk-skel { background: var(--panel-2); border-radius: var(--radius); margin-bottom: .5rem; animation: skPulse 1.4s ease-in-out infinite; }
@keyframes skPulse { 50% { opacity: .5; } }
@media (prefers-reduced-motion: reduce) { #skipObjectsModal .sk-skel { animation: none; } }
`;

function skEl(id) { return document.getElementById(id); }

function skBuild() {
    if (skEl('skipObjectsModal')) return;
    const st = document.createElement('style');
    st.textContent = SK_CSS;
    document.head.appendChild(st);
    const m = document.createElement('div');
    m.id = 'skipObjectsModal';
    m.className = 'hidden overlay-tint backdrop-blur-sm';
    m.innerHTML =
        '<div class="sk-sheet floating" role="dialog" aria-modal="true" aria-labelledby="skipTitle">'
        + '<div class="sk-head">'
        + '<h3 id="skipTitle" class="font-heading text-xl font-medium leading-tight">Skip objects</h3>'
        + '<p id="skipSub" class="sk-note" style="margin:.25rem 0 0"></p>'
        + '</div>'
        + '<div id="skipBody" class="sk-body" aria-live="polite"></div>'
        + '<p id="skipError" role="alert" class="sk-error hidden"></p>'
        + '<div class="sk-foot">'
        + '<button type="button" id="skipClose" class="btn-outline btn-lg">Close</button>'
        + '<button type="button" id="skipGo" class="btn-danger btn-lg hidden" disabled></button>'
        + '</div>'
        + '</div>';
    document.body.appendChild(m);
    m.addEventListener('click', e => { if (e.target === m) skClose(); });
    m.addEventListener('keydown', skTrapTab);
    skEl('skipClose').onclick = skClose;
    skEl('skipGo').onclick = skSubmit;
    const body = skEl('skipBody');
    body.addEventListener('change', e => {
        const i = e.target.getAttribute && e.target.getAttribute('data-i');
        if (i !== null && i !== undefined && e.target.type === 'checkbox') skToggle(Number(i), e.target.checked);
    });
    body.addEventListener('click', e => {
        const sh = e.target.closest && e.target.closest('.sk-shape');
        if (sh) skToggle(Number(sh.getAttribute('data-i')));
        if (e.target.id === 'skipRetry') skLoad();
    });
    if (typeof MODAL_CLOSERS !== 'undefined') MODAL_CLOSERS.push(['skipObjectsModal', skClose]);
}

// Keep Tab inside the sheet while it is open.
function skTrapTab(e) {
    if (e.key !== 'Tab') return;
    const items = Array.from(skEl('skipObjectsModal').querySelectorAll('button, input, [tabindex]'))
        .filter(x => !x.disabled && !x.classList.contains('hidden') && x.getAttribute('tabindex') !== '-1' && x.offsetParent !== null);
    if (!items.length) return;
    const first = items[0], last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
}

window.openSkipObjects = function (enc) {
    skBuild();
    SK.id = decodePrinterId(enc);
    SK.name = printerNameForId(SK.id);
    SK.brand = printerBrand(configuredPrinters.find(p => p.id === SK.id));
    SK.picked = new Set();
    SK.error = '';
    SK.busy = false;
    skEl('skipSub').textContent = 'On ' + SK.name;
    openModal('skipObjectsModal');
    skEl('skipClose').focus();
    clearInterval(SK.timer);
    SK.timer = setInterval(skTick, 15000);
    skLoad();
};

function skClose() {
    SK.seq++;
    if (SK.ctl) SK.ctl.abort();
    clearInterval(SK.timer);
    hideModal('skipObjectsModal');
}

function skUrl() { return '/api/objects/' + encodeURIComponent(SK.id); }

// First load (and Try again): skeleton while the Bridge reads the file.
async function skLoad() {
    const seq = ++SK.seq;
    if (SK.ctl) SK.ctl.abort();
    SK.ctl = new AbortController();
    SK.view = 'loading';
    SK.error = '';
    skRender();
    try {
        const d = await apiFetch(skUrl(), { signal: SK.ctl.signal });
        if (seq !== SK.seq) return;
        skApply(d);
    } catch (e) {
        if (seq !== SK.seq) return;
        SK.view = 'error';
        SK.error = friendlyError(e.message);
    }
    skRender();
}

// Quiet refresh while the sheet is open: the current object moves on Klipper,
// and a print can end.
async function skTick() {
    const confirmOpen = !skEl('confirmModal').classList.contains('hidden');
    if (SK.busy || SK.view !== 'list' || confirmOpen || document.hidden) return;
    const seq = SK.seq;
    try {
        const d = await apiFetch(skUrl(), { signal: SK.ctl ? SK.ctl.signal : undefined });
        if (seq !== SK.seq || SK.busy) return;
        if (JSON.stringify(d) === JSON.stringify(SK.data)) return;
        skApply(d);
        skRender();
    } catch (_) { /* the next tick tries again */ }
}

function skApply(d) {
    SK.data = d;
    if (!d.supported) { SK.view = 'unsupported'; return; }
    SK.view = 'list';
    const live = new Set((d.objects || []).filter(o => !o.skipped).map(o => o.id));
    SK.picked = new Set([...SK.picked].filter(id => live.has(id)));
}

// Extent of everything drawn, in the server's picture coordinates (origin top left).
function skExtent(d) {
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    const grow = (x, y) => { x0 = Math.min(x0, x); y0 = Math.min(y0, y); x1 = Math.max(x1, x); y1 = Math.max(y1, y); };
    for (const o of d.objects) {
        if (o.polygon && o.polygon.length) o.polygon.forEach(p => grow(p[0], p[1]));
        else if (o.bbox && o.bbox.length === 4) { grow(o.bbox[0], o.bbox[1]); grow(o.bbox[2], o.bbox[3]); }
    }
    if (!isFinite(x0)) return null;
    if (d.bed && d.bed.w > 0 && d.bed.h > 0) { x0 = Math.min(x0, 0); y0 = Math.min(y0, 0); x1 = Math.max(x1, d.bed.w); y1 = Math.max(y1, d.bed.h); }
    return { x0, y0, w: x1 - x0 || 1, h: y1 - y0 || 1 };
}

function skPlateSVG(d) {
    const ex = skExtent(d);
    if (!ex) return '';
    const big = Math.max(ex.w, ex.h);
    const pad = big * 0.05;
    const f = n => Number(n).toFixed(2);
    const hatch = big / 70;
    let svg = '<svg viewBox="' + [f(ex.x0 - pad), f(ex.y0 - pad), f(ex.w + 2 * pad), f(ex.h + 2 * pad)].join(' ')
        + '" role="img" aria-label="Top-down view of the plate. Use the list below to choose objects.">'
        + '<defs><pattern id="skHatch" patternUnits="userSpaceOnUse" width="' + f(hatch) + '" height="' + f(hatch) + '" patternTransform="rotate(45)">'
        + '<line x1="0" y1="0" x2="0" y2="' + f(hatch) + '" stroke="currentColor" stroke-width="1" vector-effect="non-scaling-stroke"/></pattern></defs>';
    if (d.bed && d.bed.w > 0 && d.bed.h > 0) svg += '<rect class="sk-bed" x="0" y="0" width="' + f(d.bed.w) + '" height="' + f(d.bed.h) + '"/>';
    d.objects.forEach((o, i) => {
        const cls = skShapeClass(o);
        const title = '<title>' + escHtml((i + 1) + ' ' + (o.name || '')) + '</title>';
        let cx, cy, shape;
        if (o.polygon && o.polygon.length > 2) {
            shape = '<polygon class="' + cls + '" data-i="' + i + '" points="' + o.polygon.map(p => f(p[0]) + ',' + f(p[1])).join(' ') + '">' + title + '</polygon>';
            const xs = o.polygon.map(p => p[0]), ys = o.polygon.map(p => p[1]);
            cx = (Math.min(...xs) + Math.max(...xs)) / 2; cy = (Math.min(...ys) + Math.max(...ys)) / 2;
        } else if (o.bbox && o.bbox.length === 4) {
            const b = o.bbox;
            shape = '<rect class="' + cls + '" data-i="' + i + '" x="' + f(b[0]) + '" y="' + f(b[1]) + '" width="' + f(b[2] - b[0]) + '" height="' + f(b[3] - b[1]) + '" rx="' + f(big / 120) + '">' + title + '</rect>';
            cx = (b[0] + b[2]) / 2; cy = (b[1] + b[3]) / 2;
        } else return;
        svg += shape + '<text class="sk-num-svg" x="' + f(cx) + '" y="' + f(cy) + '" font-size="' + f(big * 0.045) + '">' + (i + 1) + '</text>';
    });
    return '<div class="sk-plate">' + svg + '</svg></div>';
}

function skShapeClass(o) {
    return 'sk-shape' + (o.skipped ? ' is-skip' : SK.picked.has(o.id) ? ' is-sel' : o.current ? ' is-cur' : '');
}

function skRowHTML(o, i) {
    const picked = SK.picked.has(o.id);
    return '<li><label class="sk-row' + (o.skipped ? ' is-skip' : picked ? ' is-sel' : '') + '">'
        + '<input type="checkbox" data-i="' + i + '"' + (o.skipped ? ' disabled' : '') + (picked ? ' checked' : '') + '>'
        + '<span class="sk-n" aria-hidden="true">' + (i + 1) + '</span>'
        + '<span class="sk-name" title="' + escHtml(o.name) + '">' + escHtml(o.name || ('Object ' + o.id)) + '</span>'
        + (o.skipped ? '<span class="sk-tag">Skipped</span>' : o.current ? '<span class="sk-tag is-now">Printing now</span>' : '')
        + '</label></li>';
}

function skRender() {
    const body = skEl('skipBody');
    const keep = document.activeElement && body.contains(document.activeElement) ? document.activeElement.getAttribute('data-i') : null;
    const d = SK.data;
    let html = '';
    if (SK.view === 'loading') {
        html = '<div role="status"><p class="sk-note">'
            + (SK.brand === 'bambu' ? 'Reading the print file from the printer. This can take about 10 seconds.' : 'Reading the object list from the printer.')
            + '</p><div class="sk-skel" style="height:10rem"></div><div class="sk-skel" style="height:2.75rem"></div><div class="sk-skel" style="height:2.75rem"></div></div>';
    } else if (SK.view === 'error') {
        html = '<p class="sk-note" style="color:var(--danger)">' + escHtml(SK.error) + '</p>'
            + '<button type="button" id="skipRetry" class="btn-outline btn-lg">Try again</button>';
    } else if (SK.view === 'unsupported') {
        html = '<p class="sk-note">' + escHtml(d.reason || 'This printer cannot skip objects right now.') + '</p>';
    } else {
        const left = d.objects.filter(o => !o.skipped).length;
        html = '<p class="sk-note">' + d.objects.length + (d.objects.length === 1 ? ' object' : ' objects') + ' on the plate, ' + left + ' still to print. '
            + 'Tick the ones to cancel. The rest of the print carries on, and a skipped object cannot be brought back.</p>'
            + skPlateSVG(d)
            + '<ul class="sk-list" aria-label="Objects on the plate">' + d.objects.map(skRowHTML).join('') + '</ul>';
    }
    body.innerHTML = html;
    if (keep !== null) { const el = body.querySelector('[data-i="' + keep + '"]:not(.sk-shape)'); if (el) el.focus(); }
    skSync();
}

// Cheap update after a tick or a click: classes, error line and the Skip button.
function skSync() {
    const d = SK.data;
    const body = skEl('skipBody');
    const inList = SK.view === 'list';
    if (inList) {
        d.objects.forEach((o, i) => {
            const sh = body.querySelector('.sk-shape[data-i="' + i + '"]');
            if (sh) sh.setAttribute('class', skShapeClass(o));
            const cb = body.querySelector('input[data-i="' + i + '"]');
            if (cb) { cb.checked = SK.picked.has(o.id); cb.closest('.sk-row').classList.toggle('is-sel', SK.picked.has(o.id)); }
        });
    }
    const err = skEl('skipError');
    err.textContent = SK.error && SK.view === 'list' ? SK.error : '';
    err.classList.toggle('hidden', !(SK.error && SK.view === 'list'));
    const go = skEl('skipGo');
    go.classList.toggle('hidden', !inList);
    const n = SK.picked.size;
    go.disabled = !n || SK.busy;
    go.innerHTML = (SK.busy ? '<i class="fas fa-circle-notch fa-spin mr-1"></i>' : '') + (n ? 'Skip ' + n + (n === 1 ? ' object' : ' objects') : 'Skip objects');
}

function skToggle(i, want) {
    const o = SK.data && SK.data.objects[i];
    if (!o || o.skipped || SK.busy) return;
    const on = want === undefined ? !SK.picked.has(o.id) : want;
    if (on) SK.picked.add(o.id); else SK.picked.delete(o.id);
    SK.error = '';
    skSync();
}

function skNames(list) {
    const n = list.map(o => o.name || ('Object ' + o.id));
    if (n.length === 1) return n[0];
    if (n.length <= 4) return n.slice(0, -1).join(', ') + ' and ' + n[n.length - 1];
    return n.slice(0, 3).join(', ') + ' and ' + (n.length - 3) + ' more';
}

async function skSubmit() {
    if (SK.busy || !SK.picked.size) return;
    const objects = SK.data.objects;
    const picked = objects.filter(o => SK.picked.has(o.id));
    const n = picked.length;
    const everyOne = n === objects.filter(o => !o.skipped).length;
    const ok = await confirmDialog({
        title: n === 1 ? 'Skip this object?' : 'Skip ' + n + ' objects?',
        body: skNames(picked) + ' will not be printed. The rest of the plate carries on. This cannot be undone.'
            + (everyOne ? ' That is every object left, so nothing more will print. Use Stop if you want to end the print.' : ''),
        confirmLabel: n === 1 ? 'Skip object' : 'Skip ' + n + ' objects',
    });
    if (!ok || skEl('skipObjectsModal').classList.contains('hidden')) return;
    const seq = SK.seq;
    SK.busy = true;
    SK.error = '';
    skSync();
    try {
        const d = await apiFetch(skUrl() + '/skip', { method: 'POST', body: JSON.stringify({ ids: picked.map(o => o.id) }) });
        if (seq !== SK.seq) return;
        SK.picked = new Set();
        skApply(d);
        skRender();
        toast(n === 1 ? 'Object skipped' : n + ' objects skipped');
    } catch (e) {
        if (seq !== SK.seq) return;
        SK.error = friendlyError(e.message);
    } finally {
        if (seq === SK.seq) { SK.busy = false; skSync(); }
    }
}
