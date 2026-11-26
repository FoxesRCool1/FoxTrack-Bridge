//go:build headless

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

// postPrinter sends one POST /api/printers.
func postPrinter(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handlePrinters(rec, httptest.NewRequest(http.MethodPost, "/api/printers", strings.NewReader(body)))
	return rec
}

func errorText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body.Error
}

// forgetLAN stops any poller a test started for these names.
func forgetLAN(t *testing.T, names ...string) {
	t.Cleanup(func() {
		for _, n := range names {
			lanCtrl.RemovePrinter(n)
		}
	})
}

// Adding a machine that is already saved, under any name, is refused: it would
// open a second connection to the same printer.
func TestCreateRefusesSamePrinterTwice(t *testing.T) {
	withBridgeConfig(t, &config.Config{Printers: []config.Printer{
		{ID: "b1", Name: "X1C", IP: "192.0.2.10", Serial: "00M09A350100001", LANCode: "12345678"},
		{ID: "k1", Name: "Voron", MoonrakerURL: "http://192.0.2.30:7125"},
		{ID: "o1", Name: "Octo", OctoPrintURL: "http://192.0.2.40", APIKey: "ok"},
		{ID: "p1", Name: "MK4", PrusaLinkURL: "http://192.0.2.50", APIKey: "pk"},
		{ID: "h1", Name: "HA one", HomeAssistantURL: "http://127.0.0.1:1", HomeAssistantToken: "t", HAStateEntity: "sensor.one"},
	}})
	for name, c := range map[string]struct{ body, owner string }{
		"bambu same serial":    {`{"name":"Other","ip":"192.0.2.11","serial":"00m09a350100001","lan_code":"87654321"}`, "X1C"},
		"klipper no scheme":    {`{"name":"Other","moonraker_url":"192.0.2.30:7125/"}`, "Voron"},
		"klipper other case":   {`{"name":"Other","moonraker_url":"HTTP://192.0.2.30:7125"}`, "Voron"},
		"octoprint same url":   {`{"name":"Other","octoprint_url":"http://192.0.2.40/","api_key":"x"}`, "Octo"},
		"prusalink same url":   {`{"name":"Other","prusalink_url":"192.0.2.50","api_key":"x"}`, "MK4"},
		"home assistant equal": {`{"name":"Other","home_assistant_url":"http://127.0.0.1:1/","home_assistant_token":"t","ha_state_entity":"sensor.one"}`, "HA one"},
	} {
		rec := postPrinter(t, c.body)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: status %d, want 409: %s", name, rec.Code, rec.Body)
			continue
		}
		if got, want := errorText(t, rec), `Already added as "`+c.owner+`".`; got != want {
			t.Errorf("%s: error %q, want %q", name, got, want)
		}
	}
	if n := len(liveConfig().Printers); n != 5 {
		t.Fatalf("printers = %d after refused adds, want 5", n)
	}

	// Many Home Assistant printers share one server; other entities are another printer.
	forgetLAN(t, "HA two")
	rec := postPrinter(t, `{"name":"HA two","home_assistant_url":"http://127.0.0.1:1","home_assistant_token":"t","ha_state_entity":"sensor.two"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second Home Assistant printer on one server: status %d: %s", rec.Code, rec.Body)
	}
	// A Klipper on another port is another printer.
	forgetLAN(t, "Second Voron")
	if rec := postPrinter(t, `{"name":"Second Voron","moonraker_url":"http://127.0.0.1:1"}`); rec.Code != http.StatusOK {
		t.Fatalf("other Moonraker: status %d: %s", rec.Code, rec.Body)
	}
}

// A Bambu printer is the same machine on LAN and through the cloud.
func TestSamePrinterIdentity(t *testing.T) {
	lan := config.Printer{Name: "A", IP: "192.0.2.1", Serial: "SER1", LANCode: "x"}
	cloud := config.Printer{Name: "B", Serial: "ser1", Connection: config.ConnectionCloud}
	if !samePrinterIdentity(lan, cloud) {
		t.Error("LAN and cloud entries with one serial are not the same printer")
	}
	if samePrinterIdentity(config.Printer{Name: "A"}, config.Printer{Name: "B"}) {
		t.Error("two Bambu entries without a serial count as the same printer")
	}
	k := config.Printer{MoonrakerURL: "http://192.0.2.5:7125"}
	o := config.Printer{OctoPrintURL: "http://192.0.2.5:7125"}
	if samePrinterIdentity(k, o) {
		t.Error("a Klipper and an OctoPrint on one address count as the same printer")
	}
}

// Edits refuse only a change that makes this printer another saved one.
func TestEditRefusesIdentityCollision(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{
		{ID: "a", Name: "A", IP: "192.0.2.1", Serial: "SA", LANCode: "11111111"},
		{ID: "b", Name: "B", IP: "192.0.2.2", Serial: "SB", LANCode: "22222222"},
		{ID: "c", Name: "C", IP: "192.0.2.3", Serial: "SB", LANCode: "33333333"}, // an older pair
		{ID: "k", Name: "K", MoonrakerURL: "http://192.0.2.30:7125"},
		{ID: "k2", Name: "K2", MoonrakerURL: "http://192.0.2.31:7125"},
	}}
	str := func(s string) *string { return &s }
	_, _, err := applyPrinterEdit(old, "a", printerEdit{Serial: str("SB"), LANCode: str("44444444")}, nil)
	if err == nil || err.Error() != `Already added as "B".` {
		t.Errorf("serial changed onto B: err = %v", err)
	}
	_, _, err = applyPrinterEdit(old, "k2", printerEdit{MoonrakerURL: str("192.0.2.30:7125")}, nil)
	if err == nil || err.Error() != `Already added as "K".` {
		t.Errorf("address changed onto K: err = %v", err)
	}
	// One printer of an existing pair can still be renamed and re-addressed.
	cfg, idx, err := applyPrinterEdit(old, "c", printerEdit{Name: str("C garage"), IP: str("http://192.0.2.9/")}, nil)
	if err != nil {
		t.Fatalf("edit of an existing pair refused: %v", err)
	}
	if p := cfg.Printers[idx]; p.Name != "C garage" || p.IP != "192.0.2.9" || p.LANCode != "33333333" {
		t.Errorf("edited printer = %+v", p)
	}
	// A typed address without a scheme is saved as a full URL.
	cfg, idx, err = applyPrinterEdit(old, "k2", printerEdit{MoonrakerURL: str("192.0.2.32:7125")}, nil)
	if err != nil || cfg.Printers[idx].MoonrakerURL != "http://192.0.2.32:7125" {
		t.Errorf("moonraker edit: err %v, url %q", err, cfg.Printers[idx].MoonrakerURL)
	}
}

// A printer typed the way the scan list shows it is saved as a full URL.
func TestCreateStoresFullURL(t *testing.T) {
	withBridgeConfig(t, &config.Config{})
	forgetLAN(t, "Bare")
	rec := postPrinter(t, `{"name":"Bare","moonraker_url":"127.0.0.1:1/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := liveConfig().Printers[0].MoonrakerURL; got != "http://127.0.0.1:1/" {
		t.Errorf("stored moonraker_url = %q, want http://127.0.0.1:1/", got)
	}
}

