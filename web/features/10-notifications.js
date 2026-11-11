// Settings > Notifications: send print events to chat and push services.
// State lives in NF; the server (GET /api/notifications) is the source of
// truth and never sends a secret back, only token_set / url_set flags, so a
// secret field left blank means "keep what is saved".

const NF = {
    data: null,          // last server view: channels, services, events, quiet hours
    chs: [],             // channels as shown; drafts (_draft) are not saved yet
    open: null,          // _k of the channel whose editor is open
    sec: null,
    busy: false,
    pending: new Map(),  // channel id -> { timer, bar } for deletes that can still be undone
    n: 0,
};

// What each service asks for. `secret` names the server flag that says a value
// is already saved. Tips say where to find each value.
const NF_FIELDS = {
    discord: [
        { k: 'url', label: 'Webhook URL', ph: 'https://discord.com/api/webhooks/…', secret: 'url_set',
          tip: 'In Discord: Server settings, Integrations, Webhooks, New webhook. Pick the channel, then Copy webhook URL. Anyone with this URL can post there, so the Bridge keeps it private.' },
    ],
    slack: [
        { k: 'url', label: 'Webhook URL', ph: 'https://hooks.slack.com/services/…', secret: 'url_set',
          tip: 'At api.slack.com/apps create an app, turn on Incoming Webhooks, then Add New Webhook to Workspace and pick a channel. Copy the URL it shows.' },
    ],
    telegram: [
        { k: 'token', label: 'Bot token', ph: '123456:ABC-DEF…', secret: 'token_set',
          tip: 'In Telegram, open a chat with @BotFather, send /newbot and follow the steps. It gives you a token to paste here.' },
        { k: 'target', label: 'Chat ID', ph: '123456789',
          tip: 'Send your new bot any message first. Then message @userinfobot to see your own ID, or for a group add the bot and use the group ID (it starts with a minus sign).' },
    ],
    ntfy: [
        { k: 'url', label: 'Topic URL', ph: 'https://ntfy.sh/my-private-topic',
          tip: 'Pick a topic name nobody can guess, because anyone who knows it can read it. Subscribe to the same topic in the ntfy app. For your own server, use its address.' },
        { k: 'token', label: 'Access token (optional)', ph: 'tk_…', secret: 'token_set', optional: true,
          tip: 'Only for a protected topic or a self-hosted server. In the ntfy web app: Account, Access tokens, Create token.' },
    ],
    pushover: [
        { k: 'token', label: 'App token', ph: 'Application API token', secret: 'token_set',
          tip: 'At pushover.net/apps/build create an application (call it FoxTrack Bridge) and copy its API token.' },
        { k: 'target', label: 'User key', ph: 'Your user key',
          tip: 'Shown at the top of your dashboard on pushover.net after you sign in.' },
    ],
    gotify: [
        { k: 'url', label: 'Server URL', ph: 'http://192.168.1.20:8080',
          tip: 'The address of your Gotify server, as you open it in a browser.' },
        { k: 'token', label: 'App token', ph: 'Application token', secret: 'token_set',
          tip: 'In Gotify: Apps, Create application. Copy the token shown on the new app.' },
    ],
    email: [
        { k: 'smtp.host', label: 'Mail server', ph: 'smtp.example.com',
          tip: 'Your provider\'s outgoing (SMTP) server. For Gmail that is smtp.gmail.com, port 587, STARTTLS, and an app password (Google Account, Security, 2-Step Verification, App passwords).' },
        { k: 'smtp.port', label: 'Port', ph: '587', type: 'number', half: true },
        { k: 'smtp.security', label: 'Security', half: true, select: [['starttls', 'STARTTLS (usually port 587)'], ['tls', 'TLS (usually port 465)'], ['none', 'None (port 25, not private)']] },
        { k: 'smtp.username', label: 'Username', ph: 'Often your email address', half: true },
        { k: 'smtp.password', label: 'Password', secret: 'smtp.password_set', half: true, pw: true },
        { k: 'smtp.from', label: 'From', ph: 'Fox Bridge <bridge@example.com>',
          tip: 'The sender address. Many providers only allow your own address here.' },
        { k: 'target', label: 'Send to', ph: 'you@example.com, partner@example.com',
          tip: 'One or more addresses, separated by commas.' },
    ],
    webhook: [
        { k: 'url', label: 'URL', ph: 'https://example.com/hook',
          tip: 'Receives a JSON POST for every event: event, printer, file, progress, message, code, severity, time and, for prints that ended, the record. Handy for Home Assistant, n8n or Node-RED.' },
    ],
};

const NF_PRIVACY = 'Messages go from this machine straight to the service you pick. With Picture on, a camera photo is attached.';

