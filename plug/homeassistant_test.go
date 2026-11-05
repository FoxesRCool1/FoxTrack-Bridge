package plug

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

// newFakeHA serves a fake Home Assistant REST API. States are keyed by
// entity_id; requests without the right bearer token get 401.
func newFakeHA(t *testing.T, token string, states map[string]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/states/"):
			entity := strings.TrimPrefix(r.URL.Path, "/api/states/")
			s, ok := states[entity]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/services/homeassistant/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["entity_id"]; !ok {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func haPlug(addr string) config.Plug {
	return config.Plug{
		Kind:    config.PlugHA,
		Address: addr,
		Token:   "secret-token",
		Entity:  "switch.printer_plug",
	}
}

func TestHABase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ha.local:8123/", "http://ha.local:8123"},
		{"  ha.local  ", "http://ha.local"},
		{"http://ha.local:8123/", "http://ha.local:8123"},
		{"https://ha.local", "https://ha.local"},
	}
	for _, c := range cases {
		if got := haBase(c.in); got != c.want {
			t.Errorf("haBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHAPlugReadOnWithPower(t *testing.T) {
	srv := newFakeHA(t, "secret-token", map[string]map[string]any{
		"switch.printer_plug":  {"entity_id": "switch.printer_plug", "state": "on"},
		"sensor.plug_power":    {"entity_id": "sensor.plug_power", "state": "152.4", "attributes": map[string]any{"unit_of_measurement": "W"}},
		"sensor.plug_power_kw": {"entity_id": "sensor.plug_power_kw", "state": "0.2", "attributes": map[string]any{"unit_of_measurement": "kW"}},
		"sensor.plug_power_un": {"entity_id": "sensor.plug_power_un", "state": "unavailable"},
	})
	defer srv.Close()

	g := haPlug(srv.URL)
	g.PowerEntity = "sensor.plug_power"
	r, err := haPlugRead(context.Background(), g)
	if err != nil {
		t.Fatalf("haPlugRead: %v", err)
	}
	if !r.On {
		t.Error("On = false, want true")
	}
	if r.Watts == nil || *r.Watts != 152.4 {
		t.Errorf("Watts = %v, want 152.4", r.Watts)
	}
	if r.EnergyWh != nil {
		t.Errorf("EnergyWh = %v, want nil", r.EnergyWh)
	}

	g.PowerEntity = "sensor.plug_power_kw"
	r, err = haPlugRead(context.Background(), g)
	if err != nil {
		t.Fatalf("haPlugRead (kW): %v", err)
	}
	if r.Watts == nil || *r.Watts != 200 {
		t.Errorf("Watts = %v, want 200", r.Watts)
	}

	g.PowerEntity = "sensor.plug_power_un"
	r, err = haPlugRead(context.Background(), g)
	if err != nil {
		t.Fatalf("haPlugRead (unavailable power): %v", err)
	}
	if r.Watts != nil {
		t.Errorf("Watts = %v, want nil", r.Watts)
	}

	g.PowerEntity = ""
	r, err = haPlugRead(context.Background(), g)
	if err != nil {
		t.Fatalf("haPlugRead (no power entity): %v", err)
	}
	if r.Watts != nil {
		t.Errorf("Watts = %v, want nil", r.Watts)
	}
}

func TestHAPlugReadOff(t *testing.T) {
	srv := newFakeHA(t, "secret-token", map[string]map[string]any{
		"switch.printer_plug": {"entity_id": "switch.printer_plug", "state": "off"},
	})
	defer srv.Close()

	r, err := haPlugRead(context.Background(), haPlug(srv.URL))
	if err != nil {
		t.Fatalf("haPlugRead: %v", err)
	}
	if r.On {
		t.Error("On = true, want false")
	}
	if r.Watts != nil {
		t.Errorf("Watts = %v, want nil", r.Watts)
	}
}

func TestHAPlugReadUnavailable(t *testing.T) {
	srv := newFakeHA(t, "secret-token", map[string]map[string]any{
		"switch.printer_plug": {"entity_id": "switch.printer_plug", "state": "unavailable"},
	})
	defer srv.Close()

	_, err := haPlugRead(context.Background(), haPlug(srv.URL))
	if !errors.Is(err, errHAUnavailable) {
		t.Errorf("err = %v, want errHAUnavailable", err)
	}
}

func TestHAPlugReadWrongToken(t *testing.T) {
	srv := newFakeHA(t, "the-right-token", map[string]map[string]any{
		"switch.printer_plug": {"entity_id": "switch.printer_plug", "state": "on"},
	})
	defer srv.Close()

	_, err := haPlugRead(context.Background(), haPlug(srv.URL))
	if !errors.Is(err, errHAToken) {
		t.Errorf("err = %v, want errHAToken", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Error("error message contains the token")
	}
}

func TestHAPlugReadUnknownEntity(t *testing.T) {
	srv := newFakeHA(t, "secret-token", nil)
	defer srv.Close()

	g := haPlug(srv.URL)
	g.Entity = "switch.does_not_exist"
	_, err := haPlugRead(context.Background(), g)
	if err == nil {
		t.Fatal("expected an error for an unknown entity")
	}
	if !strings.Contains(err.Error(), "switch.does_not_exist") {
		t.Errorf("err = %v, want it to name the entity", err)
	}
}

func TestHAPlugReadMissingEntityOrToken(t *testing.T) {
	g := haPlug("http://ha.local")
	g.Entity = ""
	if _, err := haPlugRead(context.Background(), g); !errors.Is(err, errHAEntityToken) {
		t.Errorf("err = %v, want errHAEntityToken", err)
	}
	g = haPlug("http://ha.local")
	g.Token = ""
	if _, err := haPlugRead(context.Background(), g); !errors.Is(err, errHAEntityToken) {
		t.Errorf("err = %v, want errHAEntityToken", err)
	}
	if err := haPlugSwitch(context.Background(), g, true); !errors.Is(err, errHAEntityToken) {
		t.Errorf("err = %v, want errHAEntityToken", err)
	}
}

func TestHAPlugSwitchOn(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		path, body = r.URL.Path, string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := haPlugSwitch(context.Background(), haPlug(srv.URL), true); err != nil {
		t.Fatalf("haPlugSwitch: %v", err)
	}
	if path != "/api/services/homeassistant/turn_on" {
		t.Errorf("path = %q, want /api/services/homeassistant/turn_on", path)
	}
	if body != `{"entity_id":"switch.printer_plug"}` {
		t.Errorf("body = %q, want {\"entity_id\":\"switch.printer_plug\"}", body)
	}
}

func TestHAPlugSwitchOff(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := haPlugSwitch(context.Background(), haPlug(srv.URL), false); err != nil {
		t.Fatalf("haPlugSwitch: %v", err)
	}
	if path != "/api/services/homeassistant/turn_off" {
		t.Errorf("path = %q, want /api/services/homeassistant/turn_off", path)
	}
}

func TestHAPlugSwitchWrongToken(t *testing.T) {
	srv := newFakeHA(t, "the-right-token", nil)
	defer srv.Close()

	if err := haPlugSwitch(context.Background(), haPlug(srv.URL), true); !errors.Is(err, errHAToken) {
		t.Errorf("err = %v, want errHAToken", err)
	}
}
