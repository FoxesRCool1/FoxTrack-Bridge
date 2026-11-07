// History view: the print history table, per-print notes, the temperature
// chart and the CSV export. Shared state (_historyRecords, _historyCurrency)
// stays in ui.html; the shared helpers (apiFetch, toast, escHtml,
// friendlyError, configuredPrinters, liveStatus) come from there too.

// statusMeta() would label an unknown result "Connecting", so map the known ones.
function resultLabel(result) {
    const known = { finished:'Finished', cancelled:'Canceled', error:'Failed', failed:'Failed' };
    return known[result] || (result ? result.charAt(0).toUpperCase() + result.slice(1) : '-');
}

function toggleTempChart(uid) {
    const row = document.getElementById('hc-' + uid);
    if (row) row.classList.toggle('hidden');
}

// ── Per-print note ─────────────────────────────────────────────────────────────
// Inline edit for one record's note. The record's identity (printer name and
// start time) is looked up from _historyRecords by the row's uid.
function historyRecordByUid(uid) {
    const enc = uid.slice(0, uid.lastIndexOf('_'));
    const idx = Number(uid.slice(uid.lastIndexOf('_') + 1));
    const name = decodeURIComponent(enc);
    return _historyRecords.find(r => (r.printer_name || 'Unknown') === name && r._uid === idx);
}
function toggleNoteEdit(ev, uid) {
    if (ev) ev.stopPropagation(); // don't toggle the row's temperature chart
    const row = document.getElementById('hr-' + uid);
    if (!row) return;
    const rec = historyRecordByUid(uid);
    if (!rec) return;
    const fileCell = row.cells[0];
    // Collapse any open editor in another row first.
    const open = document.querySelector('.hist-note-editing');
    if (open && open !== row) {
        const cancel = open.querySelector('.hist-note-cancel');
        if (cancel) cancel.click();
    }
    if (row.classList.contains('hist-note-editing')) {
        const cancel = row.querySelector('.hist-note-cancel');
        if (cancel) cancel.click();
        return;
    }
    row.classList.add('hist-note-editing');
    const ta = document.createElement('textarea');
    ta.className = 'field w-full text-[11px]';
    ta.style.minWidth = '14rem'; // the file column is narrow; the cell scrolls sideways if needed
    ta.maxLength = 500;
    ta.rows = 2;
    ta.value = rec.note || '';
    ta.placeholder = 'Note (optional)';
    ta.setAttribute('aria-label', 'Edit note');
    const saveBtn = document.createElement('button');
    saveBtn.type = 'button';
    saveBtn.className = 'btn-primary btn-xs hist-note-save';
    saveBtn.textContent = 'Save';
    const cancelBtn = document.createElement('button');
    cancelBtn.type = 'button';
    cancelBtn.className = 'btn-outline btn-xs hist-note-cancel';
    cancelBtn.textContent = 'Cancel';
    const buttons = document.createElement('div');
    buttons.className = 'flex items-center gap-1 mt-1';
    buttons.append(saveBtn, cancelBtn);
    const wrap = document.createElement('div');
    wrap.className = 'mt-1';
    wrap.append(ta, buttons);
    fileCell.appendChild(wrap);
    ta.focus();
    ta.select();
    const close = () => { row.classList.remove('hist-note-editing'); wrap.remove(); };
    const doSave = async () => {
        saveBtn.disabled = true;
        try {
            const res = await apiFetch('/api/history/note', {
                method: 'POST',
                body: JSON.stringify({ printer_name: rec.printer_name, start_time: rec.start_time, note: ta.value }),
            });
            rec.note = (res && res.note) || ta.value;
            toast(rec.note ? 'Note saved' : 'Note cleared', 'success');
        } catch (e) {
            toast(friendlyError(e.message), 'error');
            return; // keep the editor open so the text isn't lost
        }
        close();
        // Update the row in place (no full reload).
        const oldNote = fileCell.querySelector('[data-hist-note]');
        if (oldNote) oldNote.remove();
        if (rec.note) {
            const span = document.createElement('span');
            span.className = 'block text-[11px] text-text-2 truncate mt-0.5';
            span.setAttribute('data-hist-note', '');
            span.title = rec.note;
            span.textContent = rec.note;
            fileCell.appendChild(span);
        }
    };
    saveBtn.addEventListener('click', doSave);
    cancelBtn.addEventListener('click', close);
    ta.addEventListener('keydown', e => {
        if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); doSave(); }
        else if (e.key === 'Escape') { e.preventDefault(); close(); }
    });
}

