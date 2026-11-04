// Settings > Home Assistant: publish printer state to the user's own MQTT
// broker, with Home Assistant discovery. Talks to /api/mqtt-export.
registerSettingsTab({
    id: 'homeassistant', label: 'Home Assistant', group: 'Connect', order: 10,
    mount(sec) { haMount(sec); },
    onShow() { haRefreshStatus(); },
});

let haSec = null, haStored = null, haPoll = 0;

function haEl(id) { return haSec.querySelector('#' + id); }

function haMount(sec) {
    haSec = sec;
    sec.innerHTML = `
<style>
#settings-homeassistant .ha-dot { display: inline-block; width: .5rem; height: .5rem; margin-right: .5rem; border-radius: 9999px; background: var(--text-2); }
#settings-homeassistant .ha-dot.ok { background: var(--accent); }
#settings-homeassistant .ha-dot.bad { background: var(--danger); }
#settings-homeassistant .switch::after { content: ''; position: absolute; inset: -12px -6px; }
@media (max-width: 640px) { #settings-homeassistant .field, #settings-homeassistant .btn-lg { height: 2.75rem; } }
</style>
<div class="flex items-start justify-between gap-3">
    <h2 class="font-heading text-xl font-medium leading-tight">Home Assistant</h2>
    <button id="haEnabled" type="button" class="switch shrink-0 mt-1" role="switch" aria-checked="false" aria-label="Send printer state to an MQTT broker"><span class="switch-thumb"></span></button>
</div>
<p class="mt-1.5 text-sm text-text-2 flex items-center gap-1">Show your printers in Home Assistant, or react to them from any MQTT client.
    ${tip('<span class="block">In Home Assistant, open Settings, then Devices and services, then MQTT. Each printer is a device there, with sensors, pause, resume and stop buttons, and a light.</span><span class="block mt-1.5">Home Assistant has to use the same broker. Automations can also listen on the printer\'s event topic for prints that start, finish or fail.</span>', 'How to find the printers in Home Assistant')}
</p>
<p class="mt-1.5 text-xs text-text-2">Printer state goes to the broker you name here and nowhere else.</p>
<p id="haConn" class="mt-4 text-sm" role="status" aria-live="polite"></p>

<div class="mt-5">
    <label for="haBroker" class="field-label">Broker address</label>
    <input id="haBroker" type="text" class="field field-mono" autocomplete="off" spellcheck="false" placeholder="tcp://192.168.1.10:1883">
    <p class="text-xs text-text-2 mt-1.5">Use tcp:// for a plain connection or ssl:// for an encrypted one.</p>
</div>
<div class="mt-3 grid grid-cols-1 sm:grid-cols-2 gap-3">
    <div>
        <label for="haUser" class="field-label">Username</label>
        <input id="haUser" type="text" class="field" autocomplete="off" placeholder="Optional">
    </div>
    <div>
        <label for="haPass" class="field-label">Password</label>
        <input id="haPass" type="password" class="field" autocomplete="new-password" placeholder="Optional">
        <p id="haPassHint" class="text-xs text-text-2 mt-1.5">Kept only on this machine.</p>
    </div>
</div>
<div id="haSkipRow" class="hidden mt-4 flex items-start justify-between gap-4">
    <div class="min-w-0">
        <p class="text-sm font-medium">Skip certificate check</p>
        <p class="text-xs text-text-2 mt-0.5">For a broker with a self-signed certificate. Anyone on your network could then pose as the broker.</p>
    </div>
    <button id="haSkip" type="button" class="switch shrink-0 mt-0.5" role="switch" aria-checked="false" aria-label="Skip the broker certificate check"><span class="switch-thumb"></span></button>
</div>
<div class="mt-3">
    <label for="haPrefix" class="field-label">Topic prefix</label>
    <input id="haPrefix" type="text" class="field field-mono" autocomplete="off" spellcheck="false" placeholder="foxtrack_bridge">
    <p class="text-xs text-text-2 mt-1.5">State goes to prefix/printer id/state, events to prefix/printer id/event.</p>
</div>
<div class="mt-5 flex items-start justify-between gap-4">
    <div class="min-w-0">
        <p class="text-sm font-medium">Home Assistant discovery</p>
        <p class="text-xs text-text-2 mt-0.5">Adds each printer to Home Assistant by itself, and removes it when you delete the printer.</p>
    </div>
    <button id="haDiscovery" type="button" class="switch shrink-0 mt-0.5" role="switch" aria-checked="false" aria-label="Home Assistant discovery"><span class="switch-thumb"></span></button>
</div>
<div id="haDiscRow" class="mt-3">
    <label for="haDiscPrefix" class="field-label">Discovery prefix</label>
    <input id="haDiscPrefix" type="text" class="field field-mono" autocomplete="off" spellcheck="false" placeholder="homeassistant">
    <p class="text-xs text-text-2 mt-1.5">Leave this as it is unless you changed it in Home Assistant.</p>
</div>

<p id="haError" class="hidden text-xs text-danger mt-4"></p>
<button id="haRetry" type="button" class="btn-outline btn-lg mt-2 hidden">Try again</button>
<div class="mt-6 flex flex-wrap items-center justify-end gap-3">
    <span id="haTestResult" class="text-xs text-text-2 mr-auto min-w-0" role="status" aria-live="polite"></span>
    <button id="haTest" type="button" class="btn-outline btn-lg">Test</button>
    <button id="haSave" type="button" class="btn-primary btn-lg">Save</button>
</div>`;

    const toggle = (id, after) => haEl(id).addEventListener('click', () => { setSwitch(id, !getSwitch(id)); if (after) after(); });
    toggle('haEnabled');
    toggle('haSkip');
    toggle('haDiscovery', haShowRows);
    haEl('haBroker').addEventListener('input', haShowRows);
    haEl('haTest').addEventListener('click', haTest);
    haEl('haSave').addEventListener('click', haSave);
    haEl('haRetry').addEventListener('click', haLoad);
    haLoad();
    setInterval(() => { if (!document.hidden && !haSec.classList.contains('hidden')) haRefreshStatus(); }, 10000);
}