function nfGet(o, k) { return k.split('.').reduce((a, p) => (a == null ? a : a[p]), o); }
function nfSet(o, k, v) {
    const ps = k.split('.');
    ps.slice(0, -1).forEach(p => { o = o[p] = o[p] || {}; });
    o[ps[ps.length - 1]] = v;
}
function nfService(kind) { return ((NF.data && NF.data.services) || []).find(s => s.kind === kind) || { kind, name: kind, picture: false }; }
function nfTitle(ch) { return ch.name || nfService(ch.kind).name; }

// Whole sentences, so each language can put the time where it belongs.
function nfAgo(ok, sec, error) {
    const d = Math.max(0, Math.round(Date.now() / 1000 - sec));
    const [n, unit] = d < 60 ? [0, ''] : d < 3600 ? [Math.floor(d / 60), 'min'] : d < 86400 ? [Math.floor(d / 3600), 'h'] : [Math.floor(d / 86400), 'd'];
    if (ok) return unit === '' ? 'Sent just now' : fill(unit === 'min' ? 'Sent {0} min ago' : unit === 'h' ? 'Sent {0} h ago' : 'Sent {0} d ago', n);
    if (unit === '') return fill('Failed just now: {0}', error);
    return fill(unit === 'min' ? 'Failed {0} min ago: {1}' : unit === 'h' ? 'Failed {0} h ago: {1}' : 'Failed {0} d ago: {1}', n, error);
}

function nfStyle() {
    if (document.getElementById('nfStyle')) return;
    const st = document.createElement('style');
    st.id = 'nfStyle';
    st.textContent = `
#settings-notifications .nf-card { border: 1px solid var(--border); border-radius: 12px; background: var(--panel); }
#settings-notifications .nf-card + .nf-card { margin-top: .625rem; }
#settings-notifications .nf-row { display: flex; align-items: center; gap: .75rem; padding-right: 1rem; }
#settings-notifications .nf-main { flex: 1; min-width: 0; min-height: 56px; display: flex; flex-direction: column; justify-content: center; gap: .125rem; padding: .5rem 0 .5rem 1rem; text-align: left; background: transparent; border: 0; color: var(--text); cursor: pointer; border-radius: 12px; }
#settings-notifications .nf-svc { font-size: .6875rem; color: var(--text-2); letter-spacing: .02em; }
#settings-notifications .nf-name { font-size: .9375rem; font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
#settings-notifications .nf-last { display: flex; align-items: center; gap: .375rem; font-size: .75rem; color: var(--text-2); min-width: 0; }
#settings-notifications .nf-last span:last-child { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
#settings-notifications .nf-dot { width: .5rem; height: .5rem; border-radius: 9999px; flex-shrink: 0; background: var(--border); }
#settings-notifications .nf-dot.ok { background: var(--accent); }
#settings-notifications .nf-dot.bad { background: var(--danger); }
#settings-notifications .nf-chev { color: var(--text-2); font-size: .75rem; transition: transform 150ms ease; }
#settings-notifications .nf-card.is-open .nf-chev { transform: rotate(180deg); }
#settings-notifications .nf-edit { border-top: 1px solid var(--border); padding: 1rem; }
#settings-notifications .nf-grid2 { display: grid; grid-template-columns: 1fr; gap: .75rem; }
@media (min-width: 640px) { #settings-notifications .nf-grid2 { grid-template-columns: 1fr 1fr; } #settings-notifications .nf-full { grid-column: 1 / -1; } }
#settings-notifications .nf-checks { display: grid; grid-template-columns: 1fr; gap: 0 1rem; }
@media (min-width: 640px) { #settings-notifications .nf-checks { grid-template-columns: 1fr 1fr; } }
#settings-notifications .nf-check { display: flex; align-items: center; gap: .625rem; min-height: 44px; font-size: .875rem; cursor: pointer; }
#settings-notifications .nf-check input { width: 1rem; height: 1rem; accent-color: var(--accent); flex-shrink: 0; }
#settings-notifications .nf-picker { display: grid; grid-template-columns: repeat(auto-fill, minmax(8.5rem, 1fr)); gap: .5rem; }
#settings-notifications .btn-primary, #settings-notifications .btn-outline, #settings-notifications .btn-ghost, #settings-notifications .btn-danger { min-height: 44px; }
#settings-notifications .switch { position: relative; }
#settings-notifications .switch::after { content: ''; position: absolute; inset: -14px -8px; }
#settings-notifications .nf-skel { height: 56px; border-radius: 12px; background: var(--panel-2); margin-top: .625rem; animation: nfPulse 1.4s ease-in-out infinite; }
@keyframes nfPulse { 50% { opacity: .5; } }
@media (prefers-reduced-motion: reduce) { #settings-notifications .nf-skel { animation: none; } #settings-notifications .nf-chev { transition: none; } }
#settings-notifications .nf-empty { border: 1px dashed var(--border); border-radius: 12px; padding: 1.25rem; font-size: .875rem; color: var(--text-2); }
#settings-notifications .nf-result { min-height: 1.25rem; font-size: .8125rem; }
#settings-notifications .nf-quiet { display: flex; flex-wrap: wrap; align-items: center; gap: .5rem .75rem; }
#settings-notifications .nf-quiet input[type=time] { width: 8.5rem; min-height: 44px; font-variant-numeric: tabular-nums; }
`;
    document.head.appendChild(st);
}

