// Costs: what filament and electricity cost, and what the last days of
// printing came to. Prices live on this machine (config.json); nothing is sent.
const csDefaultMaterials = ['PLA', 'PETG', 'ABS', 'ASA', 'TPU'];
const cs = { root: null, loaded: false, currency: '', days: 30 };

const csStyle = document.createElement('style');
csStyle.textContent = `
#settings-costs .cs-grid { display: grid; grid-template-columns: 1fr; gap: .75rem; }
@media (min-width: 640px) { #settings-costs .cs-grid { grid-template-columns: repeat(3, 1fr); } }
#settings-costs input, #settings-costs .cs-v { font-variant-numeric: tabular-nums; }
#settings-costs .cs-mat { display: grid; grid-template-columns: 1fr 1fr 2.25rem; gap: .5rem; align-items: center; margin-bottom: .5rem; }
#settings-costs .cs-x { width: 2.25rem; height: 2.25rem; display: inline-flex; align-items: center; justify-content: center; border: 0; background: transparent; color: var(--text-2); border-radius: var(--radius); cursor: pointer; }
#settings-costs .cs-x:hover { background: var(--panel); color: var(--text); }
#settings-costs .cs-row { padding: .75rem 0; border-top: 1px solid var(--border); }
#settings-costs .cs-stats { display: grid; grid-template-columns: repeat(4, 1fr); gap: .5rem; margin-top: .375rem; }
#settings-costs .cs-v { display: block; font-size: .875rem; font-weight: 500; overflow-wrap: anywhere; }
#settings-costs .cs-l { display: block; font-size: .6875rem; color: var(--text-2); }
#settings-costs .cs-days { width: 9.5rem; }
#settings-costs .cs-spend { margin-top: 2rem; padding-top: 1.5rem; border-top: 1px solid var(--border); }
#settings-costs .cs-skel { height: 3rem; border-radius: var(--radius); background: var(--panel-2); margin-top: .5rem; }
@media (max-width: 640px) {
  #settings-costs .field, #settings-costs .btn-primary, #settings-costs .btn-outline { min-height: 2.75rem; }
  #settings-costs .cs-mat { grid-template-columns: 1fr 1fr 2.75rem; }
  #settings-costs .cs-x { width: 2.75rem; height: 2.75rem; }
}`;
document.head.appendChild(csStyle);

function csNum(n, digits = 0) {
    return Number(n || 0).toLocaleString(undefined, { minimumFractionDigits: digits, maximumFractionDigits: digits });
}
function csMoney(n) {
    const a = csNum(n, 2), c = cs.currency;
    return !c ? a : (c.length > 1 ? c + ' ' + a : c + a);
}
function csGrams(g) { return g >= 1000 ? csNum(g / 1000, 2) + ' kg' : csNum(g) + ' g'; }
// Blank means "use the default price". NaN means the text is not a price.
function csPrice(text) {
    const t = String(text).trim().replace(',', '.');
    if (t === '') return 0;
    const n = Number(t);
    return Number.isFinite(n) && n >= 0 ? n : NaN;
}
const csPriceText = n => (n ? String(n) : '');

function csMatRow(name, price) {
    return '<div class="cs-mat">'
        + '<input type="text" class="field" data-cs="mat-name" aria-label="Material name" maxlength="24" autocomplete="off" value="' + escHtml(name) + '" placeholder="Material">'
        + '<input type="text" inputmode="decimal" class="field" data-cs="mat-price" aria-label="Price per kg" autocomplete="off" value="' + escHtml(price) + '" placeholder="Default">'
        + '<button type="button" class="cs-x" data-cs-act="remove" aria-label="Remove this material"><i class="fas fa-times"></i></button>'
        + '</div>';
}

function csMount(section) {
    cs.root = section;
    section.innerHTML = `
<h2 class="font-heading text-xl font-medium leading-tight">Costs</h2>
<p class="mt-1.5 text-sm text-text-2">Turns each print's filament and electricity into a cost. Prices stay on this machine.</p>

<div id="csLoadError" class="hidden mt-4 text-sm text-danger flex items-center gap-3 flex-wrap">
  <span>Your prices could not be loaded.</span>
  <button type="button" class="btn-outline btn-xs" data-cs-act="retry">Try again</button>
</div>

<div class="mt-5 cs-grid">
  <div>
    <label for="csCurrency" class="field-label">Currency</label>
    <input id="csCurrency" type="text" class="field" maxlength="8" autocomplete="off" placeholder="$ or EUR">
  </div>
  <div>
    <label for="csFilament" class="field-label">Filament price per kg</label>
    <input id="csFilament" type="text" inputmode="decimal" class="field" autocomplete="off" placeholder="20">
  </div>
  <div>
    <label for="csEnergy" class="field-label">Electricity price per kWh</label>
    <input id="csEnergy" type="text" inputmode="decimal" class="field" autocomplete="off" placeholder="0.30">
  </div>
</div>

<div class="mt-5">
  <div class="label-row">
    <span class="field-label">Price per kg by material</span>
    ${tip('A material you leave blank uses the filament price above. Bambu prints are priced per spool material. Klipper and OctoPrint report no material, so they use the filament price.', 'About material prices')}
  </div>
  <div id="csMaterials"></div>
  <button type="button" class="btn-outline btn-xs" data-cs-act="add"><i class="fas fa-plus"></i>Add a material</button>
</div>

<p id="csError" class="hidden text-xs text-danger mt-4"></p>
<div class="mt-6 flex items-center justify-end gap-3">
  <button type="button" id="csSave" class="btn-primary btn-lg" data-cs-act="save" disabled>Save</button>
</div>

<div class="cs-spend">
  <div class="flex items-center justify-between gap-3">
    <h3 class="font-heading text-lg font-medium leading-tight">Spending</h3>
    <div class="select-wrap cs-days">
      <label for="csDays" class="sr-only">Period</label>
      <select id="csDays" class="field">
        <option value="7">Last 7 days</option>
        <option value="30" selected>Last 30 days</option>
        <option value="90">Last 90 days</option>
        <option value="365">Last year</option>
      </select>
      <i class="fas fa-chevron-down"></i>
    </div>
  </div>
  <div id="csSummary" class="mt-2" aria-live="polite"></div>
  <p class="mt-3 text-xs text-text-2">A print adds to the filament figure when the printer reports its weight or length, and to the energy figure when it has a smart plug with a meter. Anything else counts as 0.</p>
</div>`;

    section.addEventListener('click', e => {
        const b = e.target.closest('[data-cs-act]');
        if (!b) return;
        const act = b.dataset.csAct;
        if (act === 'add') {
            const wrap = section.querySelector('#csMaterials');
            wrap.insertAdjacentHTML('beforeend', csMatRow('', ''));
            wrap.lastElementChild.querySelector('input').focus();
        } else if (act === 'remove') b.closest('.cs-mat').remove();
        else if (act === 'save') csSave();
        else if (act === 'retry') csLoad();
    });
    section.querySelector('#csDays').addEventListener('change', e => { cs.days = Number(e.target.value); csSummary(); });
    csLoad();
}

