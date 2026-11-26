//go:build headless

package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

// closedHostPort returns a 127.0.0.1 address nothing listens on, so a poller
// started by an edit only gets a refused connection.
func closedHostPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// schemelessFixture is a config as v2.6.6 saved it: HTTP printer addresses
// with no scheme, each with its key or token.
func schemelessFixture(t *testing.T) (*config.Config, map[string]string) {
	t.Helper()
	addr := map[string]string{
		"k": closedHostPort(t), "o": closedHostPort(t), "p": closedHostPort(t), "h": closedHostPort(t),
	}
	return &config.Config{Printers: []config.Printer{
		{ID: "k", Name: "Voron", MoonrakerURL: addr["k"], APIKey: "moon-key"},
		{ID: "o", Name: "Octo", OctoPrintURL: addr["o"], APIKey: "octo-key"},
		{ID: "p", Name: "Prusa", PrusaLinkURL: addr["p"], APIKey: "prusa-key"},
		{ID: "h", Name: "HA", HomeAssistantURL: addr["h"], HomeAssistantToken: "ha-token", HAStateEntity: "sensor.state"},
	}}, addr
}

func useConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	prev := configStore
	configStore = cfg
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		for _, p := range configStore.Printers {
			lanCtrl.RemovePrinter(p.Name)
		}
		configStore = prev
		configMutex.Unlock()
	})
}

// secretOf is the key or token a printer's edit must keep.
func secretOf(p config.Printer) string {
	if p.Kind() == config.KindHomeAssistant {
		return p.HomeAssistantToken
	}
	return p.APIKey
}

// editBody is the dashboard's PUT for a rename: the address in the form given,
// the key left blank.
func editBody(id, name, address string) string {
	switch id {
	case "k":
		return `{"name":"` + name + `","moonraker_url":"` + address + `","api_key":""}`
	case "o":
		return `{"name":"` + name + `","octoprint_url":"` + address + `","api_key":""}`
	case "p":
		return `{"name":"` + name + `","prusalink_url":"` + address + `","api_key":""}`
	}
	return `{"name":"` + name + `","home_assistant_url":"` + address + `","home_assistant_token":"","ha_state_entity":"sensor.state"}`
}

func TestEditKeepsSecretOfSchemelessSavedAddress(t *testing.T) {
	want := map[string]string{"k": "moon-key", "o": "octo-key", "p": "prusa-key", "h": "ha-token"}
	for _, form := range []string{"http", "echo", "name-only"} {
		for _, id := range []string{"k", "o", "p", "h"} {
			t.Run(form+"/"+id, func(t *testing.T) {
				cfg, addr := schemelessFixture(t)
				useConfig(t, cfg)
				var body string
				switch form {
				case "http": // the current dashboard adds http://
					body = editBody(id, "Renamed", "http://"+addr[id])
				case "echo": // the v2.6.6 dashboard sent the stored value back
					body = editBody(id, "Renamed", addr[id])
				default:
					body = `{"name":"Renamed"}`
				}
				rec := httptest.NewRecorder()
				handlePrinterByName(rec, httptest.NewRequest("PUT", "/api/printers/"+id, strings.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), enterKeyAgain) {
					t.Fatalf("edit refused: %s", rec.Body.String())
				}
				configMutex.RLock()
				var got config.Printer
				for _, p := range configStore.Printers {
					if p.ID == id {
						got = p
					}
				}
				configMutex.RUnlock()
				if got.Name != "Renamed" || secretOf(got) != want[id] {
					t.Fatalf("saved printer = %+v, want name Renamed and secret %q", got, want[id])
				}
				onDisk, err := config.LoadConfig()
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range onDisk.Printers {
					if p.ID == id && secretOf(p) != want[id] {
						t.Fatalf("config.json secret = %q, want %q", secretOf(p), want[id])
					}
				}
			})
		}
	}
}