// ---- loading ----

function nfSkeleton() {
    return '<h2 class="font-heading text-xl font-medium leading-tight">Notifications</h2><div aria-busy="true" aria-label="Loading notifications"><div class="nf-skel"></div><div class="nf-skel"></div></div>';
}

async function nfLoad() {
    try {
        const v = await apiFetch('/api/notifications');
        NF.data = v;
        const keep = NF.open ? NF.chs.find(c => c._k === NF.open) : null;
        if (keep) {
            // An editor is open: keep it and what was typed; only the list rows refresh.
            const keys = new Map(NF.chs.filter(c => c.id).map(c => [c.id, c._k]));
            NF.chs = (v.channels || []).map(c => {
                if (!keep._draft && c.id === keep.id) { keep.last = c.last; return keep; }
                nfAdopt(c);
                if (keys.has(c.id)) c._k = keys.get(c.id);
                return c;
            });
            if (keep._draft) NF.chs.push(keep);
            NF.chs.forEach(c => { if (c !== keep && !c._deleted) nfUpdateRow(c); });
            nfUpdateRow(keep);
            return;
        }
        NF.chs = (v.channels || []).map(c => nfAdopt(c));
        NF.open = null;
        nfRender();
    } catch (e) {
        NF.sec.innerHTML = '<h2 class="font-heading text-xl font-medium leading-tight">Notifications</h2>'
            + '<p class="mt-3 text-sm text-danger">' + escHtml(friendlyError ? friendlyError(e.message) : e.message) + '</p>'
            + '<button type="button" class="btn-outline btn-lg mt-3" data-act="retry">Try again</button>';
    }
}

// nfAdopt marks a channel from the server with a client key and re-applies a delete that can still be undone.
function nfAdopt(c) {
    c._k = 'k' + (++NF.n);
    if (NF.pending.has(c.id)) c._deleted = true;
    return c;
}

function nfMount(sec) {
    NF.sec = sec;
    nfStyle();
    sec.innerHTML = nfSkeleton();
    sec.addEventListener('click', nfClick);
    sec.addEventListener('change', nfChange);
    nfLoad();
}

function nfShow() {
    // Refresh the last results when nothing is being edited.
    if (!NF.open && !NF.busy) nfLoad();
}

registerSettingsTab({ id: 'notifications', label: 'Notifications', group: 'Printing', order: 10, mount: nfMount, onShow: nfShow });

const NF_FT_KINDS = { telegram: 'Telegram', whatsapp: 'WhatsApp', discord: 'Discord', slack: 'Slack', email: 'email' };

// nfFtEvents returns the Set of Bridge event kinds FoxTrack sends to any channel, or null when FoxTrack's alerts are not known.
function nfFtEvents() {
    const fa = NF.data && NF.data.foxtrack_alerts;
    if (!fa) return null;
    const s = new Set();
    Object.keys(fa).forEach(k => (fa[k] || []).forEach(e => s.add(e)));
    return s;
}

// nfJoin writes "A, B and C".
function nfJoin(list) {
    if (list.length < 2) return list.join('');
    return list.slice(0, -1).join(', ') + ' ' + i18n('and') + ' ' + list[list.length - 1];
}

// ---- rendering ----

function nfLastHtml(ch) {
    const l = ch.last;
    if (!l) return '<span class="nf-dot"></span><span>Nothing sent yet</span>';
    const when = new Date(l.time * 1000).toLocaleString();
    if (l.ok) return '<span class="nf-dot ok"></span><span title="' + escHtml(when) + '">' + escHtml(nfAgo(true, l.time)) + '</span>';
    return '<span class="nf-dot bad"></span><span title="' + escHtml(l.error || '') + '">' + escHtml(nfAgo(false, l.time, l.error || 'unknown error')) + '</span>';
}