async function csLoad() {
    const s = cs.root;
    try {
        const c = await apiFetch('/api/costs');
        cs.currency = c.currency || '';
        s.querySelector('#csCurrency').value = cs.currency;
        s.querySelector('#csFilament').value = csPriceText(c.filament_per_kg);
        s.querySelector('#csEnergy').value = csPriceText(c.energy_per_kwh);
        const prices = c.material_per_kg || {};
        const names = csDefaultMaterials.concat(Object.keys(prices).filter(k => !csDefaultMaterials.includes(k)).sort());
        s.querySelector('#csMaterials').innerHTML = names.map(n => csMatRow(n, csPriceText(prices[n]))).join('');
        cs.loaded = true;
        s.querySelector('#csLoadError').classList.add('hidden');
    } catch (_) {
        cs.loaded = false;
        s.querySelector('#csLoadError').classList.remove('hidden');
    }
    // Saving before the prices have loaded would send blanks over the real ones.
    s.querySelector('#csSave').disabled = !cs.loaded;
    csSummary();
}

async function csSave() {
    const s = cs.root, err = s.querySelector('#csError');
    const fail = msg => { err.textContent = msg; err.classList.remove('hidden'); };
    err.classList.add('hidden');
    const filament = csPrice(s.querySelector('#csFilament').value);
    const energy = csPrice(s.querySelector('#csEnergy').value);
    if (Number.isNaN(filament) || Number.isNaN(energy)) return fail('Enter prices as numbers, like 20 or 0.30.');
    const materials = {};
    for (const row of s.querySelectorAll('.cs-mat')) {
        const name = row.querySelector('[data-cs="mat-name"]').value.trim().toUpperCase();
        const text = row.querySelector('[data-cs="mat-price"]').value;
        if (!name && !text.trim()) continue;
        if (!name) return fail('Give each price a material name.');
        const price = csPrice(text);
        if (Number.isNaN(price)) return fail('Enter prices as numbers, like 20 or 0.30.');
        if (name in materials) return fail(name + ' is listed twice.');
        if (text.trim() !== '') materials[name] = price;
    }
    const btn = s.querySelector('#csSave');
    btn.disabled = true;
    try {
        const c = await apiFetch('/api/costs', {
            method: 'POST',
            body: JSON.stringify({ currency: s.querySelector('#csCurrency').value.trim(), filament_per_kg: filament, energy_per_kwh: energy, material_per_kg: materials }),
        });
        cs.currency = c.currency || '';
        toast('Prices saved');
        csSummary();
    } catch (e) {
        fail(friendlyError(e.message));
    }
    btn.disabled = !cs.loaded;
}

async function csSummary() {
    const box = cs.root.querySelector('#csSummary');
    if (!box.children.length) box.innerHTML = '<div class="cs-skel"></div><div class="cs-skel"></div>';
    try {
        const d = await apiFetch('/api/costs/summary?days=' + cs.days);
        cs.currency = d.currency || cs.currency;
        const stat = (v, l) => '<div><span class="cs-v">' + escHtml(v) + '</span><span class="cs-l">' + l + '</span></div>';
        const row = (name, t, first) => '<div class="cs-row"' + (first ? ' style="border-top:0"' : '') + '>'
            + '<p class="text-sm font-medium truncate">' + escHtml(name) + '</p>'
            + '<div class="cs-stats">' + stat(csNum(t.prints), t.prints === 1 ? 'print' : 'prints') + stat(csGrams(t.grams), 'filament')
            + stat(csNum(t.kwh, 2), 'kWh') + stat(csMoney(t.cost), 'cost') + '</div></div>';
        if (!d.printers.length) {
            box.innerHTML = '<p class="text-sm text-text-2">No prints ended in this time.</p>';
            return;
        }
        box.innerHTML = d.printers.map((p, i) => row(p.printer, p, i === 0)).join('')
            + (d.printers.length > 1 ? row('All printers', d.overall, false) : '');
    } catch (e) {
        box.innerHTML = '<p class="text-sm text-danger">The spending could not be loaded: ' + escHtml(friendlyError(e.message)) + '</p>';
    }
}

registerSettingsTab({
    id: 'costs', label: 'Costs', group: 'Printing', order: 40,
    mount: csMount,
    onShow() { if (!cs.loaded) csLoad(); else csSummary(); },
});