// The certificate switch only means something for ssl://; the discovery prefix
// only when discovery is on.
function haShowRows() {
    haEl('haSkipRow').classList.toggle('hidden', !/^\s*(ssl|tls|mqtts):\/\//i.test(haEl('haBroker').value));
    haEl('haDiscRow').classList.toggle('hidden', !getSwitch('haDiscovery'));
}

function haFill(v) {
    haStored = v;
    setSwitch('haEnabled', !!v.enabled);
    setSwitch('haSkip', !!v.skip_cert_check);
    setSwitch('haDiscovery', !!v.discovery);
    haEl('haBroker').value = v.broker || '';
    haEl('haUser').value = v.username || '';
    haEl('haPass').value = '';
    haEl('haPassHint').textContent = v.password_set ? 'A password is saved. Leave blank to keep it.' : 'Kept only on this machine.';
    haEl('haPrefix').value = v.topic_prefix || '';
    haEl('haDiscPrefix').value = v.discovery_prefix || '';
    haShowRows();
    haShowStatus(v);
}

function haShowStatus(v) {
    let dot = '', text;
    if (!v.enabled) text = 'Off. Nothing is sent until you turn it on and save.';
    else if (v.connected) { dot = 'ok'; text = 'Connected to the broker.'; }
    else if (v.error) { dot = 'bad'; text = 'Not connected. ' + v.error; }
    else text = 'Connecting to the broker…';
    haEl('haConn').innerHTML = '<span class="ha-dot ' + dot + '"></span>' + escHtml(text);
}

// Until the saved settings have loaded, Save would post blanks over them.
function haLock(on) {
    haSec.querySelectorAll('input, button:not(#haRetry)').forEach(el => { el.disabled = on; });
}

async function haLoad() {
    haEl('haError').classList.add('hidden');
    haEl('haRetry').classList.add('hidden');
    haLock(true);
    try { haFill(await apiFetch('/api/mqtt-export')); haLock(false); }
    catch (e) {
        haShowError("Couldn't load these settings. " + friendlyError(e.message));
        haEl('haRetry').classList.remove('hidden');
    }
}

async function haRefreshStatus() {
    if (!haSec || !haStored) return;
    try {
        const v = await apiFetch('/api/mqtt-export');
        haShowStatus(v);
        haStored = v;
    } catch (_) { /* the next refresh tries again */ }
}

function haShowError(msg) {
    const el = haEl('haError');
    el.textContent = msg;
    el.classList.toggle('hidden', !msg);
}

// The form as the server takes it. The password is sent only when typed, so a
// save never wipes the one that is kept.
function haBody() {
    const body = {
        enabled: getSwitch('haEnabled'),
        broker: haEl('haBroker').value.trim(),
        username: haEl('haUser').value.trim(),
        topic_prefix: haEl('haPrefix').value.trim(),
        discovery: getSwitch('haDiscovery'),
        discovery_prefix: haEl('haDiscPrefix').value.trim(),
        skip_cert_check: getSwitch('haSkip'),
    };
    const pass = haEl('haPass').value;
    if (pass !== '') body.password = pass;
    return body;
}

async function haSave() {
    const btn = haEl('haSave');
    haShowError('');
    haEl('haTestResult').textContent = '';
    btn.disabled = true;
    try {
        const v = await apiFetch('/api/mqtt-export', { method: 'POST', body: JSON.stringify(haBody()) });
        haFill(v);
        toast(v.enabled ? 'Saved. Connecting to the broker.' : 'Saved.', 'success');
        if (v.enabled) haWatchConnect(0);
    } catch (e) {
        haShowError(e.message);
    } finally {
        btn.disabled = false;
    }
}

// After a save the connection takes a moment; look a few times.
function haWatchConnect(n) {
    clearTimeout(haPoll);
    haPoll = setTimeout(async () => {
        try {
            const v = await apiFetch('/api/mqtt-export');
            haShowStatus(v);
            if (!v.connected && !v.error && n < 6) haWatchConnect(n + 1);
        } catch (_) { /* leave the last status */ }
    }, 1500);
}

async function haTest() {
    const btn = haEl('haTest'), out = haEl('haTestResult');
    haShowError('');
    btn.disabled = true;
    out.textContent = 'Testing…';
    try {
        const r = await apiFetch('/api/mqtt-export/test', { method: 'POST', body: JSON.stringify(haBody()) });
        out.textContent = r.message;
        out.className = 'text-xs mr-auto min-w-0 ' + (r.ok ? 'text-text' : 'text-danger');
    } catch (e) {
        out.textContent = e.message;
        out.className = 'text-xs mr-auto min-w-0 text-danger';
    } finally {
        btn.disabled = false;
    }
}
