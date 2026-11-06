"""List the dashboard's user-visible English strings for translation.

Usage, from the repo root: python3 scripts/i18n-extract.py . web/i18n/_source.json
then translate the new keys into each web/i18n/<lang>.json and check them with
python3 web/i18n/_check.py. Reads web/ui.html, web/login.html, web/share.html and
web/features/*.js: HTML text, placeholder/title/aria-label, and JS strings.
"""
import glob, html.parser, json, os, re, sys

ROOT = sys.argv[1]
OUT = sys.argv[2]
found = set()


class P(html.parser.HTMLParser):
    skip = 0
    def handle_starttag(self, tag, attrs):
        if tag in ('script', 'style', 'code', 'pre', 'kbd'):
            self.skip += 1
        for k, v in attrs:
            if k in ('placeholder', 'title', 'aria-label') and v:
                add(v)
    def handle_endtag(self, tag):
        if tag in ('script', 'style', 'code', 'pre', 'kbd') and self.skip:
            self.skip -= 1
    def handle_data(self, d):
        if not self.skip:
            add(d)


PH = re.compile(r'\{\d\}')  # "{0}": where a name or number goes in a pattern key


def add(s):
    s = re.sub(r'\s+', ' ', html.unescape(s)).strip()
    if len(s) < 2 or len(s) > 400 or not re.search(r'[A-Za-z]{2}', s):
        return
    if re.fullmatch(r'[\w.\-/:#?=&%{}$]+', s) and ' ' not in s and not re.fullmatch(r'[A-Z][a-z]+', s):
        return  # identifiers, URLs, class names
    if re.search(r'[{}]|=>|\bfunction\b|\(\)|\.\w+\(', PH.sub('X', s)):  # X, not '': "({0})" is text, not a call
        return  # code
    found.add(s)


def html_text(s):
    p = P()
    try:
        p.feed(s)
        p.close()
    except Exception:
        pass


CLASSY = re.compile(r'^[a-z0-9:\[\]./%#\s-]+$')  # tailwind class lists, ids

HTML_SENTENCE = re.compile(r'<span data-i18n-html>(.*?)</span>', re.S)


def html_sentences(src):
    """Pull data-i18n-html sentences out as one key each, and drop them from src."""
    def one(m):
        n = [0]
        def slot(_):
            n[0] += 1
            return '{%d}' % (n[0] - 1)
        found.add(re.sub(r'\s+', ' ', html.unescape(re.sub(r'<(\w+)[^>]*>.*?</\1>', slot, m.group(1), flags=re.S))).strip())
        return ' '
    return HTML_SENTENCE.sub(one, src)


def js_strings(src):
    src = re.sub(r'(?m)^\s*//.*$', '', src)  # whole-line comments
    for m in re.finditer(r"'((?:\\.|[^'\\\n])*)'|\"((?:\\.|[^\"\\\n])*)\"|`((?:\\.|[^`\\])*)`", src):
        s = m.group(1) if m.group(1) is not None else m.group(2) if m.group(2) is not None else m.group(3)
        if s is None:
            continue
        for inner in re.findall(r'\$\{([^}]*)\}', s):
            js_strings(inner)  # a tip('...') call inside a template literal
        s = s.replace("\\'", "'").replace('\\"', '"').replace('\\n', ' ')
        if '<' in s and '>' in s:
            # Template pieces like '<p class="x">Text</p>'.
            html_text(re.sub(r'\$\{[^}]*\}', ' ', s))
            continue
        s = re.sub(r'\$\{[^}]*\}', '', s)
        if CLASSY.match(s.strip()):
            continue
        if not re.search(r'[A-Z]|\s', s.strip()):
            continue
        add(s)


def split_page(path):
    """Markup and script bodies of one HTML page, split at every <script>."""
    parts = re.split(r'(<script[^>]*>|</script>)', open(os.path.join(ROOT, path)).read())
    markup, scripts, in_script = [], [], False
    for p in parts:
        if p.startswith('<script'):
            in_script = True
        elif p == '</script>':
            in_script = False
        elif in_script:
            scripts.append(p)
        else:
            markup.append(p)
    return ''.join(markup), scripts


# The dashboard, the sign-in page and the share page. Each is split at every
# <script>: the sign-in page has one in <head>, so cutting at the first one
# used to drop its whole body.
for page in ('web/ui.html', 'web/login.html', 'web/share.html'):
    markup, scripts = split_page(page)
    html_text(markup)
    for s in scripts:
        js_strings(s)
# Event names in the notification settings come from the server
# (notify.Labels) and are translated in the page like any other text.
labels = open(os.path.join(ROOT, 'notify/message.go')).read()
labels = labels[labels.index('var Labels = map'):]
for m in re.finditer(r':\s*"([^"]+)"', labels[:labels.index('\n}')]):
    add(m.group(1))

for f in sorted(glob.glob(os.path.join(ROOT, 'web/features/*.js'))):
    src = html_sentences(open(f).read())
    js_strings(src)
    # Template literals nested inside ${...} defeat the string regex above, so
    # also read the markup text between tags straight from the source.
    for m in re.finditer(r'>([^<>`${}\n]*[A-Za-z][^<>`${}\n]*)(?=<|\$\{)', src):
        t = m.group(1).replace("\\'", "'")
        if not re.search(r"' \+|\+ '|\? '", t):
            add(t)

# Drop markup and code fragments that slipped through.
JUNK = re.compile(r'^#|:not\(|[<>=\[\]{}]|^["\'.,:;)\-]|^\w+_\w+$|@example|ftb_|ftk_|https?:')
out = sorted(x for x in found if not JUNK.search(PH.sub('X', x)))
json.dump(out, open(OUT, 'w'), indent=0, ensure_ascii=False)
print(len(out), 'strings')
