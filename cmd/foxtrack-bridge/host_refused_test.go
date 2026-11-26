package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An unknown name gets a page a person can act on in a browser, and the JSON
// error everywhere else. The name is escaped: it comes from the request.
func TestHostRefused_PageForBrowsersJSONForAPI(t *testing.T) {
	t.Setenv("FOXTRACK_BRIDGE_ALLOWED_HOSTS", "")
	h := guardWrites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("refused host reached the handler")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "bridge.example.com:8080"
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("page status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("page Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Open the Bridge by its address", "FOXTRACK_BRIDGE_ALLOWED_HOSTS=bridge.example.com", ".ts.net"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Host = "<script>x</script>.example.com"
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "<script>x") {
		t.Error("host name is not escaped on the page")
	}

	req = httptest.NewRequest("GET", "/api/status", nil)
	req.Host = "bridge.example.com"
	req.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("API refusal = %d %q, want 403 JSON", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "does not answer to the name bridge.example.com") {
		t.Errorf("API error = %s", rec.Body.String())
	}
}

// In Docker the container's own addresses are unreachable from the browser, so
// the page names the Docker computer instead of listing them.
func TestHostRefused_DockerNamesTheHostComputer(t *testing.T) {
	t.Setenv("FOXTRACK_BRIDGE_ALLOWED_HOSTS", "")
	prev := hostRefusedInDocker
	t.Cleanup(func() { hostRefusedInDocker = prev })
	page := func() string {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "bridge.example.com:8080"
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		writeHostRefused(rec, req)
		return rec.Body.String()
	}
	const line = "In Docker, use the address of the computer that runs Docker."

	hostRefusedInDocker = func() bool { return true }
	body := page()
	if !strings.Contains(body, line) {
		t.Errorf("Docker page missing %q", line)
	}
	if strings.Contains(body, "this computer's addresses") || strings.Contains(body, `<a href="http://`) {
		t.Error("Docker page still lists the container's addresses")
	}

	hostRefusedInDocker = func() bool { return false }
	if body := page(); strings.Contains(body, line) {
		t.Error("the Docker line shows outside Docker")
	} else if len(lanAddresses()) > 0 && !strings.Contains(body, "this computer's addresses") {
		t.Error("outside Docker the address list is gone")
	}
}