function nfRowHtml(ch) {
    const sw = ch._draft ? '' :
        '<button type="button" class="switch shrink-0" data-no-i18n role="switch" data-act="toggle" aria-checked="' + (ch.enabled ? 'true' : 'false') + '" aria-label="Send to ' + escHtml(nfTitle(ch)) + '"><span class="switch-thumb"></span></button>';
    return '<button type="button" class="nf-main" data-act="open" aria-expanded="' + (NF.open === ch._k ? 'true' : 'false') + '">'
        + '<span class="nf-svc">' + escHtml(nfService(ch.kind).name) + (ch._draft ? ' · new' : '') + '</span>'
        + '<span class="nf-name" data-no-i18n>' + escHtml(nfTitle(ch)) + '</span>'
        + (ch._draft ? '' : '<span class="nf-last">' + nfLastHtml(ch) + '</span>')
        + '</button>' + sw + '<i class="fas fa-chevron-down nf-chev" aria-hidden="true"></i>';
}

function nfRender() {
    const d = NF.data;
    const shown = NF.chs.filter(c => !c._deleted);
    let h = '<h2 class="font-heading text-xl font-medium leading-tight">Notifications</h2>'
        + '<p class="mt-1.5 text-sm text-text-2 flex items-center gap-1">Get a message when a print finishes or goes wrong, without any app. ' + tip(escHtml(NF_PRIVACY), 'About privacy') + '</p>';
    // A linked shop may also have FoxTrack's own alerts on: both mean two copies.
    if (typeof foxtrackLinked === 'function' && foxtrackLinked()) {
        const ft = nfFtEvents();
        if (!ft) {
            h += '<p class="mt-2 text-sm text-text-2">FoxTrack can send the same print alerts (in FoxTrack: Settings, Account, Alerts). Turn each alert on in one place only, or it arrives twice.</p>';
        } else {
            const mine = new Set();
            shown.forEach(c => { if (c.enabled && !c._draft) (c.events || []).forEach(e => mine.add(e)); });
            const both = d.events.filter(e => ft.has(e.kind) && mine.has(e.kind));
            if (both.length) {
                const kinds = Object.keys(d.foxtrack_alerts).filter(k => both.some(b => (d.foxtrack_alerts[k] || []).includes(b.kind)));
                h += '<p class="mt-2 text-sm text-text-2">' + escHtml(fill(i18n('FoxTrack already sends {0} to {1} (in FoxTrack: Settings, Account, Alerts). Turn each one on in one place only, or it arrives twice.'),
                    nfJoin(both.map(e => i18n(e.label))), nfJoin(kinds.map(k => i18n(NF_FT_KINDS[k] || k))))) + '</p>';
            }
        }
    }
    h += '<div class="mt-5" id="nfList">';
    if (!shown.length) {
        h += '<div class="nf-empty">No channels yet. Add one below.</div>';
    }
    for (const ch of shown) {
        h += '<div class="nf-card' + (NF.open === ch._k ? ' is-open' : '') + '" data-k="' + ch._k + '"><div class="nf-row">' + nfRowHtml(ch) + '</div>'
            + (NF.open === ch._k ? '<div class="nf-edit">' + nfEditorHtml(ch) + '</div>' : '') + '</div>';
    }
    h += '</div>';
    h += '<div class="mt-6"><p class="field-label">Add a channel</p><div class="nf-picker">'
        + d.services.map(s => '<button type="button" class="btn-outline btn-lg" data-act="add" data-kind="' + escHtml(s.kind) + '"><i class="fas fa-plus" aria-hidden="true"></i> ' + escHtml(s.name) + '</button>').join('')
        + '</div></div>';
    h += '<div class="mt-8 border-t border-border pt-5"><div class="label-row"><span class="field-label" id="nfQuietLabel">Quiet hours</span>'
        + tip('Between these times only failed prints, printer alarms and their all-clear are sent. Other messages are dropped. Uses this machine\'s clock.', 'About quiet hours') + '</div>'
        + '<div class="nf-quiet" role="group" aria-labelledby="nfQuietLabel">'
        + '<input id="nfQs" type="time" class="field" aria-label="Quiet hours start" value="' + escHtml(d.quiet_start || '') + '">'
        + '<span class="text-sm text-text-2">to</span>'
        + '<input id="nfQe" type="time" class="field" aria-label="Quiet hours end" value="' + escHtml(d.quiet_end || '') + '">'
        + '<button type="button" class="btn-ghost btn-lg" data-act="quiet-clear">Clear</button>'
        + '<span id="nfQuietMsg" class="text-xs text-text-2" role="status"></span></div></div>';
    h += '<div class="mt-5"><div class="label-row"><label for="nfAdm" class="field-label">Almost-done alert (minutes before the end)</label>'
        + tip('A channel that listens to “Almost done” gets a message once per print, when the printer’s own estimate says this many minutes are left. Leave it empty for the default of 10.', 'About the almost-done alert') + '</div>'
        + '<input id="nfAdm" type="number" inputmode="numeric" min="1" max="120" step="1" placeholder="10" class="field" style="width:6rem" aria-label="Almost-done alert (minutes before the end)" value="' + (d.almost_done_minutes || '') + '"></div>';
    NF.sec.innerHTML = h;
    if (NF.open) {
        const f = NF.sec.querySelector('.nf-edit input.field, .nf-edit select.field');
        if (f && NF.focusOnRender) f.focus();
    }
    NF.focusOnRender = false;
}

