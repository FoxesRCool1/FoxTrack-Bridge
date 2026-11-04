# Third-party licenses

FoxTrack Bridge embeds the following third-party assets in its binary.

## Inter (font)

- Source: https://github.com/rsms/inter
- Copyright (c) 2016-2020 The Inter Project Authors.
- License: SIL Open Font License, Version 1.1 (https://openfontlicense.org)
- Usage: a latin subset (variable, weights 400-600), served at `/fonts/inter-latin.woff2`.

## Cormorant Garamond (font)

- Source: https://github.com/CatharsisFonts/Cormorant
- Copyright 2015 The Cormorant Project Authors.
- License: SIL Open Font License, Version 1.1 (https://openfontlicense.org)
- Usage: a latin subset (weight 500), served at `/fonts/cormorant-garamond-latin-500.woff2`. The heading face shared with the FoxTrack web app.

## Lucide (icons)

- Source: https://lucide.dev (lucide v0.575.0)
- Copyright (c) for portions of Lucide are held by Cole Bemis 2013-2022 as part of Feather (MIT). All other copyright (c) for Lucide are held by Lucide Contributors 2022.
- License: ISC License (https://github.com/lucide-icons/lucide/blob/main/LICENSE)
- Usage: only the icons used by `web/ui.html`, re-encoded as inline-SVG CSS masks in `web/icons.css`.

## Bambu Lab error code texts (data)

- Source: Bambu Lab's public error code table, `https://e.bambulab.com/query.php?lang=en`, the table Bambu Studio reads (English, version 202611031644, fetched 2026-11-04). The texts are Bambu Lab's; the table is published at a public address without a licence statement.
- Gap fill: codes that table lacks (more AMS units, newer models) come from the English table in ha-bambulab, `custom_components/bambu_lab/pybambu/hms_error_text/hms_en.json.gz` (https://github.com/greghesp/ha-bambulab). Bambu's text wins where both have a code.
- Copyright (c) 2023 ha-bambulab contributors. License: MIT (https://github.com/greghesp/ha-bambulab/blob/main/LICENSE): permission is granted, free of charge, to use, copy, modify, merge, publish, distribute, sublicense and sell copies, provided the copyright notice and this permission notice are included in all copies or substantial portions. The software is provided "as is", without warranty of any kind.
- Usage: merged into `mqtt/hms_en.json` and embedded in the binary. It gives a printer's HMS and print error codes a plain description on the card and in notifications. The Bridge never contacts Bambu for it; refresh the file with `go run mqtt/hms_gen.go`.
