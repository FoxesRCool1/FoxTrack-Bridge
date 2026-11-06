//go:build headless

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

func withShareConfig(t *testing.T) {
	t.Helper()
	configMutex.Lock()
	saved := configStore
	configStore = &config.Config{Printers: []config.Printer{
		{ID: "p1", Name: "Monsieur", IP: "192.168.87.22", LANCode: "81f1aafd"},
	}}
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = saved
		configMutex.Unlock()
	})
}

func createShare(t *testing.T, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleShares(rec, req)
	var resp struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp.Token
}

func TestShareLinks_CreateViewRevoke(t *testing.T) {
	withShareConfig(t)

	if code, _ := createShare(t, `{"printer_id":"nope"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown printer: status %d", code)
	}
	code, tok := createShare(t, `{"printer_id":"p1","camera":false}`)
	if code != 200 || !strings.HasPrefix(tok, sharePrefix) {
		t.Fatalf("create: %d %q", code, tok)
	}

	configMutex.RLock()
	stored := configStore.Security.Shares[0]
	configMutex.RUnlock()
	if stored.Hash == tok || stored.Hash != sha256Hex(tok) {
		t.Fatalf("token must be stored hashed only")
	}
	if stored.Name != "Monsieur" {
		t.Fatalf("default name = %q", stored.Name)
	}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handleSharePage(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	if rec := get("/share/" + tok); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Read-only view") {
		t.Fatalf("page: %d", rec.Code)
	}
	rec := get("/share/" + tok + "/status")
	if rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"192.168", "81f1aafd", "lan_code", "\"ip\""} {
		if strings.Contains(body, leak) {
			t.Fatalf("status leaks %q: %s", leak, body)
		}
	}
	if rec := get("/share/" + tok + "/camera.jpg"); rec.Code != http.StatusNotFound {
		t.Fatalf("camera not shared: status %d", rec.Code)
	}
	for _, bad := range []string{"/share/", "/share/fts_wrong", "/share/" + tok + "x", "/share/" + tok[:len(tok)-1]} {
		if rec := get(bad); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", bad, rec.Code)
		}
	}

	// Revoke: the link stops working at once.
	del := httptest.NewRecorder()
	handleShares(del, httptest.NewRequest("DELETE", "/api/shares/"+stored.ID, nil))
	if del.Code != 200 {
		t.Fatalf("revoke: %d", del.Code)
	}
	if rec := get("/share/" + tok + "/status"); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked link still answers: %d", rec.Code)
	}
}

func TestShareLinks_PrinterRemovedStopsLink(t *testing.T) {
	withShareConfig(t)
	_, tok := createShare(t, `{"printer_id":"p1"}`)
	configMutex.Lock()
	configStore.Printers = nil
	configMutex.Unlock()
	rec := httptest.NewRecorder()
	handleSharePage(rec, httptest.NewRequest("GET", "/share/"+tok+"/status", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404 once the printer is gone", rec.Code)
	}
}

func TestShareLinks_PublicPathOnlyForShare(t *testing.T) {
	if !isPublicPath("/share/fts_x") || !isPublicPath("/i18n/de.json") || isPublicPath("/api/shares") || isPublicPath("/sharex") {
		t.Fatal("only /share/ pages may skip sign-in")
	}
}
