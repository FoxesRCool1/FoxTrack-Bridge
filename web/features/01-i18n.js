// Translations. The dashboard is written in English; this swaps visible text
// (and placeholder, title, aria-label and alt) for the chosen language as it is
// drawn, so no screen code has to know about languages. Dictionaries are
// web/i18n/<lang>.json: { "English text": "translation" }, where "{0}",
// "{1}"... in a key stand for any text (numbers, names) and are put back in
// the same place in the translation. The choice is kept in this browser.
(function () {
    const LANGS = { en: 'English', de: 'Deutsch', es: 'Español', fr: 'Français', it: 'Italiano', nl: 'Nederlands', pt: 'Português' };
    const KEY = 'foxtrack.bridge.lang';
    const ATTRS = ['placeholder', 'title', 'aria-label', 'alt'];
    // Nothing in these is translated, text or attributes. Input and textarea
    // values are never touched, but their placeholder, title and aria-label are.
    const SKIP_ALL = 'script, style, code, pre, kbd, select option[value=""] ~ option, [data-no-i18n], [data-i18n-html], #logOutput, #assistantMessages';
    const SKIP = SKIP_ALL + ', textarea, input';

    function stored() { try { return localStorage.getItem(KEY); } catch (_) { return null; } }
    function pick() {
        const s = stored();
        if (s && LANGS[s]) return s;
        const nav = (navigator.languages || [navigator.language || 'en']).map(l => String(l).slice(0, 2).toLowerCase());
        return nav.find(l => LANGS[l]) || 'en';
    }

    let lang = pick();
    let exact = new Map();   // English -> translation
    let all = new Map();     // every key, {n} ones too, for swapHtml
    let patterns = [];       // [RegExp, translation template]
    const done = new WeakMap(); // node -> the text we last wrote, so our own writes are not re-translated

    const norm = s => s.replace(/\s+/g, ' ').trim();
    const reEsc = s => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

    function load(dict) {
        exact = new Map();
        all = new Map();
        patterns = [];
        for (const [en, tr] of Object.entries(dict || {})) {
            if (!tr) continue;
            all.set(norm(en), tr);
            if (/\{\d\}/.test(en)) {
                const re = new RegExp('^' + reEsc(en).replace(/\\\{(\d)\\\}/g, '(?<p$1>.+?)') + '$');
                patterns.push([re, tr, en.replace(/\{\d\}/g, '').length]);
            } else {
                exact.set(norm(en), tr);
            }
        }
        // The key with the most fixed text is the most specific, so it is tried first.
        patterns.sort((a, b) => b[2] - a[2]);
    }

    function translate(text) {
        const key = norm(text);
        if (!key || !/[A-Za-z]/.test(key)) return null;
        const hit = exact.get(key);
        if (hit) return hit;
        for (const [re, tr] of patterns) {
            const m = re.exec(key);
            if (m) return tr.replace(/\{(\d)\}/g, (_, i) => (m.groups && m.groups['p' + i]) || '');
        }
        return null;
    }

    function swapText(node) {
        const v = node.nodeValue;
        if (done.get(node) === v) return;
        const tr = translate(v);
        if (tr == null) return;
        // Keep the spaces around the words, which layout may depend on.
        const lead = v.match(/^\s*/)[0], trail = v.match(/\s*$/)[0];
        const out = lead + tr + trail;
        done.set(node, out);
        if (out !== v) node.nodeValue = out;
    }

    function swapAttrs(el) {
        for (const a of ATTRS) {
            const v = el.getAttribute(a);
            if (!v) continue;
            const mark = 'data-i18n-' + a;
            if (el.getAttribute(mark) === v) continue;
            const tr = translate(v);
            if (tr == null) continue;
            el.setAttribute(mark, tr);
            el.setAttribute(a, tr);
        }
    }

    // A sentence with an element inside (a <code> snippet): the element is
    // {0} in the key and goes back in place, untranslated.
    function swapHtml(el) {
        if (done.get(el) === el.innerHTML) return;
        const parts = [];
        let key = '';
        el.childNodes.forEach(n => {
            if (n.nodeType === 3) key += n.nodeValue;
            else if (n.nodeType === 1) { key += '{' + parts.length + '}'; parts.push(n.outerHTML); }
        });
        const tr = all.get(norm(key));
        if (!tr) return;
        el.innerHTML = tr.replace(/\{(\d)\}/g, (_, i) => parts[i] || '');
        done.set(el, el.innerHTML);
    }

    function walk(root) {
        if (lang === 'en' || !exact.size) return;
        if (root.nodeType === 1) {
            if (root.matches('[data-i18n-html]')) swapHtml(root);
            root.querySelectorAll('[data-i18n-html]').forEach(swapHtml);
        }
        if (root.nodeType === 3) { if (!root.parentElement || !root.parentElement.closest(SKIP)) swapText(root); return; }
        if (root.nodeType !== 1 || root.closest(SKIP_ALL)) return;
        swapAttrs(root);
        const it = document.createTreeWalker(root, NodeFilter.SHOW_TEXT | NodeFilter.SHOW_ELEMENT, {
            acceptNode: n => (n.nodeType === 1 && n.matches(SKIP_ALL)) ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT,
        });
        for (let n = it.nextNode(); n; n = it.nextNode()) {
            if (n.nodeType === 3) { if (n.parentElement.tagName !== 'TEXTAREA') swapText(n); } else swapAttrs(n);
        }
    }

    let queued = new Set(), scheduled = false;
    const obs = new MutationObserver(muts => {
        for (const m of muts) {
            if (m.type === 'characterData') queued.add(m.target);
            else if (m.type === 'attributes') queued.add(m.target);
            else m.addedNodes.forEach(n => queued.add(n));
        }
        if (!scheduled) {
            scheduled = true;
            // Before the next paint, so the English never flashes.
            queueMicrotask(() => { scheduled = false; const q = queued; queued = new Set(); q.forEach(n => n.isConnected && walk(n)); });
        }
    });

    async function apply(next) {
        lang = next;
        document.documentElement.lang = lang;
        if (lang === 'en') { if (stored() && stored() !== 'en') location.reload(); return; }
        try {
            const res = await fetch('/i18n/' + lang + '.json');
            if (!res.ok) throw new Error('HTTP ' + res.status);
            load(await res.json());
        } catch (e) {
            console.warn('translation not loaded', e);
            return;
        }
        walk(document.body);
        obs.disconnect();
        obs.observe(document.body, { subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ATTRS });
    }

    // The toast and dialog helpers write with textContent, which the observer
    // sees; nothing else to hook.
    window.bridgeLanguage = () => lang;
    // For text that never reaches the page (notifications) or is joined from
    // pieces. Before the dictionary has loaded nothing matches, so it returns s.
    window.bridgeT = s => (lang === 'en' ? s : (translate(s) ?? s));

    // Language picker at the top of Settings, General.
    function addPicker() {
        const general = document.querySelector('[data-settings-tab="general"] section .space-y-5');
        if (!general || document.getElementById('langSelect')) return;
        const row = document.createElement('div');
        row.className = 'flex items-start justify-between gap-4';
        row.innerHTML = '<div class="min-w-0"><p class="text-sm font-medium flex items-center gap-1"><label for="langSelect">Language</label>' + tip('For this browser. Printer and file names stay as they are.', 'About the language') + '</p></div>'
            + '<div class="select-wrap shrink-0"><select id="langSelect" class="field" style="width:auto;min-width:9rem" data-no-i18n>'
            + Object.entries(LANGS).map(([k, v]) => '<option value="' + k + '">' + v + '</option>').join('')
            + '</select><i class="fas fa-chevron-down"></i></div>';
        general.prepend(row);
        const sel = row.querySelector('select');
        sel.value = lang;
        sel.addEventListener('change', () => {
            try { localStorage.setItem(KEY, sel.value); } catch (_) {}
            location.reload(); // text already translated cannot be translated again
        });
    }
    addPicker();
    document.documentElement.lang = lang;
    if (lang !== 'en') apply(lang);
})();