func TestEditOfSchemelessAddressToNewHostStillDropsSecret(t *testing.T) {
	cfg, _ := schemelessFixture(t)
	other := "http://" + closedHostPort(t)
	for _, id := range []string{"o", "p", "h"} {
		if _, _, err := applyPrinterEdit(cfg, id, decodeEdit(t, editBody(id, "Moved", other)), notBusy); !errors.Is(err, errInvalidPrinterEdit) || err.Error() != enterKeyAgain {
			t.Fatalf("%s: err = %v, want %q", id, err, enterKeyAgain)
		}
	}
	// Klipper's key is optional: the edit is saved without the old key.
	got, idx, err := applyPrinterEdit(cfg, "k", decodeEdit(t, editBody("k", "Moved", other)), notBusy)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Printers[idx]; p.APIKey != "" || p.MoonrakerURL != other {
		t.Fatalf("printer = %+v, want the new address and no key", p)
	}
	// Another scheme is another endpoint too.
	https := "https://" + strings.TrimPrefix(cfg.Printers[1].OctoPrintURL, "http://")
	if _, _, err := applyPrinterEdit(cfg, "o", decodeEdit(t, editBody("o", "Octo", https)), notBusy); err == nil || err.Error() != enterKeyAgain {
		t.Fatalf("https edit: err = %v, want %q", err, enterKeyAgain)
	}
	if cfg.Printers[0].APIKey != "moon-key" || cfg.Printers[0].MoonrakerURL == other {
		t.Fatalf("the stored config was changed: %+v", cfg.Printers[0])
	}
}

func decodeEdit(t *testing.T, body string) printerEdit {
	t.Helper()
	var e printerEdit
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestProbeUsesSavedKeyOfSchemelessSavedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "saved-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"result":{"state":"ready","hostname":"voron"}}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	saved := &config.Config{Printers: []config.Printer{{ID: "abc", Name: "voron", MoonrakerURL: host, APIKey: "saved-key"}}}
	useConfig(t, saved)

	wantProbe(t, runProbe(t, `{"id":"abc","moonraker_url":"`+srv.URL+`","api_key":""}`), probeOK, true)
	wantProbe(t, runProbe(t, `{"id":"abc","moonraker_url":"`+host+`","api_key":""}`), probeOK, true)
	// Another host still gets no saved key.
	other := httptest.NewServer(srv.Config.Handler)
	defer other.Close()
	wantProbe(t, runProbe(t, `{"id":"abc","moonraker_url":"`+other.URL+`","api_key":""}`), probeAuth, false)
	if got := saved.Printers[0].MoonrakerURL; got != host {
		t.Fatalf("a test rewrote the saved address to %q", got)
	}
}

func TestRefuseClearPrintersHasHint(t *testing.T) {
	check := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["error"] != "This would remove every saved printer, so it was blocked. Your printers were kept." {
			t.Fatalf("error = %q", body["error"])
		}
		if !strings.Contains(body["hint"], `"confirm_clear_printers": true`) || !strings.Contains(body["hint"], "?confirm_clear_printers=true") {
			t.Fatalf("hint = %q", body["hint"])
		}
	}
	t.Run("config", func(t *testing.T) {
		cfg, _ := schemelessFixture(t)
		useConfig(t, cfg)
		rec := httptest.NewRecorder()
		handleConfig(rec, httptest.NewRequest("POST", "/api/config", strings.NewReader(`{"printers":[]}`)))
		check(t, rec)
	})
	t.Run("import", func(t *testing.T) {
		cfg, _ := schemelessFixture(t)
		useConfig(t, cfg)
		req := httptest.NewRequest("POST", "/api/config/import", strings.NewReader(`{"printers":[]}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handleConfigImport(rec, req)
		check(t, rec)
	})
	// Other errors keep the one-key body.
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusBadRequest, "invalid json")
	if strings.TrimSpace(rec.Body.String()) != `{"error":"invalid json"}` {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
