// Settings > Prometheus: turn /metrics on or off, and show how to scrape it.
registerSettingsTab({
    id: 'metrics', label: 'Prometheus', group: 'Connect', order: 30,
    mount(sec) { mxMount(sec); },
});

function mxScrapeConfig() {
    const https = location.protocol === 'https:';
    return 'scrape_configs:\n'
        + '  - job_name: foxtrack-bridge\n'
        + '    scrape_interval: 30s\n'
        + (https ? '    scheme: https\n' : '')
        + '    static_configs:\n'
        + "      - targets: ['" + location.host + "']\n"
        + '    # With a dashboard password, add an API token from Security:\n'
        + '    # authorization:\n'
        + '    #   credentials: YOUR_API_TOKEN\n';
}

async function mxCopy(text, source, what) {
    const ok = typeof copyText === 'function'
        ? await copyText(text, source)
        : !!(navigator.clipboard && await navigator.clipboard.writeText(text).then(() => true, () => false));
    if (ok) toast('Copied the ' + what + '.', 'success');
    else toast("Couldn't copy. Select the text and copy it by hand.", 'error');
}

function mxMount(sec) {
    const url = location.origin + '/metrics';
    const local = ['localhost', '127.0.0.1', '[::1]'].includes(location.hostname);
    sec.innerHTML = `
<style>
#settings-metrics .switch::after { content: ''; position: absolute; inset: -12px -6px; }
#settings-metrics pre { margin: 0; white-space: pre; font-size: .8125rem; tab-size: 2; }
@media (max-width: 640px) { #settings-metrics .field, #settings-metrics .btn-lg { height: 2.75rem; } }
</style>
<div class="flex items-start justify-between gap-3">
    <h2 class="font-heading text-xl font-medium leading-tight">Prometheus</h2>
    <button id="mxEnabled" type="button" class="switch shrink-0 mt-1" role="switch" aria-checked="false" aria-label="Serve Prometheus metrics"><span class="switch-thumb"></span></button>
</div>
<p class="mt-1.5 text-sm text-text-2">Let Prometheus, Grafana or any scraper read your printers' temperatures, progress and print totals.</p>
<p class="mt-1.5 text-xs text-text-2">Your scraper asks this Bridge for the numbers. The Bridge sends nothing out.</p>
<p id="mxState" class="mt-4 text-sm" role="status" aria-live="polite"></p>

<div class="mt-5">
    <label for="mxUrl" class="field-label">Metrics address</label>
    <div class="flex gap-2">
        <input id="mxUrl" type="text" readonly class="field field-mono" value="${escHtml(url)}">
        <button id="mxCopyUrl" type="button" class="btn-outline btn-lg shrink-0" aria-label="Copy the metrics address"><i class="fas fa-copy"></i></button>
    </div>
    ${local ? '<p class="text-xs text-text-2 mt-1.5">You are on localhost. From another machine, use this machine\'s network address instead.</p>' : ''}
</div>

<p class="mt-4 text-xs text-text-2">If you set a dashboard password, create an API token in Security and add it as a bearer token in your scrape config.</p>

<div class="mt-5">
    <div class="flex items-center justify-between gap-3 mb-2">
        <span class="field-label" style="margin:0" id="mxSnipLabel">Sample prometheus.yml</span>
        <button id="mxCopySnip" type="button" class="btn-outline btn-lg" aria-label="Copy the sample prometheus.yml"><i class="fas fa-copy mr-1"></i>Copy</button>
    </div>
    <div class="rounded-md bg-panel-2 p-3 overflow-x-auto"><pre id="mxSnip" class="font-mono" tabindex="0" aria-labelledby="mxSnipLabel">${escHtml(mxScrapeConfig())}</pre></div>
</div>`;

    const state = (on) => {
        document.getElementById('mxState').textContent = on
            ? 'On. The address above answers with metrics.'
            : 'Off. The address above answers 404 until you turn this on.';
    };
    document.getElementById('mxCopyUrl').addEventListener('click', () => mxCopy(url, document.getElementById('mxUrl'), 'address'));
    document.getElementById('mxCopySnip').addEventListener('click', () => mxCopy(mxScrapeConfig(), document.getElementById('mxSnip'), 'sample'));
    // The switch saves at once: there is nothing else to fill in.
    const sw = document.getElementById('mxEnabled');
    sw.addEventListener('click', async () => {
        const on = !getSwitch('mxEnabled');
        sw.disabled = true;
        try {
            const r = await apiFetch('/api/metrics', { method: 'POST', body: JSON.stringify({ enabled: on }) });
            setSwitch('mxEnabled', !!r.enabled);
            state(!!r.enabled);
            toast(r.enabled ? 'Prometheus metrics are on.' : 'Prometheus metrics are off.', 'success');
        } catch (e) {
            toast("Couldn't change that. " + friendlyError(e.message), 'error');
        } finally {
            sw.disabled = false;
        }
    });
    document.getElementById('mxState').textContent = 'Checking…';
    apiFetch('/api/metrics').then(r => { setSwitch('mxEnabled', !!r.enabled); state(!!r.enabled); })
        .catch(e => { document.getElementById('mxState').textContent = "Couldn't load this setting. " + friendlyError(e.message); });
}
