"""Check every web/i18n/<lang>.json against _source.json: valid JSON, every
key present, no extra keys, no empty values. Run from the repo root."""
import glob, json, sys

src = json.load(open('web/i18n/_source.json'))
files = [f for f in glob.glob('web/i18n/*.json') if not f.endswith(('_source.json', 'en.json'))]
if not files:
    sys.exit('no translation file yet')
bad = False
for f in files:
    try:
        d = json.load(open(f, encoding='utf-8'))
    except Exception as e:
        print(f, 'is not valid JSON:', e)
        bad = True
        continue
    missing = [s for s in src if s not in d]
    # Pattern keys ({0} for any text) are added by hand on top of the source list.
    extra = [k for k in d if k not in src and '{0}' not in k and k != 'to']
    empty = [k for k, v in d.items() if not isinstance(v, str) or not v.strip()]
    if missing or extra or empty:
        bad = True
        print(f, 'missing', len(missing), missing[:5], 'extra', len(extra), extra[:5], 'empty', len(empty), empty[:5])
sys.exit(1 if bad else 0)
