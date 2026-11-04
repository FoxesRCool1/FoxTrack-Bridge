// Settings, Security: the optional dashboard password and API tokens.
(function () {
    const S = '#settings-security';
    const style = document.createElement('style');
    style.textContent = `
        ${S} .sec-row { display: flex; align-items: center; justify-content: space-between; gap: .75rem; min-height: 2.75rem; padding: .5rem .75rem; border: 1px solid var(--border); border-radius: 8px; }
        ${S} .sec-row + .sec-row { margin-top: .5rem; }
        ${S} .sec-token { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .8125rem; word-break: break-all; padding: .625rem .75rem; border-radius: 8px; background: var(--panel-2); }
        ${S} .sec-dot { width: .375rem; height: .375rem; border-radius: 9999px; flex-shrink: 0; }
        ${S} .sec-skel { height: 2.75rem; border-radius: 8px; background: var(--panel-2); }
        ${S} .sec-num { font-variant-numeric: tabular-nums; }
    `;
    document.head.appendChild(style);

    let view = null;

    function fmtDate(sec) {
        try { return new Date(sec * 1000).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' }); }
        catch (_) { return ''; }
    }

    function render(el) {
        if (!view) {
            el.innerHTML = '<h2 class="font-heading text-xl font-medium leading-tight">Security</h2>'
                + '<div class="mt-5 space-y-2" aria-busy="true"><div class="sec-skel"></div><div class="sec-skel"></div></div>';
            return;
        }
        const on = view.password_set;
        const tokens = view.tokens || [];
        el.innerHTML = `
            <h2 class="font-heading text-xl font-medium leading-tight">Security</h2>
            <p class="mt-1.5 text-sm text-text-2">${on
                ? 'The dashboard and API need the password. Scripts and Home Assistant can use an API token instead.'
                : 'Anyone on your network can open this dashboard and control your printers. Set a password to stop that.'}</p>
            <p class="mt-4 text-xs flex items-center gap-1.5" role="status">
                <span class="sec-dot" style="background:var(${on ? '--accent' : '--warning'})"></span>
                <span class="${on ? 'text-text' : 'text-warning'}">${on ? 'Password on' : 'No password'}</span>
            </p>

            <form id="secPwForm" class="mt-5 space-y-4" novalidate>
                <input type="text" name="username" value="FoxTrack Bridge" autocomplete="username" class="hidden" aria-hidden="true" tabindex="-1">
                ${on ? `<div>
                    <label for="secCurrent" class="field-label">Current password</label>
                    <input id="secCurrent" type="password" class="field" autocomplete="current-password">
                </div>` : ''}
                <div>
                    <label for="secNew" class="field-label">${on ? 'New password' : 'Password'}</label>
                    <input id="secNew" type="password" class="field" autocomplete="new-password" minlength="8" aria-describedby="secNewHelp">
                    <p id="secNewHelp" class="mt-1.5 text-xs text-text-2">At least 8 characters. Everyone else is signed out when it changes.</p>
                </div>
                <div class="flex flex-wrap items-center justify-end gap-2">
                    ${on ? '<button type="button" id="secRemove" class="btn-danger btn-lg">Remove password</button>' : ''}
                    <button type="submit" id="secSave" class="btn-primary btn-lg">${on ? 'Change password' : 'Set password'}</button>
                </div>
            </form>
            ${on ? `<p class="mt-3 text-xs text-text-2">Forgot it? Stop the Bridge, run it once with <code>--reset-password</code>, then start it again.</p>
                <div class="mt-4 flex justify-end"><button type="button" id="secSignOut" class="btn-ghost">Sign out of this browser</button></div>` : ''}

            <div class="mt-8 pt-6 border-t border-border">
                <div class="flex items-center gap-2">
                    <h3 class="text-sm font-medium">API tokens</h3>
                    ${tip('For Home Assistant, Prometheus or your own scripts. Send it as <code>Authorization: Bearer &lt;token&gt;</code>. A token only matters once a password is set.', 'About API tokens')}
                </div>
                <div id="secTokenList" class="mt-3">
                    ${tokens.length ? tokens.map(t => `
                        <div class="sec-row">
                            <div class="min-w-0">
                                <p class="text-sm font-medium truncate">${escHtml(t.name)}</p>
                                <p class="text-xs text-text-2 sec-num">…${escHtml(t.hint)} · created ${escHtml(fmtDate(t.created_at))}</p>
                            </div>
                            <button type="button" class="btn-danger btn-xs" data-revoke="${escHtml(t.id)}" aria-label="Revoke ${escHtml(t.name)}">Revoke</button>
                        </div>`).join('') : '<p class="text-xs text-text-2">No tokens yet.</p>'}
                </div>
                <div id="secNewToken" class="hidden mt-3"></div>
                <form id="secTokenForm" class="mt-4 flex gap-2" novalidate>
                    <label for="secTokenName" class="sr-only">Token name</label>
                    <input id="secTokenName" class="field" placeholder="Name, e.g. Home Assistant" maxlength="60">
                    <button type="submit" class="btn-outline btn-lg shrink-0">Create token</button>
                </form>
            </div>`;
        wire(el);
    }

    function wire(el) {
        el.querySelector('#secPwForm').addEventListener('submit', async e => {
            e.preventDefault();
            const cur = el.querySelector('#secCurrent');
            const nw = el.querySelector('#secNew');
            if ((nw.value || '').length < 8) { nw.classList.add('field-invalid'); nw.focus(); toast('Use at least 8 characters.', 'error'); return; }
            nw.classList.remove('field-invalid');
            const btn = el.querySelector('#secSave');
            btn.disabled = true;
            try {
                view = await apiFetch('/api/security', { method: 'POST', body: JSON.stringify({ current: cur ? cur.value : '', new: nw.value }) });
                toast(view.password_set ? 'Password saved. This browser stays signed in.' : 'Saved.');
                render(el);
            } catch (err) {
                btn.disabled = false;
                toast(err.message, 'error');
                if (cur && /current/i.test(err.message)) { cur.classList.add('field-invalid'); cur.focus(); }
            }
        });
        const rm = el.querySelector('#secRemove');
        if (rm) rm.addEventListener('click', async () => {
            const cur = el.querySelector('#secCurrent');
            if (!cur.value) { cur.classList.add('field-invalid'); cur.focus(); toast('Type the current password to remove it.', 'error'); return; }
            const ok = await confirmDialog({ title: 'Remove the password?', body: 'Anyone on your network will be able to open the dashboard and control your printers again.', confirmLabel: 'Remove password' });
            if (!ok) return;
            try {
                view = await apiFetch('/api/security', { method: 'POST', body: JSON.stringify({ current: cur.value, remove: true }) });
                toast('Password removed.');
                render(el);
            } catch (err) { toast(err.message, 'error'); }
        });
        const so = el.querySelector('#secSignOut');
        if (so) so.addEventListener('click', async () => {
            try { await apiFetch('/api/auth/logout', { method: 'POST' }); } catch (_) {}
            location.href = '/login';
        });
        el.querySelector('#secTokenForm').addEventListener('submit', async e => {
            e.preventDefault();
            const name = el.querySelector('#secTokenName').value.trim();
            try {
                const res = await apiFetch('/api/security/tokens', { method: 'POST', body: JSON.stringify({ name }) });
                view = res.view;
                render(el);
                const box = el.querySelector('#secNewToken');
                box.classList.remove('hidden');
                box.innerHTML = `<p class="text-xs text-text-2 mb-1.5">Copy it now. It is not shown again.</p>
                    <div class="flex items-center gap-2"><div class="sec-token flex-1 min-w-0" id="secTokVal">${escHtml(res.token)}</div>
                    <button type="button" class="btn-outline shrink-0" id="secCopy" aria-label="Copy token"><i class="fas fa-copy"></i> Copy</button></div>`;
                box.querySelector('#secCopy').addEventListener('click', async () => {
                    try { await navigator.clipboard.writeText(res.token); toast('Token copied.'); }
                    catch (_) { getSelection().selectAllChildren(box.querySelector('#secTokVal')); toast('Select and copy it by hand.', 'info'); }
                });
            } catch (err) { toast(err.message, 'error'); }
        });
        el.querySelectorAll('[data-revoke]').forEach(b => b.addEventListener('click', async () => {
            const name = b.closest('.sec-row').querySelector('p').textContent;
            const ok = await confirmDialog({ title: 'Revoke this token?', body: 'Anything using "' + name + '" stops working at once. This cannot be undone.', confirmLabel: 'Revoke' });
            if (!ok) return;
            try {
                view = await apiFetch('/api/security/tokens/' + encodeURIComponent(b.dataset.revoke), { method: 'DELETE' });
                toast('Token revoked.');
                render(el);
            } catch (err) { toast(err.message, 'error'); }
        }));
    }

    async function load(el) {
        try { view = await apiFetch('/api/security'); }
        catch (err) { el.innerHTML = '<h2 class="font-heading text-xl font-medium leading-tight">Security</h2><p class="mt-3 text-sm text-danger">' + escHtml(friendlyError(err.message)) + '</p>'; return; }
        render(el);
    }

    registerSettingsTab({
        id: 'security', label: 'Security', group: 'Bridge', order: 10,
        mount(el) { render(el); load(el); },
        onShow(el) { load(el); },
    });
})();