function nfFieldHtml(ch, f, key) {
    const id = 'nf-' + key + '-' + f.k.replace('.', '-');
    const v = nfGet(ch, f.k);
    const isSet = f.secret ? !!nfGet(ch, f.secret) : false;
    let h = '<div' + (f.half ? '' : ' class="nf-full"') + '><div class="label-row"><label for="' + id + '" class="field-label">' + escHtml(f.label) + '</label>'
        + (f.tip ? tip(escHtml(f.tip), fill('Where to find: {0}', i18n(f.label))) : '') + '</div>';
    if (f.select) {
        h += '<div class="select-wrap"><select id="' + id + '" class="field" data-f="' + f.k + '">'
            + f.select.map(o => '<option value="' + o[0] + '"' + ((v || 'starttls') === o[0] ? ' selected' : '') + '>' + escHtml(o[1]) + '</option>').join('')
            + '</select><i class="fas fa-chevron-down" aria-hidden="true"></i></div>';
    } else {
        const secret = !!f.secret;
        const ph = secret && isSet ? 'Saved. Type to replace it.' : (f.ph || '');
        h += '<input id="' + id + '" data-f="' + f.k + '" class="field' + (f.k === 'url' || f.k === 'token' ? ' field-mono' : '') + '" autocomplete="' + (secret ? 'new-password' : 'off') + '" spellcheck="false" '
            + 'type="' + (secret && (f.pw || f.k.endsWith('token') || f.k === 'url' && ch.kind !== 'ntfy') ? 'password' : (f.type || 'text')) + '" '
            + (f.type === 'number' ? 'min="1" max="65535" inputmode="numeric" ' : '')
            + 'placeholder="' + escHtml(ph) + '" value="' + escHtml(secret || v == null ? '' : String(v)) + '">';
        if (secret && isSet && f.optional) {
            h += '<button type="button" class="btn-ghost btn-xs mt-1.5" data-act="clear-secret" data-f="' + f.k + '">Remove the saved ' + (f.k === 'token' ? 'token' : 'value') + '</button>';
        }
    }
    return h + '</div>';
}

function nfEditorHtml(ch) {
    const svc = nfService(ch.kind);
    const key = ch._k;
    const fields = NF_FIELDS[ch.kind] || [];
    const chosen = new Set(ch.events || []);
    const ft = nfFtEvents();
    let h = '<div class="nf-grid2">';
    h += '<div class="nf-full"><label for="nf-' + key + '-name" class="field-label">Name (optional)</label>'
        + '<input id="nf-' + key + '-name" data-f="name" class="field" maxlength="60" autocomplete="off" placeholder="' + escHtml(svc.name) + '" value="' + escHtml(ch.name || '') + '"></div>';
    h += fields.map(f => nfFieldHtml(ch, f, key)).join('');
    h += '</div>';

    h += '<fieldset class="mt-5"><legend class="field-label">Send a message when</legend><div class="nf-checks">'
        + NF.data.events.map(e => '<label class="nf-check"><input type="checkbox" data-ev="' + e.kind + '"' + (chosen.has(e.kind) ? ' checked' : '') + '> ' + escHtml(e.label) + (ft && (typeof foxtrackLinked === 'function' && foxtrackLinked()) && ft.has(e.kind) ? ' <span class="text-xs text-text-2">' + escHtml(i18n('(FoxTrack sends it too)')) + '</span>' : '') + '</label>').join('')
        + '</div></fieldset>';

    const ps = (typeof configuredPrinters !== 'undefined' && configuredPrinters) || [];
    const all = !(ch.printers && ch.printers.length);
    if (ps.length > 1 || !all) {
        h += '<div class="mt-4"><div class="flex items-center justify-between gap-4"><div class="min-w-0"><p class="text-sm font-medium flex items-center gap-1" id="nf-' + key + '-allL">All printers' + tip('Turn off to pick which printers this channel hears about.', 'About all printers') + '</p></div>'
            + '<button type="button" id="nf-' + key + '-all" class="switch shrink-0" role="switch" data-act="all-printers" aria-checked="' + (all ? 'true' : 'false') + '" aria-labelledby="nf-' + key + '-allL"><span class="switch-thumb"></span></button></div>'
            + '<div id="nf-' + key + '-plist" class="nf-checks mt-1' + (all ? ' hidden' : '') + '">'
            + ps.map(p => '<label class="nf-check"><input type="checkbox" data-pr="' + escHtml(p.id) + '"' + ((ch.printers || []).includes(p.id) ? ' checked' : '') + '> ' + escHtml(p.name) + '</label>').join('')
            + '</div></div>';
    }

    if (svc.picture) {
        h += '<div class="mt-4 flex items-center justify-between gap-4"><div class="min-w-0"><p class="text-sm font-medium" id="nf-' + key + '-picL">Picture</p>'
            + tip('<span class="block">Attach a camera photo to each message.</span><span class="block mt-1.5">' + fill('The photo is sent to {0} along with the message. Printers without a camera send text only.', escHtml(svc.name)) + '</span>', 'About the picture') + '</div>'
            + '<button type="button" id="nf-' + key + '-pic" class="switch shrink-0" role="switch" data-act="picture" aria-checked="' + (ch.snapshot ? 'true' : 'false') + '" aria-labelledby="nf-' + key + '-picL"><span class="switch-thumb"></span></button></div>';
    }

    h += '<p class="nf-result mt-4 text-text-2" role="status" aria-live="polite" data-result></p>';
    h += '<div class="mt-2 flex flex-wrap items-center gap-2">'
        + '<button type="button" class="btn-primary btn-lg" data-act="save">Save</button>'
        + '<button type="button" class="btn-outline btn-lg" data-act="test">Send test</button>'
        + '<button type="button" class="btn-ghost btn-lg" data-act="cancel">' + (ch._draft ? 'Discard' : 'Close') + '</button>'
        + (ch._draft ? '' : '<button type="button" class="btn-danger btn-lg ml-auto" data-act="delete">Remove</button>')
        + '</div>';
    return h;
}

