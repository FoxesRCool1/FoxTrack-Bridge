//go:build headless

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxtrack-bridge/config"
	"foxtrack-bridge/webhook"
)

// Settings' "Connected to FoxTrack" line: each FoxTrack answer maps to one
// state, and the token goes only in the Authorization header.
func TestFoxTrackCheck(t *testing.T) {
	status := http.StatusOK
	var gotAuth, gotQuery string
	fox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		w.WriteHeader(status)
		w.Write([]byte(`{"commands":[]}`))
	}))
	defer fox.Close()
	oldURL := webhook.BridgeCommandsURLV2
	webhook.BridgeCommandsURLV2 = fox.URL
	t.Cleanup(func() { webhook.BridgeCommandsURLV2 = oldURL })
	configMutex.Lock()
	oldCfg := configStore
	configStore = &config.Config{}
	configMutex.Unlock()
	t.Cleanup(func() { configMutex.Lock(); configStore = oldCfg; configMutex.Unlock() })

	check := func() string {
		rec := httptest.NewRecorder()
		handleFoxTrackCheck(rec, httptest.NewRequest("GET", "/api/foxtrack/check", nil))
		var out struct{ State, Message string }
		if rec.Code != 200 || json.NewDecoder(rec.Body).Decode(&out) != nil {
			t.Fatalf("code %d body %s", rec.Code, rec.Body)
		}
		if (out.State == "ok" || out.State == "not_set") != (out.Message == "") {
			t.Fatalf("state %q with message %q", out.State, out.Message)
		}
		return out.State
	}

	if s := check(); s != "not_set" {
		t.Fatalf("no token: %q", s)
	}
	configMutex.Lock()
	configStore.FoxTrack2APIKey = "ftb_test"
	configMutex.Unlock()
	for code, want := range map[int]string{200: "ok", 401: "bad_token", 403: "plan", 500: "error"} {
		status = code
		if s := check(); s != want {
			t.Fatalf("HTTP %d: state %q, want %q", code, s, want)
		}
	}
	if gotAuth != "Bearer ftb_test" || gotQuery != "" {
		t.Fatalf("auth %q query %q", gotAuth, gotQuery)
	}
	fox.Close()
	if s := check(); s != "offline" {
		t.Fatalf("unreachable FoxTrack: %q", s)
	}
}

// A newly saved token is tried at once: the save wakes the command poll out
// of its refused-token backoff. Saving the same token does not.
func TestConfigSave_NewTokenWakesCommandPoll(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	oldCfg := configStore
	configStore = &config.Config{FoxTrack2APIKey: "ftb_old"}
	configMutex.Unlock()
	t.Cleanup(func() { configMutex.Lock(); configStore = oldCfg; configMutex.Unlock() })
	drain := func() bool {
		select {
		case <-bridgeCommandsWake:
			return true
		default:
			return false
		}
	}
	drain()
	save := func(body string) {
		rec := httptest.NewRecorder()
		handleConfig(rec, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("save %s: %d %s", body, rec.Code, rec.Body)
		}
	}

	save(`{"foxtrack2_api_key":""}`) // blank keeps the saved token
	if drain() {
		t.Fatal("an unchanged token woke the poll")
	}
	save(`{"foxtrack2_api_key":"ftb_new"}`)
	if !drain() {
		t.Fatal("a new token did not wake the poll")
	}
}
