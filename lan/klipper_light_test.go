package lan

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configpkg "foxtrack-bridge/config"
)

// fakePowerDevices answers Moonraker's device list with devices (a JSON array)
// and records every POST.
func fakePowerDevices(t *testing.T, devices string, posts *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			*posts = append(*posts, r.URL.RequestURI())
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":{"devices":` + devices + `}}`))
	}))
}

// A light command must never fall back to some other power device: it may feed
// the printer itself.
func TestKlipperLight_OnlyADeviceNamedLikeALight(t *testing.T) {
	var posts []string
	srv := fakePowerDevices(t, `[{"device":"printer_psu"},{"device":"Chamber LED"}]`, &posts)
	defer srv.Close()
	c := NewController()
	c.printers["Voron"] = configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL}
	if err := c.SendCommand("Voron", "light_on", nil); err != nil {
		t.Fatalf("light_on with a Chamber LED device: %v", err)
	}
	if len(posts) != 1 || !strings.Contains(posts[0], "device=Chamber+LED") || !strings.Contains(posts[0], "action=on") {
		t.Fatalf("posts = %q, want one switching the Chamber LED on", posts)
	}
}

func TestKlipperLight_NoLightDeviceIsAnErrorAndSwitchesNothing(t *testing.T) {
	for name, devices := range map[string]string{
		"only a psu":     `[{"device":"printer_psu"}]`,
		"plain names":    `["psu","fan_relay"]`,
		"nameless entry": `[{"status":"on"}]`,
	} {
		var posts []string
		srv := fakePowerDevices(t, devices, &posts)
		c := NewController()
		c.printers["Voron"] = configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL}
		for _, cmd := range []string{"light_on", "light_off", "toggle_light"} {
			if err := c.SendCommand("Voron", cmd, nil); !errors.Is(err, errNoKlipperLight) {
				t.Errorf("%s, %s: err = %v, want errNoKlipperLight", name, cmd, err)
			}
		}
		if len(posts) != 0 {
			t.Errorf("%s: switched %q, want nothing", name, posts)
		}
		srv.Close()
	}
}
