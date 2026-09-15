//go:build headless

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestAssetsEmbedded(t *testing.T) {
	if len(tailwindCSS) == 0 {
		t.Error("tailwindCSS is empty — web/tailwind.css not embedded")
	}
	if len(iconsCSS) == 0 {
		t.Error("iconsCSS is empty — web/icons.css not embedded")
	}
	if len(fontsCSS) == 0 {
		t.Error("fontsCSS is empty — web/fonts.css not embedded")
	}
}

func TestCSSHandlerServesCSS(t *testing.T) {
	cases := map[string][]byte{
		"/tailwind.css": tailwindCSS,
		"/icons.css":    iconsCSS,
		"/fonts.css":    fontsCSS,
	}
	for path, body := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		cssHandler(body)(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
			t.Errorf("%s: Content-Type = %q, want text/css…", path, ct)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}
}

func TestNoCDNReferences(t *testing.T) {
	banned := []string{
		"cdn.tailwindcss.com",
		"fonts.googleapis.com",
		"fonts.gstatic.com",
		"cdnjs.cloudflare.com",
	}
	for _, b := range banned {
		if bytes.Contains(uiHTML, []byte(b)) {
			t.Errorf("ui.html still references CDN host %q", b)
		}
	}
}

func TestUIReferencesLocalAssets(t *testing.T) {
	for _, want := range []string{`href="/tailwind.css"`, `href="/icons.css"`, `href="/fonts.css"`} {
		if !bytes.Contains(uiHTML, []byte(want)) {
			t.Errorf("ui.html missing local asset link %q", want)
		}
	}
}

func TestFontsEmbedded(t *testing.T) {
	data, err := fontsFS.ReadFile("web/fonts/inter-latin.woff2")
	if err != nil {
		t.Fatalf("embedded font web/fonts/inter-latin.woff2 missing: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("wOF2")) {
		t.Error("inter-latin.woff2 is not a woff2 file (bad magic)")
	}
	if len(data) < 1000 {
		t.Errorf("inter-latin.woff2 suspiciously small: %d bytes", len(data))
	}
}

func TestFontHandlerServesWoff2(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fonts/inter-latin.woff2", nil)
	fontHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "font/woff2" {
		t.Errorf("Content-Type = %q, want font/woff2", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("empty body")
	}
}

func TestFontHandlerRejectsUnknown(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fonts/nope.woff2", nil)
	fontHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAllUsedIconsDefined(t *testing.T) {
	re := regexp.MustCompile(`fa-[a-z0-9-]+`)
	ignore := map[string]bool{"fa-spin": true} // animation utility, not a glyph
	seen := map[string]bool{}
	for _, m := range re.FindAll(uiHTML, -1) {
		icon := string(m)
		if ignore[icon] || seen[icon] {
			continue
		}
		seen[icon] = true
		if !bytes.Contains(iconsCSS, []byte("."+icon)) {
			t.Errorf("icon %q used in ui.html but not defined in icons.css", icon)
		}
	}
}

func TestHeadingFontEmbedded(t *testing.T) {
	data, err := fontsFS.ReadFile("web/fonts/cormorant-garamond-latin-500.woff2")
	if err != nil {
		t.Fatalf("embedded font web/fonts/cormorant-garamond-latin-500.woff2 missing: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("wOF2")) {
		t.Error("cormorant-garamond-latin-500.woff2 is not a woff2 file (bad magic)")
	}
	if !bytes.Contains(fontsCSS, []byte("/fonts/cormorant-garamond-latin-500.woff2")) {
		t.Error("fonts.css does not reference the heading font")
	}
}

func TestLogoVariantsEmbedded(t *testing.T) {
	pngMagic := []byte("\x89PNG")
	for name, body := range map[string][]byte{"logo-light.png": logoLightPNG, "logo-dark.png": logoDarkPNG} {
		if !bytes.HasPrefix(body, pngMagic) {
			t.Errorf("%s: not a PNG (bad magic) or empty", name)
		}
	}
}

func TestPNGHandlerServesPNG(t *testing.T) {
	for _, body := range [][]byte{logoLightPNG, logoDarkPNG} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/logo-light.png", nil)
		pngHandler(body)(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("Content-Type = %q, want image/png", ct)
		}
		if rec.Body.Len() == 0 {
			t.Error("empty body")
		}
	}
}

func TestUIReferencesThemeLogos(t *testing.T) {
	for _, want := range []string{`src="/logo-light.png"`, `src="/logo-dark.png"`, `data-theme="paper"`, `foxtrack.theme`} {
		if !bytes.Contains(uiHTML, []byte(want)) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}

// The dashboard follows the FoxTrack design tokens (bg-panel, text-text-2,
// border-border, bg-brand …). A Tailwind palette colour or a white/black
// utility would not follow the theme, so none may creep back in.
func TestUIUsesNoPaletteColors(t *testing.T) {
	re := regexp.MustCompile(`\b(?:bg|text|border|ring|from|to|divide)-(?:zinc|gray|slate|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|white|black)(?:-\d+)?(?:/\d+)?\b`)
	if m := re.FindAll(uiHTML, -1); len(m) > 0 {
		seen := map[string]bool{}
		for _, b := range m {
			seen[string(b)] = true
		}
		t.Errorf("ui.html uses Tailwind palette colours instead of design tokens: %v", seen)
	}
}
