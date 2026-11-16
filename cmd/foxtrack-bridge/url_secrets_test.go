//go:build headless

package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

// A webhook URL and a login inside a printer address are secrets: they stay out
// of the export, and a restore on the same install brings them back.
func TestExportHidesURLSecrets_RestoreKeepsThem(t *testing.T) {
	old := &config.Config{
		Printers: []config.Printer{{ID: "a", Name: "K", MoonrakerURL: "http://k.local", APIKey: "kept-key", WebcamURL: "rtsp://u:SECRET@cam.local/live"}},
		Notifications: &config.Notifications{Channels: []config.NotifyChannel{
			{ID: "d", Kind: "discord", URL: "https://discord.com/api/webhooks/SECRET"},
			{ID: "g", Kind: "gotify", URL: "https://gotify.local", Token: "SECRET-token"},
		}},
	}
	inner, err := json.Marshal(exportableConfig(old))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inner), "SECRET") {
		t.Fatalf("export holds a secret: %s", inner)
	}
	if !strings.Contains(string(inner), "https://gotify.local") || !strings.Contains(string(inner), "cam.local/live") {
		t.Fatalf("export lost an address that is not secret: %s", inner)
	}

	got, err := resolveConfigImport(old, inner, false)
	if err != nil {
		t.Fatal(err)
	}
	if u := got.Printers[0].WebcamURL; u != old.Printers[0].WebcamURL {
		t.Errorf("webcam url = %q", u)
	}
	if k := got.Printers[0].APIKey; k != "kept-key" {
		t.Errorf("printer key = %q", k)
	}
	ch := got.Notifications.Channels
	if ch[0].URL != old.Notifications.Channels[0].URL || ch[1].Token != "SECRET-token" {
		t.Errorf("channels = %+v", ch)
	}
}

// A login is only put back on the address it came from.
func TestRestoreDoesNotMoveALoginToANewAddress(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{{ID: "a", Name: "K", MoonrakerURL: "http://k.local", WebcamURL: "rtsp://u:p@cam.local/live"}}}
	body, _ := json.Marshal(&config.Config{Printers: []config.Printer{{ID: "a", Name: "K", MoonrakerURL: "http://k.local", WebcamURL: "rtsp://other.local/live"}}})
	got, err := resolveConfigImport(old, body, false)
	if err != nil {
		t.Fatal(err)
	}
	if u := got.Printers[0].WebcamURL; u != "rtsp://other.local/live" {
		t.Errorf("webcam url = %q", u)
	}
}

// Behind a reverse proxy on the same computer every visitor connects from
// loopback, so a forwarding header means the request is not local.
func TestLoopbackClient_RefusesForwardedRequests(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/config/export", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	if !loopbackClient(r) {
		t.Fatal("a direct local request must count as local")
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if loopbackClient(r) {
		t.Fatal("a forwarded request must not count as local")
	}
}
