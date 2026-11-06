// Command menu (Ctrl+K or Cmd+K, or /) and keyboard shortcuts: jump to any
// view or Settings section, act on a printer, switch the theme. Shortcuts
// are shown next to each command, and "?" lists them.
(function () {
    const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
    const MOD = isMac ? '⌘' : 'Ctrl';
    const style = document.createElement('style');
    style.textContent = `
        #cmdk { position: fixed; inset: 0; z-index: 70; display: flex; align-items: flex-start; justify-content: center;
            padding: max(12vh, env(safe-area-inset-top)) 1rem 1rem; }
        #cmdk .cmdk-box { width: 100%; max-width: 34rem; border: 1px solid var(--border); border-radius: var(--radius);
            background: var(--surface); box-shadow: 0 20px 50px rgb(0 0 0 / 18%); overflow: hidden;
            animation: cmdk-in 140ms ease-out; }
        @keyframes cmdk-in { from { opacity: 0; transform: translateY(-6px) scale(.985); } }
        @media (prefers-reduced-motion: reduce) { #cmdk .cmdk-box { animation: none; } }
        #cmdk input { width: 100%; height: 3rem; padding: 0 1rem; border: 0; border-bottom: 1px solid var(--border);
            background: transparent; color: var(--text); font: inherit; font-size: .9375rem; outline: none; }
        #cmdk ul { max-height: min(60vh, 26rem); overflow-y: auto; margin: 0; padding: .375rem; list-style: none; overscroll-behavior: contain; }
        #cmdk .cmdk-group { padding: .5rem .625rem .25rem; font-size: .6875rem; font-weight: 500; letter-spacing: .04em; text-transform: uppercase; color: var(--text-2); }
        #cmdk li[role=option] { display: flex; align-items: center; gap: .625rem; min-height: 2.5rem; padding: 0 .625rem;
            border-radius: 8px; font-size: .875rem; color: var(--text); cursor: pointer; }
        #cmdk li[role=option] .cmdk-sub { color: var(--text-2); font-size: .75rem; margin-left: .25rem; }
        #cmdk li[aria-selected=true] { background: var(--panel-2); }
        #cmdk .cmdk-empty { padding: 1rem; font-size: .875rem; color: var(--text-2); }
        .kbd { display: inline-flex; align-items: center; gap: .125rem; margin-left: auto; flex-shrink: 0; }
        .kbd kbd { min-width: 1.25rem; height: 1.25rem; padding: 0 .3rem; display: inline-flex; align-items: center; justify-content: center;
            border: 1px solid var(--border); border-radius: 4px; font: 500 .6875rem/1 ui-monospace, SFMono-Regular, Menlo, monospace; color: var(--text-2); }
        @media (max-width: 640px) { #cmdk { padding-top: max(.75rem, env(safe-area-inset-top)); } #cmdk li[role=option] { min-height: 2.75rem; } }
    `;
    document.head.appendChild(style);

    const kbd = keys => keys ? '<span class="kbd" aria-hidden="true">' + keys.map(k => '<kbd>' + escHtml(k) + '</kbd>').join('') + '</span>' : '';

    function printerCommands() {
        const out = [];
        for (const p of (configuredPrinters || [])) {
            const s = (liveStatus || {})[p.id] || {};
            const st = s.status || '';
            const enc = encodeURIComponent(p.id);
            out.push({ group: 'Printers', label: p.name, raw: true, sub: st ? statusWord(st) : '', run: () => showView('printers').then(() => scrollToPrinter(p.id)) });
            if (st === 'printing') out.push({ group: 'Printers', label: fill('Pause {0}', p.name), run: () => sendControl(p.id, 'pause') });
            if (st === 'paused') out.push({ group: 'Printers', label: fill('Resume {0}', p.name), run: () => sendControl(p.id, 'resume') });
            if (st === 'printing' || st === 'paused') out.push({ group: 'Printers', label: fill('Stop {0}…', p.name), run: () => confirmStop(p.id) });
            if (typeof s.light_on === 'boolean' && st && st !== 'disconnected') out.push({ group: 'Printers', label: fill(s.light_on ? 'Light off: {0}' : 'Light on: {0}', p.name), run: () => toggleLightEncoded(enc) });
            out.push({ group: 'Printers', label: fill('Edit {0}', p.name), run: () => openEditPrinterModal(p.id) });
        }
        return out;
    }

    // Bulk actions: pause every printing printer, resume every paused one, or
    // cut the plug of an idle one whose nozzle has cooled. Each opens the shared
    // confirm modal listing the affected printers, then sends the command one
    // printer at a time.
    const IDLE_PLUG_STATUSES = ['idle', 'finished', 'error', 'disconnected'];
    // plug: a plug is switched by the Bridge itself, so a printer that refuses
    // print commands (control_blocked) still counts.
    function bulkPrinters(predicate, plug) {
        const out = [];
        for (const p of (configuredPrinters || [])) {
            const s = (liveStatus || {})[p.id] || {};
            if ((plug || !s.control_blocked) && predicate(s)) out.push(p);
        }
        return out;
    }
    // One wording per action, singular and plural, so each is a whole sentence
    // the translations can carry ({0} is the count).
    const BULK_TEXT = {
        pause: { title: ['Pause 1 printer?', 'Pause {0} printers?'], confirm: ['Pause 1 printer', 'Pause {0} printers'], done: ['Paused 1 printer', 'Paused {0} printers'] },
        resume: { title: ['Resume 1 printer?', 'Resume {0} printers?'], confirm: ['Resume 1 printer', 'Resume {0} printers'], done: ['Resumed 1 printer', 'Resumed {0} printers'] },
        plug_off: { title: ['Turn off 1 plug?', 'Turn off {0} plugs?'], confirm: ['Turn off 1 plug', 'Turn off {0} plugs'], done: ['Turned off 1 plug', 'Turned off {0} plugs'] },
    };
    const countText = (pair, n) => n === 1 ? pair[0] : fill(pair[1], n);
    function bulkControl(command, predicate, printers) {
        const text = BULK_TEXT[command];
        const n = printers.length;
        confirmDialog({
            title: countText(text.title, n),
            body: printers.map(p => p.name).join(', '),
            confirmLabel: countText(text.confirm, n),
        }).then(async ok => {
            if (!ok) return;
            // Re-read the status at confirm time: a printer that moved on since
            // the menu opened is skipped, not acted on stale. A printer that
            // refuses print commands can still have its plug switched.
            const targets = [];
            for (const p of printers) {
                const s = (liveStatus || {})[p.id] || {};
                if ((command === 'plug_off' || !s.control_blocked) && predicate(s)) targets.push(p);
            }
            if (!targets.length) { toast('No printers match anymore', 'info'); return; }
            const done = [];
            const failed = [];
            for (const p of targets) {
                try {
                    await apiFetch('/api/control/' + encodeURIComponent(printerNameForId(p.id)) + '/' + encodeURIComponent(command), {
                        method: 'POST', body: JSON.stringify({}),
                        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': (crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random()) },
                    });
                    done.push(p);
                } catch (_) { failed.push(p); }
            }
            await pollStatus();
            if (!failed.length) {
                toast(countText(text.done, done.length), 'info');
            } else {
                toast(fill('Failed: {0}', failed.map(p => p.name).join(', ')), 'error');
            }
        });
    }
    function bulkPauseAll() {
        const printers = bulkPrinters(s => s.status === 'printing');
        if (!printers.length) return;
        bulkControl('pause', s => s.status === 'printing', printers);
    }
    function bulkResumeAll() {
        const printers = bulkPrinters(s => s.status === 'paused');
        if (!printers.length) return;
        bulkControl('resume', s => s.status === 'paused', printers);
    }
    const idlePlugOn = s => s.plug && s.plug.reachable && s.plug.on
        && IDLE_PLUG_STATUSES.indexOf(s.status) >= 0
        && (s.nozzle_temp == null || s.nozzle_temp < 50);
    function bulkPlugsOff() {
        const printers = bulkPrinters(idlePlugOn, true);
        if (!printers.length) return;
        bulkControl('plug_off', idlePlugOn, printers);
    }
    function bulkCommands() {
        const cmds = [];
        const printing = bulkPrinters(s => s.status === 'printing');
        if (printing.length) cmds.push({ group: 'Actions', label: fill('Pause all printing ({0})', printing.length), icon: 'fa-print', run: bulkPauseAll });
        const paused = bulkPrinters(s => s.status === 'paused');
        if (paused.length) cmds.push({ group: 'Actions', label: fill('Resume all paused ({0})', paused.length), icon: 'fa-print', run: bulkResumeAll });
        const plugs = bulkPrinters(idlePlugOn, true);
        if (plugs.length) cmds.push({ group: 'Actions', label: fill('Turn off idle plugs ({0})', plugs.length), icon: 'fa-plug', run: bulkPlugsOff });
        return cmds;
    }
    function statusWord(st) {
        return ({ printing: 'Printing', paused: 'Paused', idle: 'Idle', finished: 'Finished', error: 'Error', disconnected: 'Offline', connected: 'Connecting' })[st] || st;
    }
    function scrollToPrinter(id) {
        const entry = (typeof printerCardEls !== 'undefined') && printerCardEls.get(id);
        if (entry && entry.el) { entry.el.scrollIntoView({ behavior: 'smooth', block: 'center' }); entry.el.focus && entry.el.focus({ preventScroll: true }); }
    }

    function allCommands() {
        const cmds = [
            { group: 'Go to', label: 'Printers', keys: ['G', 'P'], run: () => showView('printers') },
            { group: 'Go to', label: 'History', keys: ['G', 'H'], run: () => showView('history') },
            { group: 'Go to', label: 'Settings', keys: ['G', 'S'], run: () => showView('settings') },
        ];
        for (const t of SETTINGS_TABS) cmds.push({ group: 'Settings', label: t.label, sub: t.group, run: () => showView('settings').then(() => showSettingsTab(t.id)) });
        cmds.push(...printerCommands());
        cmds.push(...bulkCommands());
        cmds.push(
            { group: 'Actions', label: 'Add a printer', keys: ['N'], run: () => openAddPrinterModal() },
            { group: 'Actions', label: 'Scan the network for printers', run: () => { showView('printers'); runDiscovery(true); } },
            { group: 'Actions', label: 'Switch theme', keys: ['T'], run: () => toggleTheme() },
            { group: 'Actions', label: 'Show the Bridge log', keys: ['L'], run: () => toggleLogPanel() },
            { group: 'Actions', label: 'Check for updates', run: () => checkForUpdate() },
            { group: 'Actions', label: 'Keyboard shortcuts', keys: ['?'], run: () => open('?') },
        );
        return cmds;
    }

    // Subsequence match with a bonus for word starts and runs; 0 = no match.
    function score(q, text) {
        if (!q) return 1;
        const t = text.toLowerCase();
        const at = t.indexOf(q.toLowerCase());
        if (at >= 0) return 100 - at + (at === 0 || /[\s:\-]/.test(t[at - 1]) ? 50 : 0);
        if (q.length < 3) return 0; // a stray subsequence of two letters matches nearly everything
        let ti = 0, s = 0, run = 0;
        for (const ch of q.toLowerCase()) {
            const i = t.indexOf(ch, ti);
            if (i < 0) return 0;
            run = i === ti ? run + 1 : 0;
            s += 1 + run + (i === 0 || /[\s:\-]/.test(t[i - 1]) ? 3 : 0);
            ti = i + 1;
        }
        return s - t.length * 0.01;
    }

    let root = null, input = null, list = null, items = [], sel = 0, lastFocus = null;

    function build() {
        root = document.createElement('div');
        root.id = 'cmdk';
        root.className = 'overlay-tint hidden';
        root.innerHTML = '<div class="cmdk-box floating" role="dialog" aria-modal="true" aria-label="Command menu">'
            + '<input type="text" role="combobox" aria-expanded="true" aria-controls="cmdkList" aria-autocomplete="list" placeholder="Type a command or search…" autocomplete="off" spellcheck="false">'
            + '<ul id="cmdkList" role="listbox" aria-label="Commands"></ul></div>';
        document.body.appendChild(root);
        input = root.querySelector('input');
        list = root.querySelector('ul');
        root.addEventListener('mousedown', e => { if (e.target === root) close(); });
        input.addEventListener('input', () => { sel = 0; draw(); });
        input.addEventListener('keydown', e => {
            if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
            else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
            else if (e.key === 'Home' && !input.value) { e.preventDefault(); sel = 0; draw(); }
            else if (e.key === 'Enter') { e.preventDefault(); choose(sel); }
            else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); close(); }
            else if (e.key === 'Tab') { e.preventDefault(); move(e.shiftKey ? -1 : 1); }
        });
        list.addEventListener('mousemove', e => {
            const li = e.target.closest('li[role=option]');
            if (li && +li.dataset.i !== sel) { sel = +li.dataset.i; mark(); }
        });
        list.addEventListener('click', e => {
            const li = e.target.closest('li[role=option]');
            if (li) choose(+li.dataset.i);
        });
    }

    function draw() {
        const q = input.value.trim();
        if (q === '?') {
            items = allCommands().filter(c => c.keys);
            items.unshift({ group: 'Shortcuts', label: 'Command menu', keys: [MOD, 'K'], run: () => open() });
        } else {
            items = allCommands()
                .map(c => ({ c, s: score(q, c.label) || (c.group.toLowerCase().startsWith(q.toLowerCase()) ? 0.5 : 0) }))
                .filter(x => x.s > 0)
                .sort((a, b) => q ? b.s - a.s : 0)
                .map(x => x.c);
            if (q) items = items.slice(0, 40);
        }
        sel = Math.min(sel, Math.max(0, items.length - 1));
        let html = '', group = null;
        items.forEach((c, i) => {
            const g = q && q !== '?' ? 'Results' : c.group;
            if (g !== group) { group = g; html += '<li class="cmdk-group" role="presentation">' + escHtml(g) + '</li>'; }
            html += '<li role="option" id="cmdk-o' + i + '" data-i="' + i + '" aria-selected="' + (i === sel) + '">'
                + (c.icon ? '<i class="fas ' + c.icon + '" aria-hidden="true"></i>' : '')
                + '<span class="min-w-0 truncate">' + (c.raw ? '<span data-no-i18n>' + escHtml(c.label) + '</span>' : escHtml(c.label)) + (c.sub ? '<span class="cmdk-sub">' + escHtml(c.sub) + '</span>' : '') + '</span>'
                + kbd(c.keys) + '</li>';
        });
        list.innerHTML = html || '<li class="cmdk-empty" role="presentation">Nothing matches.</li>';
        mark();
    }
    function mark() {
        list.querySelectorAll('li[role=option]').forEach(li => li.setAttribute('aria-selected', String(+li.dataset.i === sel)));
        const cur = list.querySelector('#cmdk-o' + sel);
        input.setAttribute('aria-activedescendant', cur ? cur.id : '');
        if (cur) cur.scrollIntoView({ block: 'nearest' });
    }
    function move(d) { if (!items.length) return; sel = (sel + d + items.length) % items.length; mark(); }
    function choose(i) {
        const c = items[i];
        if (!c) return;
        close(false);
        try { c.run(); } catch (e) { toast(e.message, 'error'); }
    }

    function open(prefill) {
        if (!root) build();
        lastFocus = document.activeElement;
        root.classList.remove('hidden');
        input.value = prefill || '';
        sel = 0;
        draw();
        input.focus();
    }
    function close(restore = true) {
        if (!root || root.classList.contains('hidden')) return;
        root.classList.add('hidden');
        if (restore && lastFocus && lastFocus.focus) lastFocus.focus();
    }
    window.openCommandMenu = open;

    function typing(e) {
        const t = e.target;
        return t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
    }
    function dialogOpen() {
        // The dashboard's dialogs are full-screen .fixed.inset-0 overlays.
        return !!document.querySelector('.fixed.inset-0.z-50:not(.hidden), #skipObjectsModal:not(.hidden)');
    }

    let pendingG = 0;
    document.addEventListener('keydown', e => {
        if ((e.metaKey || e.ctrlKey) && !e.altKey && e.key.toLowerCase() === 'k') {
            e.preventDefault();
            if (root && !root.classList.contains('hidden')) close();
            else if (!dialogOpen()) open();
            return;
        }
        if (e.metaKey || e.ctrlKey || e.altKey || typing(e) || (root && !root.classList.contains('hidden'))) return;
        if (dialogOpen()) return;
        const k = e.key;
        if (pendingG && Date.now() - pendingG < 1200) {
            pendingG = 0;
            const v = { p: 'printers', h: 'history', s: 'settings' }[k.toLowerCase()];
            if (v) { e.preventDefault(); showView(v); return; }
        }
        if (e.repeat) return;
        if (k === 'g' || k === 'G') { pendingG = Date.now(); return; }
        if (k === '/') { e.preventDefault(); open(); }
        else if (k === '?') { e.preventDefault(); open('?'); }
        else if (k === 'n' || k === 'N') { e.preventDefault(); openAddPrinterModal(); }
        else if (k === 't' || k === 'T') { e.preventDefault(); toggleTheme(); }
        else if (k === 'l' || k === 'L') { e.preventDefault(); toggleLogPanel(); }
    });
})();
