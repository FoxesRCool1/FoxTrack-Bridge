//go:build headless

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
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

func TestShareLinks_SaveFailureRollsLinkBack(t *testing.T) {
	withShareConfig(t)
	blockConfigFile(t)

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(`{"printer_id":"p1","camera":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handleShares(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), sharePrefix) {
		t.Fatalf("token returned after a failed save: %s", rec.Body.String())
	}
	configMutex.RLock()
	var n int
	if configStore.Security != nil {
		n = len(configStore.Security.Shares)
	}
	configMutex.RUnlock()
	if n != 0 {
		t.Fatalf("shares left in memory after a failed save: %d", n)
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

func TestShareStatus_HidesFileName(t *testing.T) {
	withShareConfig(t)
	mqttpkg.UpdatePrinterState("Monsieur", mqttpkg.TelemetryData{Status: "printing", FileName: "secret-part.3mf"})
	b, _ := json.Marshal(shareStatusFor(config.ShareLink{Name: "x"}, config.Printer{ID: "p1", Name: "Monsieur"}))
	if strings.Contains(string(b), "file_name") || strings.Contains(string(b), "secret-part") {
		t.Fatalf("status leaks the file name: %s", b)
	}
}

// withShareStills resets the still cache and swaps in a fake snapshot.
func withShareStills(t *testing.T, snap func(context.Context, config.Printer) ([]byte, error)) {
	t.Helper()
	shareStills.Lock()
	shareStills.byPrinter = nil
	shareStills.Unlock()
	prev := shareSnapshot
	shareSnapshot = snap
	t.Cleanup(func() {
		shareSnapshot = prev
		shareStills.Lock()
		shareStills.byPrinter = nil
		shareStills.Unlock()
	})
}

func TestShareCameraStill_SlowCameraDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	withShareStills(t, func(ctx context.Context, p config.Printer) ([]byte, error) {
		if p.ID == "a" {
			<-release
		}
		return []byte(p.ID), nil
	})
	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		shareCameraStill(context.Background(), config.Printer{ID: "a"})
	}()
	bDone := make(chan error, 1)
	go func() {
		_, err := shareCameraStill(context.Background(), config.Printer{ID: "b"})
		bDone <- err
	}()
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("printer b: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("printer b waited for printer a's camera")
	}
	close(release)
	<-aDone
}

func TestShareCameraStill_FailureCachedBriefly(t *testing.T) {
	calls := 0
	withShareStills(t, func(ctx context.Context, p config.Printer) ([]byte, error) {
		calls++
		return nil, errors.New("camera offline")
	})
	p := config.Printer{ID: "p1"}
	for i := 0; i < 2; i++ {
		if _, err := shareCameraStill(context.Background(), p); err == nil {
			t.Fatal("want the camera error")
		}
	}
	if calls != 1 {
		t.Fatalf("snapshot called %d times, want 1 within the failure TTL", calls)
	}
}
