// Maintenance: reminders that come due after some hours of printing. The hours
// are counted from each printer's print history on this machine.
const mt = { root: null, printers: null, open: new Set(), editing: null, pending: new Map() };

const mtStyle = document.createElement('style');
mtStyle.textContent = `
#settings-maintenance .mt-printer { margin-top: 1.5rem; }
#settings-maintenance .mt-head { display: flex; align-items: center; justify-content: space-between; gap: .75rem; padding-bottom: .625rem; }
#settings-maintenance .mt-task { padding: .75rem 0; border-top: 1px solid var(--border); }
#settings-maintenance .mt-line { display: flex; align-items: center; justify-content: space-between; gap: .75rem; }
#settings-maintenance .mt-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: .875rem; font-weight: 500; }
#settings-maintenance .mt-chip { flex-shrink: 0; font-size: .6875rem; padding: .125rem .5rem; border-radius: 9999px; border: 1px solid var(--border); color: var(--text-2); }
#settings-maintenance .mt-soon .mt-chip { border-color: color-mix(in srgb, var(--warning) 60%, transparent); color: var(--warning); }
#settings-maintenance .mt-due .mt-chip { border-color: color-mix(in srgb, var(--danger) 60%, transparent); color: var(--danger); }
#settings-maintenance .mt-bar { height: 4px; border-radius: 9999px; background: var(--panel-2); margin: .5rem 0; overflow: hidden; }
#settings-maintenance .mt-bar > span { display: block; height: 100%; background: var(--accent); transition: width 300ms ease; }
#settings-maintenance .mt-soon .mt-bar > span { background: var(--warning); }
#settings-maintenance .mt-due .mt-bar > span { background: var(--danger); }
#settings-maintenance .mt-hours { font-size: .75rem; color: var(--text-2); font-variant-numeric: tabular-nums; }
#settings-maintenance .mt-acts { display: flex; align-items: center; gap: .25rem; }
#settings-maintenance .mt-icon { width: 2rem; height: 2rem; display: inline-flex; align-items: center; justify-content: center; border: 0; background: transparent; color: var(--text-2); border-radius: var(--radius); cursor: pointer; }
#settings-maintenance .mt-icon:hover { background: var(--panel); color: var(--text); }
#settings-maintenance .mt-form { display: grid; grid-template-columns: 1fr; gap: .75rem; padding: .75rem; margin-top: .5rem; border: 1px solid var(--border); border-radius: var(--radius); }
@media (min-width: 640px) { #settings-maintenance .mt-form { grid-template-columns: 1fr 9rem; } #settings-maintenance .mt-form .mt-wide { grid-column: 1 / -1; } }
#settings-maintenance .mt-form-acts { display: flex; justify-content: flex-end; gap: .5rem; }
#settings-maintenance .mt-skel { height: 4.5rem; border-radius: var(--radius); background: var(--panel-2); margin-top: .75rem; }
@media (max-width: 640px) {
  #settings-maintenance .field, #settings-maintenance .btn-primary, #settings-maintenance .btn-outline, #settings-maintenance .btn-ghost { min-height: 2.75rem; }
  #settings-maintenance .mt-icon { width: 2.75rem; height: 2.75rem; }
  #settings-maintenance .btn-xs { min-height: 2.75rem; padding: 0 .875rem; }
}`;
document.head.appendChild(mtStyle);

const mtHours = h => { const n = Math.round(Number(h || 0) * 10) / 10; return String(n); };
const mtStatusLabel = { ok: 'OK', soon: 'Due soon', due: 'Due' };
const mtFind = id => { for (const p of mt.printers || []) { const t = p.tasks.find(t => t.id === id); if (t) return { p, t }; } return null; };