// ---- reading the form ----

function nfCardOf(el) { return el.closest('.nf-card'); }
function nfByKey(k) { return NF.chs.find(c => c._k === k); }

// nfRead returns the channel as the form shows it. Secrets left blank stay out.
function nfRead(card, ch) {
    const out = { id: ch.id || '', kind: ch.kind, enabled: ch._draft ? true : !!ch.enabled, smtp: ch.smtp ? { ...ch.smtp } : undefined };
    for (const k of ['url', 'target']) if (ch[k] != null) out[k] = ch[k];
    card.querySelectorAll('[data-f]').forEach(el => {
        if (el.tagName === 'BUTTON') return;
        const f = el.dataset.f;
        let v = el.value.trim();
        if (el.type === 'number') v = v === '' ? 0 : parseInt(v, 10);
        nfSet(out, f, v);
    });
    if (card.querySelector('[data-f="token"]') == null) delete out.token;
    out.events = [...card.querySelectorAll('[data-ev]')].filter(x => x.checked).map(x => x.dataset.ev);
    const all = card.querySelector('[data-act="all-printers"]');
    out.printers = !all || all.getAttribute('aria-checked') === 'true' ? [] : [...card.querySelectorAll('[data-pr]')].filter(x => x.checked).map(x => x.dataset.pr);
    const pic = card.querySelector('[data-act="picture"]');
    out.snapshot = !!pic && pic.getAttribute('aria-checked') === 'true';
    if (ch._clear) {
        if (ch._clear.token) out.clear_token = true;
        if (ch._clear.password) out.clear_password = true;
    }
    return out;
}

// nfReq is what the server gets for a channel that is not being edited: the
// view as it came (no secrets), so blank secrets keep the stored ones.
function nfReq(c) {
    return { id: c.id, name: c.name, kind: c.kind, enabled: !!c.enabled, url: c.url, target: c.target, smtp: c.smtp, events: c.events, printers: c.printers, snapshot: !!c.snapshot };
}

async function nfPost(channels, extra) {
    return apiFetch('/api/notifications', { method: 'POST', body: JSON.stringify({ channels, ...extra }) });
}

// nfPayload lists every saved channel (including ones with a delete that can
// still be undone, so undo never loses a secret), with `swap` in place of its original.
function nfPayload(swap) {
    const list = [];
    for (const c of NF.chs) {
        if (c._draft && c !== swap.orig) continue;
        list.push(c === swap.orig ? swap.req : nfReq(c));
    }
    return list;
}

function nfApply(view) {
    NF.data = view;
    NF.chs = (view.channels || []).map(nfAdopt);
}

// ---- actions ----

