package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A page on another site must not be able to change the Bridge: a text/plain
// POST needs no CORS preflight, so the guard is all that stops it.
func TestGuardWrites_RefusesCrossSiteWrites(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := guardWrites(ok)
	cases := []struct {
		name, method, path, origin, site string
		want                             int
	}{
		{"cross-site config post", "POST", "/api/config", "https://evil.example", "cross-site", 403},
		{"cross-site, old browser", "POST", "/api/config", "https://evil.example", "", 403},
		{"other local app", "POST", "/api/printers", "http://192.168.1.5:3000", "same-site", 403},
		{"FoxTrack may not touch config", "POST", "/api/config", "https://foxtrack.studio", "cross-site", 403},
		{"FoxTrack control", "POST", "/api/control/P1S/pause", "https://foxtrack.studio", "cross-site", 200},
		{"dashboard", "POST", "/api/config", "http://192.168.1.10:8080", "same-origin", 200},
		{"dashboard behind a proxy that rewrites Host", "POST", "/api/config", "https://bridge.example", "same-origin", 200},
		{"dashboard, old browser", "POST", "/api/config", "http://192.168.1.10:8080", "", 200},
		{"curl", "POST", "/api/config", "", "", 200},
		{"cross-site read", "GET", "/api/status", "https://evil.example", "cross-site", 200},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://192.168.1.10:8080"+c.path, strings.NewReader("{}"))
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// A printer name holding "/" or "%" must reach the control handler intact.
func TestPathTail_KeepsSlashAndPercentInNames(t *testing.T) {
	for _, name := range []string{"Voron / Bay 1", "50% PLA", "plain"} {
		r := httptest.NewRequest("POST", "/api/control/"+url.PathEscape(name)+"/pause", nil)
		parts := strings.Split(pathTail(r, "/api/control/"), "/")
		if len(parts) != 2 {
			t.Fatalf("%q: split into %d parts", name, len(parts))
		}
		got, err := url.PathUnescape(parts[0])
		if err != nil || got != name || parts[1] != "pause" {
			t.Fatalf("%q: got %q/%q (%v)", name, got, parts[1], err)
		}
	}
}
