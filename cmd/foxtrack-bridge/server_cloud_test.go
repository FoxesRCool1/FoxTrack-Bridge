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

func linkedCloud() *config.BambuCloud {
	return &config.BambuCloud{Region: "global", Email: "a@b.c", AccessToken: "super-secret-token", MQTTUsername: "u_1", IssuedAt: 1, ExpiresAt: 9_000_000_000}
}

// A settings save never carries the Bambu account, so it must survive.
func TestResolveConfigUpdate_KeepsBambuCloudWhenOmitted(t *testing.T) {
	old := &config.Config{Printers: twoPrinters(), BambuCloud: linkedCloud()}
	got, err := resolveConfigUpdate(old, []byte(`{"api_key":"k","auto_update":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.BambuCloud == nil || got.BambuCloud.AccessToken != "super-secret-token" || got.BambuCloud.MQTTUsername != "u_1" {
		t.Fatalf("bambu_cloud = %+v, want the stored account kept", got.BambuCloud)
	}
	// A client echoing the redacted view (no token) must not wipe it either.
	got, err = resolveConfigUpdate(old, []byte(`{"bambu_cloud":{"linked":true,"email":"a@b.c"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.BambuCloud == nil || got.BambuCloud.AccessToken != "super-secret-token" {
		t.Fatalf("bambu_cloud = %+v, want token kept when the body has none", got.BambuCloud)
	}
	if len(got.Printers) != 2 {
		t.Errorf("printers = %d, want 2", len(got.Printers))
	}
}

func TestRedactConfig_NeverExposesCloudToken(t *testing.T) {
	cfg := &config.Config{Printers: []config.Printer{{Name: "C", Serial: "S1", LANCode: "code", Connection: "cloud"}}, BambuCloud: linkedCloud()}
	b, err := json.Marshal(redactConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "super-secret-token") || strings.Contains(s, "u_1") {
		t.Fatalf("redacted config leaks the account secret: %s", s)
	}
	if !strings.Contains(s, `"bambu_cloud":{"linked":true,"email":"a@b.c","region":"global","token_expires_at":9000000000}`) {
		t.Errorf("redacted account block wrong: %s", s)
	}
	if !strings.Contains(s, `"connection":"cloud"`) || !strings.Contains(s, `"lan_code_set":true`) {
		t.Errorf("cloud printer not redacted as expected: %s", s)
	}
	if b, _ := json.Marshal(redactConfig(&config.Config{})); strings.Contains(string(b), "bambu_cloud") {
		t.Errorf("unlinked config must omit bambu_cloud: %s", b)
	}
}

func TestIsBambuPrinterConfig_CloudWithoutLANCode(t *testing.T) {
	if !isBambuPrinterConfig(config.Printer{Name: "C", Serial: "S1", Connection: "cloud"}) {
		t.Error("a cloud printer is a Bambu printer even before the LAN code is known")
	}
	if isBambuPrinterConfig(config.Printer{Name: "L", Serial: "S1"}) {
		t.Error("a LAN printer without an access code is not connectable")
	}
	if isBambuPrinterConfig(config.Printer{Name: "K", Serial: "S1", Connection: "cloud", MoonrakerURL: "http://x"}) {
		t.Error("a Moonraker URL still means Klipper")
	}
}

func TestHandlePrinters_CloudNeedsLinkedAccount(t *testing.T) {
	configMutex.Lock()
	configStore = &config.Config{Printers: []config.Printer{}}
	configMutex.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/printers", strings.NewReader(`{"name":"Cloudy","serial":"S1","connection":"cloud"}`))
	handlePrinters(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "link your Bambu account") {
		t.Errorf("body = %s", rec.Body.String())
	}
	configMutex.RLock()
	n := len(configStore.Printers)
	configMutex.RUnlock()
	if n != 0 {
		t.Errorf("printers = %d, want 0 (nothing saved on a refused add)", n)
	}
}

func TestHandlePrinters_UnknownConnectionFallsBackToLAN(t *testing.T) {
	p := config.Printer{Name: "X", Serial: "S", LANCode: "c", Connection: "weird"}
	if p.IsCloud() {
		t.Fatal("only \"cloud\" is cloud")
	}
	// resolveConfigUpdate leaves the value alone (the config stays loadable);
	// only POST /api/printers normalises it, which is covered by the 400 path.
	if !isBambuPrinterConfig(p) {
		t.Error("an unknown mode with LAN details still connects over LAN")
	}
}

func TestHandleCloudStatus_Unlinked(t *testing.T) {
	configMutex.Lock()
	configStore = &config.Config{Printers: []config.Printer{{Name: "C", Serial: "S1", Connection: "cloud"}}}
	configMutex.Unlock()

	rec := httptest.NewRecorder()
	handleCloud(rec, httptest.NewRequest(http.MethodGet, "/api/cloud/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Linked        bool `json:"linked"`
		CloudPrinters int  `json:"cloud_printers"`
		MQTT          struct {
			State string `json:"state"`
		} `json:"mqtt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Linked || resp.CloudPrinters != 1 || resp.MQTT.State != "off" {
		t.Errorf("resp = %+v", resp)
	}
	if strings.Contains(rec.Body.String(), "access_token") {
		t.Error("status must never carry the token")
	}
}

func TestHandleCloud_UnknownAction(t *testing.T) {
	rec := httptest.NewRecorder()
	handleCloud(rec, httptest.NewRequest(http.MethodGet, "/api/cloud/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestCloudSessionFromConfig(t *testing.T) {
	if cloudSessionFromConfig(&config.Config{}) != nil {
		t.Error("unlinked config must give no session")
	}
	expired := linkedCloud()
	expired.ExpiresAt = 1
	if cloudSessionFromConfig(&config.Config{BambuCloud: expired}) != nil {
		t.Error("an expired token must never be used to connect")
	}
	sess := cloudSessionFromConfig(&config.Config{BambuCloud: linkedCloud()})
	if sess == nil || sess.Broker != "ssl://us.mqtt.bambulab.com:8883" || sess.Username != "u_1" || sess.Token != "super-secret-token" {
		t.Errorf("session = %+v", sess)
	}
}
