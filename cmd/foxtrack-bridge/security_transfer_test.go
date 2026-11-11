package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"foxtrack-bridge/config"
)

// Backup and restore hold the whole setup. With a password they need the
// sign-in like any request; with none, only this computer may use them. The
// rest of the dashboard keeps its rules (open without a password).
func TestRequireAuth_ConfigTransferPaths(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	h := requireAuth(inner)
	call := func(method, path, remote string, mod func(*http.Request)) int {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = remote
		if mod != nil {
			mod(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	paths := []struct{ method, path string }{{"GET", "/api/config/export"}, {"POST", "/api/config/import"}, {"GET", "/api/config/export/"}}

	// No password: this computer only.
	withSecurityConfig(t, nil)
	for _, c := range paths {
		for _, local := range []string{"127.0.0.1:50000", "[::1]:50000"} {
			if got := call(c.method, c.path, local, nil); got != http.StatusOK {
				t.Errorf("no password, %s from %s: %d, want 200", c.path, local, got)
			}
		}
		if got := call(c.method, c.path, "192.168.1.50:50000", nil); got != http.StatusForbidden {
			t.Errorf("no password, %s from the LAN: %d, want 403", c.path, got)
		}
		// A header the client controls is not an address.
		spoof := func(r *http.Request) { r.Header.Set("X-Forwarded-For", "127.0.0.1") }
		if got := call(c.method, c.path, "192.168.1.50:50000", spoof); got != http.StatusForbidden {
			t.Errorf("no password, %s with a spoofed X-Forwarded-For: %d, want 403", c.path, got)
		}
	}
	// Everything else stays open to the network, as before.
	if got := call("GET", "/api/status", "192.168.1.50:50000", nil); got != http.StatusOK {
		t.Errorf("/api/status from the LAN with no password: %d, want 200", got)
	}

	// A password: the sign-in is needed, from this computer too.
	hash, _ := hashPassword("hunter22")
	tok := tokenPrefix + "abc"
	sec := &config.Security{PasswordHash: hash, SessionKey: randomHex(32),
		Tokens: []config.APIToken{{ID: "t1", Name: "ha", Hash: sha256Hex(tok)}}}
	withSecurityConfig(t, sec)
	for _, c := range paths {
		for _, remote := range []string{"127.0.0.1:50000", "192.168.1.50:50000"} {
			if got := call(c.method, c.path, remote, nil); got != http.StatusUnauthorized {
				t.Errorf("password set, %s from %s signed out: %d, want 401", c.path, remote, got)
			}
		}
		signedIn := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
		if got := call(c.method, c.path, "192.168.1.50:50000", signedIn); got != http.StatusOK {
			t.Errorf("password set, %s signed in: %d, want 200", c.path, got)
		}
	}
}
