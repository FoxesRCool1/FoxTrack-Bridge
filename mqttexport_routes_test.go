//go:build headless

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

func setMQTTConfig(t *testing.T, block *config.MQTTExport) {
	t.Helper()
	isolateConfigDir(t)
	configMutex.Lock()
	old := configStore
	configStore = &config.Config{APIKey: "fox", Printers: aiPrinters(), MQTTExport: block}
	configMutex.Unlock()
	t.Cleanup(func() {
		mqttExporter.Apply(config.MQTTExport{}) // stop any session a test started
		configMutex.Lock()
		configStore = old
		configMutex.Unlock()
	})
}

func postMQTT(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleMQTTExport(rec, jsonPost("/api/mqtt-export", body))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func storedMQTT() config.MQTTExport {
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore.MQTTExport == nil {
		return config.MQTTExport{}
	}
	return *configStore.MQTTExport
}

func TestMQTTExport_GetNeverReturnsThePassword(t *testing.T) {
	setMQTTConfig(t, &config.MQTTExport{Broker: "tcp://10.0.0.2:1883", Username: "fox", Password: "hunter2-secret"})
	rec := httptest.NewRecorder()
	handleMQTTExport(rec, httptest.NewRequest("GET", "/api/mqtt-export", nil))
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("the password reached the response: %s", rec.Body.String())
	}
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v["password_set"] != true || v["topic_prefix"] != "foxtrack_bridge" || v["discovery_prefix"] != "homeassistant" {
		t.Errorf("view = %v", v)
	}
}

func TestMQTTExport_PostPasswordRules(t *testing.T) {
	setMQTTConfig(t, &config.MQTTExport{Broker: "tcp://127.0.0.1:1", Username: "fox", Password: "old-secret"})

	// Blank keeps it, and the printers survive.
	rec, _ := postMQTT(t, `{"broker":"tcp://127.0.0.1:1","username":"fox","password":"","topic_prefix":"home/p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := storedMQTT(); got.Password != "old-secret" || got.Username != "fox" || got.TopicPrefix != "home/p" {
		t.Errorf("after a blank password: %+v", got)
	}
	// A new username (or broker) without a new password must not reuse the
	// old one: it was saved for the old account.
	postMQTT(t, `{"username":"fox2","password":""}`)
	if got := storedMQTT(); got.Password != "" || got.Username != "fox2" {
		t.Errorf("a changed username kept the password: %+v", got)
	}
	postMQTT(t, `{"password":"old-secret"}`)
	configMutex.RLock()
	n, key := len(configStore.Printers), configStore.APIKey
	configMutex.RUnlock()
	if n != 2 || key != "fox" {
		t.Errorf("printers = %d, api key = %q: a save must not touch them", n, key)
	}

	// A typed one replaces it; the response never carries it.
	rec, _ = postMQTT(t, `{"password":"new-secret"}`)
	if got := storedMQTT(); got.Password != "new-secret" || got.Username != "fox2" {
		t.Errorf("after a new password: %+v (omitted fields must keep their values)", got)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("the password reached the response: %s", rec.Body.String())
	}

	// Clearing needs the flag.
	postMQTT(t, `{"password":"","clear_password":true}`)
	if got := storedMQTT(); got.Password != "" {
		t.Errorf("password = %q after clear_password", got.Password)
	}
}

func TestMQTTExport_PostRefusesABadBroker(t *testing.T) {
	setMQTTConfig(t, &config.MQTTExport{Broker: "tcp://10.0.0.2:1883", Username: "fox", Password: "p"})
	for _, body := range []string{`{"enabled":true,"broker":""}`, `{"enabled":true,"broker":"http://x"}`, `{"topic_prefix":"a/#"}`} {
		rec, _ := postMQTT(t, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400", body, rec.Code)
		}
	}
	if got := storedMQTT(); got.Broker != "tcp://10.0.0.2:1883" {
		t.Errorf("a refused save changed the stored settings: %+v", got)
	}
	rec := httptest.NewRecorder()
	handleMQTTExport(rec, httptest.NewRequest("POST", "/api/mqtt-export", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a POST without a JSON type -> %d, want 415", rec.Code)
	}
}

func TestMQTTExport_EnablingGivesAStableBridgeID(t *testing.T) {
	setMQTTConfig(t, nil)
	postMQTT(t, `{"enabled":true,"broker":"127.0.0.1:1","discovery":true,"bridge_id":"chosen-by-the-client"}`)
	first := storedMQTT()
	if len(first.BridgeID) != 8 || first.BridgeID == "chosen-by-the-client" || first.Broker != "tcp://127.0.0.1:1" || !first.Discovery {
		t.Fatalf("stored = %+v", first)
	}
	postMQTT(t, `{"enabled":true,"broker":"127.0.0.1:1","topic_prefix":"x"}`)
	if got := storedMQTT(); got.BridgeID != first.BridgeID {
		t.Errorf("bridge ID changed from %q to %q", first.BridgeID, got.BridgeID)
	}
}

func TestMQTTExport_TestEndpointSaysWhy(t *testing.T) {
	setMQTTConfig(t, &config.MQTTExport{Password: "stored-pass"})
	// A broker that turns everyone away: CONNACK with "bad user name or password".
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // paho retries with an older protocol version, so keep accepting
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			c.Read(buf)
			c.Write([]byte{0x20, 0x02, 0x00, 0x04})
			c.Close()
		}
	}()
	rec := httptest.NewRecorder()
	handleMQTTExportTest(rec, jsonPost("/api/mqtt-export/test", `{"broker":"`+ln.Addr().String()+`","username":"fox"}`))
	var out struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.OK || out.Message != "The broker refused the username or password." {
		t.Errorf("got %d %+v", rec.Code, out)
	}

	rec = httptest.NewRecorder()
	handleMQTTExportTest(rec, jsonPost("/api/mqtt-export/test", `{"broker":""}`))
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK || !strings.Contains(out.Message, "Enter the broker address") {
		t.Errorf("an empty broker: %+v", out)
	}
	if got := storedMQTT(); got.Broker != "" || got.Enabled {
		t.Errorf("a test must not save anything: %+v", got)
	}
}