// pairConfigJSON is a config.json written the way SaveConfig writes it, with
// one Bambu printer saved twice (same serial) next to other kinds with secrets.
const pairConfigJSON = `{
  "api_key": "ft-legacy",
  "foxtrack2_api_key": "ft2-token",
  "printers": [
    {
      "id": "p-one",
      "name": "X1C",
      "ip": "192.0.2.10",
      "serial": "00M09A350100001",
      "lan_code": "12345678"
    },
    {
      "id": "p-two",
      "name": "X1C (2)",
      "ip": "192.0.2.10",
      "serial": "00M09A350100001",
      "lan_code": "12345678"
    },
    {
      "id": "p-three",
      "name": "P1S",
      "ip": "192.0.2.11",
      "serial": "01P00A000000002",
      "lan_code": "87654321"
    }
  ]
}`

// An existing config that already holds the same printer twice keeps loading,
// saving through Settings and restoring from a backup, with both entries.
func TestExistingSameSerialPairIsKept(t *testing.T) {
	isolateConfigDir(t)
	dir, _ := os.UserConfigDir()
	path := filepath.Join(dir, "FoxTrack-Bridge", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(pairConfigJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantKept := func(stage string, c *config.Config) {
		t.Helper()
		if len(c.Printers) != 3 {
			t.Fatalf("%s: %d printers, want 3", stage, len(c.Printers))
		}
		for i, id := range []string{"p-one", "p-two", "p-three"} {
			if p := c.Printers[i]; p.ID != id || p.LANCode == "" || p.Serial == "" {
				t.Errorf("%s: printer %d = %+v, want %s with its serial and code", stage, i, p, id)
			}
		}
		if c.FoxTrack2APIKey != "ft2-token" || c.APIKey != "ft-legacy" {
			t.Errorf("%s: FoxTrack tokens lost", stage)
		}
	}
	wantKept("load", loaded)

	configMutex.Lock()
	prev := configStore
	configStore = loaded
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = prev
		configMutex.Unlock()
	})

	// Settings echo the list the dashboard got from GET /api/config.
	get := httptest.NewRecorder()
	handleConfig(get, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	post := httptest.NewRecorder()
	handleConfig(post, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(get.Body.String())))
	if post.Code != http.StatusOK {
		t.Fatalf("POST /api/config echo: status %d: %s", post.Code, post.Body)
	}
	wantKept("settings save", liveConfig())
	reloaded, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantKept("saved file", reloaded)

	// Restoring the old file as a backup keeps both entries too.
	req := httptest.NewRequest(http.MethodPost, "/api/config/import", strings.NewReader(pairConfigJSON))
	req.Header.Set("Content-Type", "application/json")
	imp := httptest.NewRecorder()
	handleConfigImport(imp, req)
	if imp.Code != http.StatusOK {
		t.Fatalf("import: status %d: %s", imp.Code, imp.Body)
	}
	wantKept("import", liveConfig())
	if reloaded, err = config.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	wantKept("imported file", reloaded)
}