function mtTaskHtml(t) {
    const pct = Math.max(0, Math.min(100, t.percent || 0));
    if (mt.editing === t.id) {
        return '<div class="mt-task" data-task="' + escHtml(t.id) + '">' + mtFormHtml({ name: t.name, interval: t.interval_hours, id: t.id }) + '</div>';
    }
    const label = escHtml(t.name);
    return '<div class="mt-task mt-' + escHtml(t.status) + '" data-task="' + escHtml(t.id) + '">'
        + '<div class="mt-line"><span class="mt-name" data-no-i18n title="' + label + '">' + label + '</span><span class="mt-chip">' + (mtStatusLabel[t.status] || '') + '</span></div>'
        + '<div class="mt-bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' + pct + '" aria-label="' + label + ': ' + escHtml(mtHours(t.hours)) + ' of ' + escHtml(mtHours(t.interval_hours)) + ' hours"><span style="width:' + pct + '%"></span></div>'
        + '<div class="mt-line"><span class="mt-hours">' + escHtml(mtHours(t.hours)) + ' / ' + escHtml(mtHours(t.interval_hours)) + ' h</span>'
        + '<span class="mt-acts">'
        + '<button type="button" class="btn-outline btn-xs" data-mt-act="done" aria-label="Mark ' + label + ' as done">Done</button>'
        + '<button type="button" class="mt-icon" data-mt-act="edit" aria-label="Edit ' + label + '"><i class="fas fa-pencil"></i></button>'
        + '<button type="button" class="mt-icon" data-mt-act="delete" aria-label="Delete ' + label + '"><i class="fas fa-times"></i></button>'
        + '</span></div></div>';
}

// One form for adding and editing. A new task can start from a suggestion.
function mtFormHtml({ name = '', interval = '', id = '', presets = [] }) {
    const k = id || 'new';
    const pick = presets.length
        ? '<div class="mt-wide"><label class="field-label" for="mtPick-' + escHtml(k) + '">Start from a suggestion</label><div class="select-wrap"><select id="mtPick-' + escHtml(k) + '" class="field" data-mt-pick>'
            + '<option value="">Custom task</option>'
            + presets.map((p, i) => '<option data-no-i18n value="' + i + '">' + escHtml(p.name) + ' (every ' + escHtml(mtHours(p.interval_hours)) + ' h)</option>').join('')
            + '</select><i class="fas fa-chevron-down"></i></div></div>'
        : '';
    return '<div class="mt-form" data-form="' + escHtml(k) + '">' + pick
        + '<div><label class="field-label" for="mtName-' + escHtml(k) + '">Task</label><input id="mtName-' + escHtml(k) + '" type="text" class="field" maxlength="80" autocomplete="off" data-no-i18n data-mt-name value="' + escHtml(name) + '" placeholder="Oil the Z axis"></div>'
        + '<div><label class="field-label" for="mtEvery-' + escHtml(k) + '">Every (printing hours)</label><input id="mtEvery-' + escHtml(k) + '" type="text" inputmode="decimal" class="field" autocomplete="off" data-mt-every value="' + escHtml(interval) + '" placeholder="100"></div>'
        + '<p class="mt-wide text-xs text-danger hidden" data-mt-error></p>'
        + '<div class="mt-wide mt-form-acts"><button type="button" class="btn-ghost" data-mt-act="cancel">Cancel</button><button type="button" class="btn-primary" data-mt-act="save">' + (id ? 'Save' : 'Add task') + '</button></div></div>';
}

function mtPrinterHtml(p) {
    const tasks = p.tasks.filter(t => !mt.pending.has(t.id));
    const body = tasks.length
        ? tasks.map(mtTaskHtml).join('')
        : '<div class="mt-task"><p class="text-sm text-text-2">No reminders for this printer yet. The suggested tasks start counting from now.</p>'
            + '<button type="button" class="btn-primary mt-3" data-mt-act="suggested">Add the suggested tasks</button></div>';
    const adding = mt.open.has(p.id);
    return '<div class="mt-printer" data-printer="' + escHtml(p.id) + '">'
        + '<div class="mt-head"><div class="min-w-0"><h3 class="text-sm font-medium truncate" data-no-i18n>' + escHtml(p.name) + '</h3></div>'
        + (adding ? '' : '<button type="button" class="btn-outline btn-xs" data-mt-act="add">Add a task</button>') + '</div>'
        + body
        + (adding ? mtFormHtml({ presets: p.presets }) : '')
        + '</div>';
}

