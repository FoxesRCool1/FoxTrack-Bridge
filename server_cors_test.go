package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The FoxTrack web app calls the local bridge first for pause/resume/stop/light
// and falls back to the cloud queue when that fails. With no CORS headers the
// browser rejected every response, so the fast path always "failed" after the
// bridge had already executed the command, and the queued copy then ran it a
// second time. These tests pin the headers that make the fast path honest.

func TestAllowWebOrigin_EchoesFoxTrackOrigin(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/control/X1C/pause", nil)
	req.Header.Set("Origin", "https://foxtrack.studio")
	rec := httptest.NewRecorder()

	allowWebOrigin(rec, req)

	h := rec.Header()
	if got := h.Get("Access-Control-Allow-Origin"); got != "https://foxtrack.studio" {
		t.Errorf("Allow-Origin = %q, want the caller's origin echoed back", got)
	}
	// Chrome refuses a public HTTPS page reaching a loopback address without this.
	if got := h.Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Errorf("Allow-Private-Network = %q, want true", got)
	}
	if got := h.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin so caches do not mix origins", got)
	}
}

// /api/control has no auth of its own. A wildcard would let any page the user
// happens to visit read their printer state and drive their printers.
func TestAllowWebOrigin_RefusesUnknownOrigin(t *testing.T) {
	for _, origin := range []string{
		"https://evil.example",
		"https://foxtrack.studio.evil.example",
		"http://foxtrack.studio",
		"https://notfoxtrack.studio",
	} {
		req := httptest.NewRequest("POST", "/api/control/X1C/pause", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()

		allowWebOrigin(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q was allowed (%q); it must not be", origin, got)
		}
	}
}

// A loopback dev server grants nothing new: any process already on this machine
// can reach the bridge's unauthenticated API directly.
func TestAllowWebOrigin_AllowsLoopbackDevServer(t *testing.T) {
	for _, origin := range []string{"http://localhost:3000", "http://127.0.0.1:5173"} {
		req := httptest.NewRequest("POST", "/api/control/X1C/pause", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()

		allowWebOrigin(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("origin %q: Allow-Origin = %q, want it echoed", origin, got)
		}
	}
}

func TestAllowWebOrigin_NoOriginHeaderIsNotCORS(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/camera/X1C", nil)
	rec := httptest.NewRecorder()

	allowWebOrigin(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("a same-origin request needs no CORS header, got %q", got)
	}
}

// The preflight must answer 200 with the headers and run no command.
func TestHandleControl_PreflightSucceedsWithoutExecuting(t *testing.T) {
	req := httptest.NewRequest("OPTIONS", "/api/control/X1C/stop", nil)
	req.Header.Set("Origin", "https://foxtrack.studio")
	rec := httptest.NewRecorder()

	handleControl(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("preflight status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://foxtrack.studio" {
		t.Errorf("preflight Allow-Origin = %q, want the origin echoed", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("preflight must advertise the allowed methods")
	}
}
