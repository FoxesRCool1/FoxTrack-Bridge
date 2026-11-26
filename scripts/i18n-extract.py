"""List the dashboard's user-visible English strings for translation.

Usage, from the repo root: python3 scripts/i18n-extract.py . web/i18n/_source.json
then translate the new keys into each web/i18n/<lang>.json and check them with
python3 web/i18n/_check.py. Reads web/ui.html, web/login.html, web/share.html and
web/features/*.js: HTML text, placeholder/title/aria-label, and JS strings, plus
the error sentences in cmd/foxtrack-bridge/*.go, mqtt/files.go, lan/files.go and
notify/send.go (see GO_FILES). Text for the FoxTrack website (bridge_commands.go,
printjob.go) and log lines are not read.
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
    html_text(html_sentences(markup))
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

# Error sentences the Go server sends and the dashboard shows as they are (a
# toast, a field note). Only sentences are taken: a capital first letter, a
# space, an end mark. "method not allowed" and the like are for programs and stay
# out; so do the Assistant's tool replies, which are written for the model, and
# the text bridge_commands.go and printjob.go send to the FoxTrack website. A
# Printf verb becomes a {0} slot, like the pattern keys in the dictionaries.
GO_FILES = ('queue.go', 'security.go', 'skip_routes.go', 'timelapse_routes.go', 'metrics.go',
            'maintenance_routes.go', 'spoolman_routes.go', 'server.go', 'costs_routes.go',
            'notify_routes.go', 'server_ai.go', 'files_commands.go', 'probe.go', 'share.go',
            'config_transfer.go', 'camera_rtsp.go', 'server_wave27.go', 'discover.go',
            'mqttexport_routes.go', 'server_cloud.go')
# Files outside cmd/foxtrack-bridge whose errors the dashboard shows.
GO_PKG_FILES = ('mqtt/files.go', 'lan/files.go')
# Where else, per file, a sentence for the user goes into a variable, a
# constant or a reply field instead of an error call.
GO_EXTRA = {
    'share.go': r'http\.Error\(w, ',
    'discover.go': r'res\.Notes = append\(res\.Notes, ',
    'camera_rtsp.go': r'rtspNeedsFFmpeg = ',
    'server.go': r'bambuCameraNoAnswer = ',
    'server_wave27.go': r'"message": ',
    'mqttexport_routes.go': r'"message": ',
    'spoolman_routes.go': r'msg :?= ',
}
GO_BASE = (r'writeErr\([^"\n]*|\bbad\(|badNotify\(|errors\.New\(|invalidPrinterEdit\(|refused = |gone := '
           r'|enterKeyAgain = |Reason: |writeSkip\(w, false, |"error": |return "(?:bad_token|plan|offline|error)", ')
GO_STRING = r'"((?:[^"\\\n]|\\.)*)"'
go_found = set()


def go_add(t, loose=False):
    """Keep t when it is a sentence. loose: a hint may end on an example
    address, and a notification service answer starts with its name (%s) and has
    no end mark."""
    t = t.replace('\\"', '"')
    start = re.match(r'[A-Z][^ ]* ', t) or (loose and re.match(r'%s [a-z]|could not [a-z]+ %s', t))
    end = re.search(r'[.?!)]$', t) or (loose and len(t.split()) >= 4)
    if not start or not end or t.startswith('Content-Type') or '"' in t:
        return
    n = [0]
    def verb(_):
        n[0] += 1
        return '{%d}' % (n[0] - 1)
    go_found.add(re.sub(r'%[sdv]', verb, t))


for name in GO_FILES + GO_PKG_FILES:
    gsrc = open(os.path.join(ROOT, name if '/' in name else 'cmd/foxtrack-bridge/' + name)).read()
    for m in re.finditer('(?:' + GO_BASE + ('|' + GO_EXTRA[name] if name in GO_EXTRA else '') + ')' + GO_STRING, gsrc):
        go_add(m.group(1))
    if name == 'probe.go':
        # Test-connection answers: probeResultOf(code, message, hint) with a plain
        # or Sprintf message, the hint constants, and the "can't reach" sentence.
        for line in gsrc.split('\n'):
            if re.search(r'probeResultOf\(|probeUnreachableAt\(|[hH]int = "', line):
                for m in re.finditer(GO_STRING, line):
                    go_add(m.group(1), loose=True)

# The notification service answers (send.go: netErr and check) start with the
# service name, a %s. The field checks in Validate are put together with the
# channel name in notify_routes.go and stay out.
send = open(os.path.join(ROOT, 'notify/send.go')).read()
send = send[send.index('func netErr'):send.index('// detail pulls')]
for m in re.finditer(r'Errorf\(' + GO_STRING, send):
    go_add(m.group(1), loose=True)

# Sentences the server puts together from pieces, which no pattern above can
# read. Each is (file, a piece that must still be in it, the sentence), so a
# change in the Go code fails here instead of leaving a stale key behind.
GO_COMPOSED = (
    ('probe.go', 'Can\'t reach %s at %s.', "Can't reach the printer at {0}."),  # what = "the printer"
    ('spoolman_routes.go', '" found."', 'Spoolman {0} found.'),
    ('spoolman_routes.go', "Can't reach Spoolman there. ", "Can't reach Spoolman there. {0}"),
    ('server.go', 'Already added as ', 'Already added as "{0}".'),
    ('server_wave27.go', '"Plug found, "', 'Plug found, on, {0} W now.'),
    ('server_wave27.go', '"Plug found, "', 'Plug found, off, {0} W now.'),
    ('server_wave27.go', 'It has no energy meter, so FoxTrack will not see watts or kWh.',
     'Plug found, on. It has no energy meter, so FoxTrack will not see watts or kWh.'),
    ('server_wave27.go', 'It has no energy meter, so FoxTrack will not see watts or kWh.',
     'Plug found, off. It has no energy meter, so FoxTrack will not see watts or kWh.'),
)
for name, piece, sentence in GO_COMPOSED:
    if piece not in open(os.path.join(ROOT, 'cmd/foxtrack-bridge', name)).read():
        sys.exit('i18n-extract: %s no longer holds %r; update GO_COMPOSED' % (name, piece))
    go_found.add(sentence)

# Drop markup and code fragments that slipped through. A string that holds a
# URL is dropped only when it is nothing but the URL (no spaces); a help
# sentence that mentions an address ("For example http://...") is kept.
JUNK = re.compile(r'^#|:not\(|[<>=\[\]{}]|^["\'.,:;)\-]|^\w+_\w+$|@example|^ftb_|^ftk_|^\S*https?:\S*$')
# A JS string writes an apostrophe as \'; the page shows a plain one.
found = {x.replace("\\'", "'") for x in found}
out = sorted({x for x in found if not JUNK.search(PH.sub('X', x))} | go_found)
json.dump(out, open(OUT, 'w'), indent=0, ensure_ascii=False)
print(len(out), 'strings')