function toggleHistoryExpand(encName, btn) {
    const dataRows = document.querySelectorAll('.hist-data-extra[data-extra-for="' + encName + '"]');
    if (!dataRows.length) return;
    const isHidden = dataRows[0].classList.contains('hidden');
    dataRows.forEach(r => r.classList.toggle('hidden', !isHidden));
    if (!isHidden) {
        // Collapsing: also re-hide any open chart rows for the extra entries
        document.querySelectorAll('.hist-chart-extra[data-extra-for="' + encName + '"]').forEach(r => r.classList.add('hidden'));
    }
    if (btn) {
        const count = dataRows.length;
        btn.innerHTML = isHidden
            ? '<i class="fas fa-chevron-up mr-1 text-[10px]"></i>Show less'
            : '<i class="fas fa-chevron-down mr-1 text-[10px]"></i>Show ' + count + ' more';
    }
}

function buildTempChartSVG(samples, totalSec) {
    const W = 420, H = 72, padL = 36, padR = 8, padT = 6, padB = 18;
    const innerW = W - padL - padR, innerH = H - padT - padB;

    const nozzleVals = samples.map(s => s.nozzle_temp);
    const bedVals    = samples.map(s => s.bed_temp);
    const allVals    = [...nozzleVals, ...bedVals].filter(v => v > 0);
    if (!allVals.length) return '<p class="text-xs text-text-2">No data</p>';

    const yMin = Math.floor(Math.min(...allVals) / 10) * 10;
    const yMax = Math.ceil(Math.max(...allVals) / 10) * 10 + 5;
    const xMax = totalSec > 0 ? totalSec : samples[samples.length - 1].t || 1;

    const px = t  => padL + (t / xMax) * innerW;
    const py = v  => padT + innerH - ((v - yMin) / (yMax - yMin)) * innerH;

    const toPoints = vals => samples
        .map((s, i) => px(s.t).toFixed(1) + ',' + py(vals[i]).toFixed(1))
        .join(' ');

    // Axis labels
    const yLabels = [yMin, Math.round((yMin + yMax) / 2), yMax].map(v =>
        '<text x="' + (padL - 4) + '" y="' + py(v).toFixed(1) + '" text-anchor="end" dominant-baseline="middle" style="fill:var(--text-2)" font-size="8">' + v + '</text>'
    ).join('');

    const durFmt = s => s >= 3600 ? Math.floor(s/3600)+'h'+Math.floor((s%3600)/60)+'m' : Math.floor(s/60)+'m';
    const xLabels = '<text x="' + padL + '" y="' + (H - 4) + '" style="fill:var(--text-2)" font-size="8">0</text>'
        + '<text x="' + (padL + innerW) + '" y="' + (H - 4) + '" text-anchor="end" style="fill:var(--text-2)" font-size="8">' + durFmt(xMax) + '</text>';

    // Legend
    const legend = '<text x="' + (padL + 4) + '" y="' + (padT + 9) + '" style="fill:var(--accent)" font-size="8">Nozzle</text>'
        + '<text x="' + (padL + 44) + '" y="' + (padT + 9) + '" style="fill:var(--link)" font-size="8">Bed</text>';

    return '<svg viewBox="0 0 ' + W + ' ' + H + '" style="width:100%;height:80px" xmlns="http://www.w3.org/2000/svg">'
        + '<polyline points="' + toPoints(nozzleVals) + '" fill="none" style="stroke:var(--accent)" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/>'
        + '<polyline points="' + toPoints(bedVals) + '" fill="none" style="stroke:var(--link)" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/>'
        + yLabels + xLabels + legend
        + '</svg>';
}