function nfClick(ev) {
    const t = ev.target.closest('[data-act]');
    if (!t || !NF.sec.contains(t)) return;
    const act = t.dataset.act;
    const card = nfCardOf(t);
    const ch = card ? nfByKey(card.dataset.k) : null;
    switch (act) {
        case 'retry': nfLoad(); break;
        case 'add': nfAdd(t.dataset.kind); break;
        case 'open': nfOpen(ch); break;
        case 'toggle': ev.stopPropagation(); nfToggle(ch, t); break;
        case 'all-printers': {
            const on = t.getAttribute('aria-checked') !== 'true';
            t.setAttribute('aria-checked', on ? 'true' : 'false');
            card.querySelector('[id$="-plist"]').classList.toggle('hidden', on);
            break;
        }
        case 'picture': t.setAttribute('aria-checked', t.getAttribute('aria-checked') === 'true' ? 'false' : 'true'); break;
        case 'clear-secret': {
            ch._clear = ch._clear || {};
            ch._clear.token = true;
            const inp = card.querySelector('[data-f="' + t.dataset.f + '"]');
            inp.placeholder = 'Will be removed when you save';
            t.remove();
            break;
        }
        case 'save': nfSave(card, ch, t); break;
        case 'test': nfTest(card, ch, t); break;
        case 'cancel': nfClose(ch); break;
        case 'delete': nfDelete(ch); break;
        case 'quiet-clear': document.getElementById('nfQs').value = ''; document.getElementById('nfQe').value = ''; nfSaveQuiet(); break;
    }
}

function nfChange(ev) {
    if (ev.target.id === 'nfQs' || ev.target.id === 'nfQe') nfSaveQuiet();
    if (ev.target.id === 'nfAdm') nfSaveAlmostDone();
    if (ev.target.dataset && ev.target.dataset.f === 'smtp.security') {
        // The server picks 465 for TLS and 25 for None only when the port is 0,
        // so a port left at the previous security's default would be ignored.
        const card = ev.target.closest('.nf-card');
        const port = card && card.querySelector('[data-f="smtp.port"]');
        if (port && (port.value === '' || port.value === '587' || port.value === '465' || port.value === '25'))
            port.value = { starttls: '587', tls: '465', none: '25' }[ev.target.value] || '';
    }
}

function nfAdd(kind) {
    // One editor at a time: an unsaved draft is dropped when another is started.
    NF.chs = NF.chs.filter(c => !c._draft);
    const svc = nfService(kind);
    const ch = { _k: 'k' + (++NF.n), _draft: true, kind, enabled: true, events: NF.data.events.filter(e => e.default).map(e => e.kind), printers: [], snapshot: !!svc.picture, smtp: kind === 'email' ? { security: 'starttls' } : undefined };
    NF.chs.push(ch);
    NF.open = ch._k;
    NF.focusOnRender = true;
    nfRender();
    const card = NF.sec.querySelector('[data-k="' + ch._k + '"]');
    if (card) card.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
}

function nfOpen(ch) {
    if (!ch) return;
    if (NF.open === ch._k) return nfClose(ch);
    NF.chs = NF.chs.filter(c => !c._draft || c === ch);
    NF.open = ch._k;
    NF.focusOnRender = true;
    nfRender();
}

function nfClose(ch) {
    if (ch && ch._draft) NF.chs = NF.chs.filter(c => c !== ch);
    if (ch) ch._clear = null;
    NF.open = null;
    nfRender();
}

function nfUpdateRow(ch) {
    const card = NF.sec.querySelector('[data-k="' + ch._k + '"] .nf-row');
    if (!card) return;
    const focus = document.activeElement && card.contains(document.activeElement) ? document.activeElement.dataset.act : null;
    card.innerHTML = nfRowHtml(ch);
    if (focus) { const b = card.querySelector('[data-act="' + focus + '"]'); if (b) b.focus(); }
}

async function nfToggle(ch, btn) {
    if (!ch || NF.busy) return;
    const on = btn.getAttribute('aria-checked') !== 'true';
    btn.setAttribute('aria-checked', on ? 'true' : 'false');
    NF.busy = true;
    try {
        await nfPost(nfPayload({ orig: ch, req: { ...nfReq(ch), enabled: on } }));
        ch.enabled = on; // only the row changes, so an open editor keeps what was typed
    } catch (e) {
        btn.setAttribute('aria-checked', on ? 'false' : 'true');
        toast(e.message, 'error');
    } finally { NF.busy = false; }
}

