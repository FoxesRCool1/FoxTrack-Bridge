package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// A Bambu printer that answered a command with "mqtt message verify failed" gets
// the plain sentence from /api/control; once a command works it is tried again.
func TestHandleControl_BlockedBambuPrinter(t *testing.T) {
	const name = "Blocked-control-test"
	printers := []config.Printer{{ID: "p-blocked", Name: name, Serial: "01P00BLOCKED", IP: "192.0.2.10", LANCode: "12345678"}}
	configMutex.Lock()
	oldCfg := configStore
	configStore = &config.Config{Printers: printers}
	configMutex.Unlock()
	const ok = `{"print":{"command":"gcode_line","result":"success","reason":"success"}}`
	t.Cleanup(func() {
		mqttpkg.NoteControlReply(name, []byte(ok))
		configMutex.Lock()
		configStore = oldCfg
		configMutex.Unlock()
	})

	post := func() (int, string) {
		r := httptest.NewRequest("POST", "/api/control/"+url.PathEscape(name)+"/resume", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		handleControl(w, r)
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(w.Body).Decode(&body)
		return w.Code, body.Error
	}

	mqttpkg.NoteControlReply(name, []byte(`{"print":{"command":"pause","result":"fail","reason":"mqtt message verify failed"}}`))
	code, msg := post()
	if code != 409 || msg != mqttpkg.ControlBlockedMsg {
		t.Fatalf("blocked printer: %d %q", code, msg)
	}
	if !strings.Contains(msg, "LAN Only Mode and Developer Mode") {
		t.Fatalf("the sentence lost its instruction: %q", msg)
	}

	// After a command worked the request goes on to the printer. This test has
	// no MQTT session, so it fails there, but not with the blocked sentence.
	mqttpkg.NoteControlReply(name, []byte(ok))
	if code, msg := post(); code != 400 || msg == mqttpkg.ControlBlockedMsg || msg == "" {
		t.Fatalf("unblocked printer: %d %q", code, msg)
	}
}