function exportHistoryCsv() {
    if (!_historyRecords.length) return;
    const cur = _historyCurrency ? ' (' + _historyCurrency + ')' : '';
    const header = 'Printer,File,Result,Duration (s),Start,End,Filament (g),Energy (kWh),Filament cost' + cur + ',Energy cost' + cur + ',Total cost' + cur + ',Note\n';
    const num = (v, d) => v ? Number(v).toFixed(d) : '';
    const rows = _historyRecords.map(r => {
        const c = r._cost || {};
        const grams = c.filament_grams || historyGrams(r);
        return [r.printer_name, r.file_name, r.result, r.duration,
         new Date((r.start_time||0)*1000).toISOString(),
         new Date((r.end_time||0)*1000).toISOString(),
         num(grams, 1), num(c.energy_kwh || (r.energy_wh ? r.energy_wh / 1000 : 0), 3),
         c.filament_known ? num(c.filament_cost, 2) : '', c.energy_known ? num(c.energy_cost, 2) : '',
         (c.filament_known || c.energy_known) ? num(c.total, 2) : '',
         r.note || '',
        ].map(v => '"' + String(v ?? '').replace(/"/g,'""').replace(/^([=+\-@])/, "'$1") + '"').join(',');
    }).join('\n');
    const blob = new Blob([header + rows], { type: 'text/csv' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'print-history.csv';
    a.click();
}

// The Used column: grams (and the cost when prices are set), else energy.
function usedText(r) {
    const c = r._cost;
    const g = (c && c.filament_grams) || historyGrams(r);
    const parts = [];
    if (g) parts.push(Math.round(g) + ' g');
    if (c && (c.filament_known || c.energy_known)) parts.push(fmtMoney(c.total));
    else if (!g && r.energy_wh) parts.push((r.energy_wh / 1000).toFixed(2) + ' kWh');
    return parts.join(' · ') || '-';
}
function usedTitle(r) {
    const c = r._cost || {};
    const t = [];
    const g = c.filament_grams || historyGrams(r);
    if (g) t.push('Filament: ' + g.toFixed(1) + ' g' + (c.filament_known ? ', ' + fmtMoney(c.filament_cost) : ''));
    if (r.energy_wh) t.push('Electricity: ' + (r.energy_wh / 1000).toFixed(3) + ' kWh' + (c.energy_known ? ', ' + fmtMoney(c.energy_cost) : ''));
    return t.join('\n');
}

// Grams the record itself carries (Bambu spools), for when no prices are set.
function historyGrams(r) {
    return (r.filaments || []).reduce((s, f) => s + (f.grams || 0), 0);
}
function fmtMoney(v) {
    const cur = _historyCurrency || '';
    const n = (Math.round(v * 100) / 100).toFixed(2);
    return cur.length === 1 ? cur + n : (cur ? n + ' ' + cur : n);
}

async function refreshGlobalHistory() {
    const content = document.getElementById('globalHistoryContent');
    if (!content) return;
    // Keep what is on screen while it refreshes; placeholders only the first time.
    if (!content.dataset.loaded) {
        content.innerHTML = '<div class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-3 gap-3" aria-hidden="true">'
            + [0, 1].map(() => '<div class="rounded-lg border border-border bg-panel p-4"><div class="skel h-4" style="width:50%"></div>'
                + '<div class="skel mt-4 h-7 w-full"></div><div class="skel mt-3 w-full" style="height:.75rem"></div><div class="skel mt-2 w-full" style="height:.75rem"></div><div class="skel mt-2" style="height:.75rem;width:70%"></div></div>').join('')
            + '</div><p class="sr-only" role="status">Loading the history…</p>';
    }
    try {
        const data = await apiFetch('/api/history');
        const records = (data && data.records) ? data.records : [];
        const costs = (data && data.costs) || [];
        _historyCurrency = (data && data.currency) || '';
        records.forEach((r, i) => { if (costs[i]) r._cost = costs[i]; r._uid = i; });
        _historyRecords = records;
        content.dataset.loaded = '1';
        const csvBtn = document.getElementById('exportCsvBtn');
        if (records.length === 0) {
            if (csvBtn) csvBtn.style.display = 'none';
            content.innerHTML = '<p class="text-text-2 text-sm py-8 text-center">No print history yet.</p>';
            return;
        }
        if (csvBtn) csvBtn.style.display = '';
        const byPrinter = {};
        records.forEach(r => {
            const n = r.printer_name || 'Unknown';
            if (!byPrinter[n]) byPrinter[n] = [];
            byPrinter[n].push(r);
        });
        Object.keys(byPrinter).forEach(n => byPrinter[n].sort((a,b) => (b.start_time||0)-(a.start_time||0)));

        // History stays keyed by printer name (out of scope for the id migration); to
        // read this printer's live status (id-keyed) we need name -> id for currently
        // configured printers. Deleted/renamed printers simply have no live entry,
        // matching today's behavior.
        const nameToId = {};
        for (const p of configuredPrinters) nameToId[p.name] = p.id;

        const cards = Object.keys(byPrinter).map(name => {
            // %27 for ', which encodeURIComponent leaves in and which would
            // end the quoted uid inside the onclick handlers below.
            const enc = encodeURIComponent(name).replace(/'/g, '%27');
            const recs = byPrinter[name];
            // Per-printer mini stats
            const pFinished = recs.filter(r => r.result === 'finished').length;
            const pSecs = recs.reduce((s, r) => s + (r.duration || 0), 0);
            const pRate = recs.length ? Math.round(pFinished / recs.length * 100) + '%' : '-';
            // Hours: use Moonraker's own lifetime total when available (Klipper), otherwise Bridge-tracked
            const live = liveStatus[nameToId[name]];
            const printerTotalHours = live && live.total_print_hours > 0 ? live.total_print_hours : null;
            const pHours = printerTotalHours !== null
                ? printerTotalHours.toFixed(1) + 'h'
                : (pSecs / 3600).toFixed(1) + 'h';
            const hoursTitle = printerTotalHours !== null ? 'Lifetime total from printer' : 'Tracked by the Bridge';
            const priced = recs.filter(r => r._cost && (r._cost.filament_known || r._cost.energy_known));
            const pCost = priced.reduce((s, r) => s + r._cost.total, 0);
            const stat = (label, value, title) => '<div class="px-3 py-2 text-center"><p class="text-[10px] text-text-2">' + label + '</p><p class="font-heading text-lg leading-tight tabular-nums mt-0.5"' + (title ? ' title="' + escHtml(title) + '"' : '') + '>' + value + '</p></div>';
            const statsStrip = '<div class="grid divide-x divide-border border-b border-border" style="grid-template-columns:repeat(' + (priced.length ? 4 : 3) + ',minmax(0,1fr))">'
                + stat('Prints', recs.length)
                + stat('Success', pRate)
                + stat('Hours', pHours + (printerTotalHours !== null ? ' <span class="text-text-2 text-[10px]">(lifetime)</span>' : ''), hoursTitle)
                + (priced.length ? stat('Cost', escHtml(fmtMoney(pCost)), 'Filament and electricity, from the prices in Settings, Costs') : '')
                + '</div>';

            const rows = recs.map((r, i) => {
                // _uid is the record's index in _historyRecords, so the note
                // editor finds this exact record (i only counts this printer's rows).
                const uid = enc + '_' + r._uid;
                const date = new Date((r.start_time||0)*1000).toLocaleDateString();
                const sec = r.duration||0;
                const dur = sec>=3600 ? Math.floor(sec/3600)+'h '+Math.floor((sec%3600)/60)+'m' : Math.floor(sec/60)+'m';
                const rc = r.result==='finished' ? 'text-brand' : r.result==='error' ? 'text-danger' : 'text-warning';
                const hasSamples = r.temp_samples && r.temp_samples.length > 1;
                const isExtra = i >= 5;
                const TEMP_CHART_LABEL = 'Temperature chart';
                const tempsBtn = hasSamples
                    ? '<button onclick="toggleTempChart(\'' + uid + '\')" class="inline-flex items-center gap-1 rounded-full border border-border text-text-2 hover:text-text hover:bg-panel tap-32 justify-center text-[11px]" title="' + TEMP_CHART_LABEL + '" aria-label="' + TEMP_CHART_LABEL + '"><i class="fas fa-chart-line"></i></button>'
                    : '';
                const noteBtn = '<button onclick="toggleNoteEdit(event, \'' + uid + '\')" class="inline-flex items-center gap-1 rounded-full border border-border text-text-2 hover:text-text hover:bg-panel tap-32 justify-center text-[11px]" title="Edit note" aria-label="Edit note"><i class="fas fa-pencil"></i></button>';
                const dataRowClass = 'border-t border-border hover:bg-panel' + (isExtra ? ' hist-data-extra hidden' : '');
                const chartRowClass = 'hidden border-t border-border bg-panel' + (isExtra ? ' hist-chart-extra' : '');
                const extraAttr = isExtra ? ' data-extra-for="' + enc + '"' : '';
                const noteLine = r.note
                    ? '<span data-hist-note class="block text-[11px] text-text-2 truncate mt-0.5" title="' + escHtml(r.note) + '">' + escHtml(r.note) + '</span>'
                    : '';
                const dataRow = '<tr id="hr-' + uid + '" class="' + dataRowClass + '"' + extraAttr + '>'
                    + '<td class="py-2 pl-4 pr-3 text-text max-w-[160px]" data-no-i18n><span class="block truncate" title="' + escHtml(r.file_name) + '">' + escHtml(r.file_name||'-') + '</span>' + noteLine + '</td>'
                    + '<td class="py-2 pr-3 ' + rc + '">' + escHtml(resultLabel(r.result)) + '</td>'
                    + '<td class="py-2 pr-3 text-text-2 whitespace-nowrap">' + escHtml(dur) + '</td>'
                    + '<td class="py-2 pr-3 text-text-2 whitespace-nowrap" title="' + escHtml(usedTitle(r)) + '">' + escHtml(usedText(r)) + '</td>'
                    + '<td class="py-2 pr-3 text-text-2 whitespace-nowrap">' + escHtml(date) + '</td>'
                    + '<td class="py-2 pr-4 text-right">' + noteBtn + tempsBtn + '</td>'
                    + '</tr>';
                const chartRow = '<tr id="hc-' + uid + '" class="' + chartRowClass + '"' + extraAttr + '>'
                    + '<td colspan="6" class="px-4 py-3">' + (hasSamples ? buildTempChartSVG(r.temp_samples, sec) : '') + '</td>'
                    + '</tr>';
                return dataRow + chartRow;
            }).join('');

            const moreRow = recs.length > 5
                ? '<tr><td colspan="6" class="px-4 py-2.5 border-t border-border text-center">'
                  + '<button onclick="toggleHistoryExpand(\'' + enc + '\', this)" class="text-[11px] text-text-2 hover:text-text transition-colors py-0.5">'
                  + '<i class="fas fa-chevron-down mr-1 text-[10px]"></i>Show ' + (recs.length - 5) + ' more'
                  + '</button></td></tr>'
                : '';

            return '<div class="rounded-lg border border-border bg-panel overflow-hidden">'
                + '<div class="px-4 py-3 border-b border-border flex items-center justify-between">'
                + '<p class="font-heading text-lg font-medium leading-tight" data-no-i18n>' + escHtml(name) + '</p>'
                + '<span class="text-[10px] text-text-2 rounded-full border border-border px-2 py-0.5">' + recs.length + ' print' + (recs.length!==1?'s':'') + '</span>'
                + '</div>'
                + statsStrip
                + '<div class="max-h-96 overflow-y-auto"><table class="w-full text-xs">'
                + '<thead><tr class="text-text-2 text-[10px]">'
                + '<th class="pl-4 pr-3 py-2 font-medium text-left">File</th>'
                + '<th class="pr-3 py-2 font-medium text-left">Result</th>'
                + '<th class="pr-3 py-2 font-medium text-left">Duration</th>'
                + '<th class="pr-3 py-2 font-medium text-left">Used</th>'
                + '<th class="pr-3 py-2 font-medium text-left">Date</th>'
                + '<th class="pr-4 py-2"></th>'
                + '</tr></thead><tbody>' + rows + moreRow + '</tbody></table></div>'
                + '</div>';
        }).join('');

        content.innerHTML = '<div class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-3 gap-3">' + cards + '</div>';
    } catch (_) { content.innerHTML = '<p class="text-danger text-sm py-4 text-center">Couldn\'t load history. Is the Bridge still running?</p>'; }
}