async function nfSave(card, ch, btn) {
    if (!ch || NF.busy) return;
    const req = nfRead(card, ch);
    const out = card.querySelector('[data-result]');
    const all = card.querySelector('[data-act="all-printers"]');
    if (all && all.getAttribute('aria-checked') !== 'true' && !req.printers.length) {
        out.className = 'nf-result mt-4 text-danger';
        out.textContent = 'Pick at least one printer, or choose all printers.';
        return;
    }
    NF.busy = true;
    btn.disabled = true;
    out.className = 'nf-result mt-4 text-text-2';
    out.textContent = 'Saving…';
    try {
        const view = await nfPost(nfPayload({ orig: ch, req }));
        const others = NF.chs.filter(c => c._draft && c !== ch);
        nfApply(view);
        NF.chs.push(...others);
        NF.open = null;
        nfRender();
        toast('Saved');
    } catch (e) {
        out.className = 'nf-result mt-4 text-danger';
        out.textContent = friendlyError(e.message);
        btn.disabled = false;
    } finally { NF.busy = false; }
}

async function nfTest(card, ch, btn) {
    if (!ch) return;
    const req = nfRead(card, ch);
    const out = card.querySelector('[data-result]');
    btn.disabled = true;
    out.className = 'nf-result mt-4 text-text-2';
    out.textContent = 'Sending a test…';
    try {
        const r = await apiFetch('/api/notifications/test', { method: 'POST', body: JSON.stringify({ channel: req }) });
        if (r.ok) {
            out.className = 'nf-result mt-4 text-text';
            out.textContent = fill('Test sent. Check {0}.', nfService(ch.kind).name);
        } else {
            out.className = 'nf-result mt-4 text-danger';
            out.textContent = r.error || 'The test failed.';
        }
        if (!ch._draft) { ch.last = { time: Math.floor(Date.now() / 1000), ok: !!r.ok, error: r.error }; nfUpdateRow(ch); }
    } catch (e) {
        out.className = 'nf-result mt-4 text-danger';
        out.textContent = friendlyError(e.message);
    } finally { btn.disabled = false; }
}

// ---- delete with undo ----

function nfDelete(ch) {
    if (!ch) return;
    if (ch._draft) return nfClose(ch);
    ch._deleted = true;
    NF.open = null;
    nfRender();
    const id = ch.id;
    const bar = undoBar('Removed', nfTitle(ch), {
        ms: 6000,
        onUndo() {
            if (!NF.pending.has(id)) return;
            NF.pending.delete(id);
            NF.chs.forEach(c => { if (c.id === id) c._deleted = false; });
            nfRender();
        },
        onCommit() {
            // Waits its turn behind a save in progress.
            (async () => {
                while (NF.busy) await new Promise(r => setTimeout(r, 100));
                NF.busy = true;
                try {
                    const keep = NF.chs.filter(c => c.id !== id && !c._draft).map(nfReq);
                    await nfPost(keep, keep.length ? {} : { confirm_clear_channels: true });
                    NF.pending.delete(id);
                    NF.chs = NF.chs.filter(c => c.id !== id); // the card is already hidden
                } catch (e) {
                    NF.pending.delete(id);
                    NF.chs.forEach(c => { if (c.id === id) c._deleted = false; });
                    nfRender();
                    toast('Could not remove ' + nfTitle(ch) + ': ' + e.message, 'error');
                } finally { NF.busy = false; }
            })();
        },
    });
    NF.pending.set(id, { bar });
}

// ---- quiet hours ----

async function nfSaveQuiet() {
    const s = document.getElementById('nfQs').value, e = document.getElementById('nfQe').value;
    const msg = document.getElementById('nfQuietMsg');
    if ((s === '') !== (e === '')) { msg.className = 'text-xs text-text-2'; msg.textContent = 'Set both times, or clear them.'; return; }
    try {
        const v = await apiFetch('/api/notifications', { method: 'POST', body: JSON.stringify({ quiet_start: s, quiet_end: e }) });
        NF.data.quiet_start = v.quiet_start; NF.data.quiet_end = v.quiet_end;
        msg.className = 'text-xs text-text-2';
        msg.textContent = s ? fill('Saved. Only failures and alarms get through from {0} to {1}.', s, e) : 'Saved. No quiet hours.';
    } catch (err) {
        msg.className = 'text-xs text-danger';
        msg.textContent = friendlyError(err.message);
    }
}

// nfSaveAlmostDone saves the almost-done threshold the same way quiet hours
// are saved: one POST of just the setting, the reply updates the view.
async function nfSaveAlmostDone() {
    const inp = document.getElementById('nfAdm');
    if (!inp) return;
    try {
        const v = await apiFetch('/api/notifications', { method: 'POST', body: JSON.stringify({ almost_done_minutes: inp.value === '' ? 0 : parseInt(inp.value, 10) }) });
        NF.data.almost_done_minutes = v.almost_done_minutes;
        inp.value = v.almost_done_minutes || '';
    } catch (err) {
        toast(friendlyError(err.message), 'error');
    }
}