function mtRenderPrinter(id) {
    const p = mt.printers.find(p => p.id === id);
    const el = mt.root.querySelector('.mt-printer[data-printer="' + CSS.escape(id) + '"]');
    if (!p || !el) return mtRenderAll();
    el.outerHTML = mtPrinterHtml(p);
}

function mtRenderAll() {
    const box = mt.root.querySelector('#mtBody');
    if (!mt.printers.length) { box.innerHTML = '<p class="mt-5 text-sm text-text-2">Add a printer on the Printers page to set reminders for it.</p>'; return; }
    box.innerHTML = mt.printers.map(mtPrinterHtml).join('');
}

async function mtLoad(quiet) {
    const box = mt.root.querySelector('#mtBody');
    if (!quiet || !mt.printers) box.innerHTML = '<div class="mt-skel"></div><div class="mt-skel"></div>';
    try {
        const d = await apiFetch('/api/maintenance');
        mt.printers = d.printers || [];
        // A delete still in its undo window stays hidden across a reload.
        for (const t of mt.pending.values()) {
            const f = mtFind(t.id);
            if (f) f.p.tasks.push(t);
        }
        mt.open = new Set([...mt.open].filter(id => mt.printers.some(p => p.id === id)));
        mtRenderAll();
    } catch (e) {
        if (mt.printers) {
            // Keep showing what we have, but say the refresh failed.
            toast(friendlyError(e.message), 'error');
            mtRenderAll();
            return;
        }
        box.innerHTML = '<p class="mt-5 text-sm text-danger">The reminders could not be loaded: ' + escHtml(friendlyError(e.message)) + '</p>'
            + '<button type="button" class="btn-outline btn-xs mt-3" data-mt-act="retry">Try again</button>';
    }
}

async function mtMarkDone(id) {
    const f = mtFind(id);
    if (!f || f.t.busy) return;
    const before = { hours: f.t.hours, percent: f.t.percent, status: f.t.status };
    Object.assign(f.t, { hours: 0, percent: 0, status: 'ok', busy: true });
    mtRenderPrinter(f.p.id);
    try {
        const v = await apiFetch('/api/maintenance/' + encodeURIComponent(id) + '/done', { method: 'POST' });
        Object.assign(f.t, v, { busy: false });
        toast('Marked as done');
    } catch (e) {
        Object.assign(f.t, before, { busy: false });
        toast(friendlyError(e.message), 'error');
    }
    mtRenderPrinter(f.p.id);
    const again = mt.root.querySelector('.mt-task[data-task="' + CSS.escape(id) + '"] [data-mt-act="done"]');
    if (again) again.focus();
}

function mtDelete(id) {
    const f = mtFind(id);
    if (!f || mt.pending.has(id)) return;
    mt.pending.set(id, f.t);
    f.p.tasks.splice(f.p.tasks.indexOf(f.t), 1);
    mtRenderPrinter(f.p.id);
    undoBar('Deleted', f.t.name, {
        ms: 10000,
        onUndo() {
            // The task is still on the server; no request needed.
            const task = mt.pending.get(id);
            if (!task) return;
            mt.pending.delete(id);
            const again = mtFind(id);
            if (again) again.p.tasks.push(task);
            mtRenderPrinter(task.printer_id);
        },
        onCommit() {
            (async () => {
                try {
                    await apiFetch('/api/maintenance/' + encodeURIComponent(id), { method: 'DELETE' });
                } catch (e) {
                    toast(friendlyError(e.message), 'error');
                }
                mt.pending.delete(id);
                mtLoad(true);
            })();
        },
    });
}

async function mtSuggested(printerId) {
    try {
        await apiFetch('/api/maintenance', { method: 'POST', body: JSON.stringify({ printer_id: printerId, presets: true }) });
        await mtLoad(true);
    } catch (e) {
        toast(friendlyError(e.message), 'error');
    }
}

async function mtSaveForm(form) {
    const printerEl = form.closest('.mt-printer');
    const printerId = printerEl.dataset.printer;
    const taskEl = form.closest('.mt-task');
    const id = taskEl ? taskEl.dataset.task : '';
    const err = form.querySelector('[data-mt-error]');
    const fail = msg => { err.textContent = msg; err.classList.remove('hidden'); };
    const name = form.querySelector('[data-mt-name]').value.trim();
    const every = Number(form.querySelector('[data-mt-every]').value.trim().replace(',', '.'));
    if (!name) return fail('Give the task a name.');
    if (!Number.isFinite(every) || every <= 0) return fail('Enter the printing hours between jobs, more than 0.');
    const btn = form.querySelector('[data-mt-act="save"]');
    btn.disabled = true;
    try {
        await apiFetch('/api/maintenance', { method: 'POST', body: JSON.stringify({ id, printer_id: printerId, name, interval_hours: every }) });
        mt.open.delete(printerId);
        mt.editing = null;
        await mtLoad(true);
    } catch (e) {
        // mtLoad re-rendered the printer, so the old button is gone; find the
        // new one and re-enable it, or the form sits open with Save disabled.
        const fresh = mt.root.querySelector('.mt-form[data-form="' + CSS.escape(id || 'new') + '"] [data-mt-act="save"]');
        if (fresh) fresh.disabled = false; else btn.disabled = false;
        fail(friendlyError(e.message));
    }
}

function mtMount(section) {
    mt.root = section;
    section.innerHTML = `
<h2 class="font-heading text-xl font-medium leading-tight">Maintenance</h2>
<p class="mt-1.5 text-sm text-text-2 flex items-center gap-1">Reminders that come due after some hours of printing, counted from each printer's print history. ${tip('Nothing leaves this machine.', 'About the maintenance reminders')}</p>
<div id="mtBody" aria-live="polite"></div>`;

    section.addEventListener('click', e => {
        const b = e.target.closest('[data-mt-act]');
        if (!b) return;
        const act = b.dataset.mtAct;
        const printerEl = b.closest('.mt-printer'), taskEl = b.closest('.mt-task');
        const printerId = printerEl ? printerEl.dataset.printer : '';
        const taskId = taskEl ? taskEl.dataset.task : '';
        if (act === 'done') mtMarkDone(taskId);
        else if (act === 'delete') mtDelete(taskId);
        else if (act === 'retry') mtLoad();
        else if (act === 'suggested') mtSuggested(printerId);
        else if (act === 'add') { mt.open.add(printerId); mtRenderPrinter(printerId); const i = section.querySelector('.mt-printer[data-printer="' + CSS.escape(printerId) + '"] [data-mt-name]'); if (i) i.focus(); }
        else if (act === 'edit') {
            const prev = mt.editing;
            mt.editing = taskId;
            if (prev) { const f = mtFind(prev); if (f) mtRenderPrinter(f.p.id); }
            mtRenderPrinter(printerId);
            const i = section.querySelector('.mt-task[data-task="' + CSS.escape(taskId) + '"] [data-mt-name]'); if (i) i.focus();
        } else if (act === 'cancel') {
            if (mt.editing && b.closest('.mt-task')) mt.editing = null; else mt.open.delete(printerId);
            mtRenderPrinter(printerId);
        } else if (act === 'save') mtSaveForm(b.closest('.mt-form'));
    });
    section.addEventListener('change', e => {
        const sel = e.target.closest('[data-mt-pick]');
        if (!sel) return;
        const form = sel.closest('.mt-form');
        const p = mt.printers.find(p => p.id === form.closest('.mt-printer').dataset.printer);
        const preset = sel.value === '' ? null : p.presets[Number(sel.value)];
        if (preset) {
            form.querySelector('[data-mt-name]').value = preset.name;
            form.querySelector('[data-mt-every]').value = mtHours(preset.interval_hours);
        }
    });
    mtLoad();
}

registerSettingsTab({
    id: 'maintenance', label: 'Maintenance', group: 'Printing', order: 30,
    mount: mtMount,
    onShow() { if (!mt.open.size && !mt.editing) mtLoad(true); }, // hours move as prints finish
});
