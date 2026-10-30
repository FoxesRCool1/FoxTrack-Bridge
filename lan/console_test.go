package lan

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configpkg "foxtrack-bridge/config"
)

// recordingServer answers 200 {} and records "METHOD path?query body".
func recordingServer(t *testing.T, got *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = r.Method + " " + r.URL.RequestURI() + " " + string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
}

func TestKlipperConsole_GcodeScript(t *testing.T) {
	var got string
	srv := recordingServer(t, &got)
	defer srv.Close()
	c := NewController()
	c.printers["Voron"] = configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL}

	cases := []struct {
		cmd  string
		args map[string]interface{}
		want string
	}{
		{"set_nozzle_temp", map[string]interface{}{"celsius": 215.0}, "POST /printer/gcode/script?script=M104+S215 "},
		{"set_bed_temp", map[string]interface{}{"celsius": 0.0}, "POST /printer/gcode/script?script=M140+S0 "},
		{"home", nil, "POST /printer/gcode/script?script=G28 "},
	}
	for _, tc := range cases {
		got = ""
		if err := c.SendCommand("Voron", tc.cmd, tc.args); err != nil {
			t.Fatalf("%s: %v", tc.cmd, err)
		}
		if got != tc.want {
			t.Fatalf("%s sent %q, want %q", tc.cmd, got, tc.want)
		}
	}
	got = ""
	if err := c.SendCommand("Voron", "set_bed_temp", map[string]interface{}{"celsius": 150.0}); err == nil || got != "" {
		t.Fatalf("150 C bed: err=%v sent=%q; want a refusal and no request", err, got)
	}
}

func TestOctoPrintConsole_Requests(t *testing.T) {
	cases := []struct {
		cmd        string
		args       map[string]interface{}
		path, body string
	}{
		{"set_nozzle_temp", map[string]interface{}{"celsius": 210.0}, "/api/printer/tool", `{"command":"target","targets":{"tool0":210}}`},
		{"set_bed_temp", map[string]interface{}{"celsius": 60.0}, "/api/printer/bed", `{"command":"target","target":60}`},
		{"home", nil, "/api/printer/printhead", `{"command":"home","axes":["x","y","z"]}`},
		{"pause", nil, "/api/job", `{"command":"pause","action":"pause"}`},
	}
	for _, tc := range cases {
		path, body, err := octoPrintCommandRequest(tc.cmd, tc.args)
		if err != nil || path != tc.path || body != tc.body {
			t.Errorf("%s: %q %q %v", tc.cmd, path, body, err)
		}
	}
	if _, _, err := octoPrintCommandRequest("set_nozzle_temp", map[string]interface{}{"celsius": 400.0}); err == nil {
		t.Fatal("400 C must be refused")
	}

	// Through the controller: the request reaches OctoPrint with the key.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "octo-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.Path + " " + string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewController()
	c.printers["Ender"] = configpkg.Printer{Name: "Ender", OctoPrintURL: srv.URL, APIKey: "octo-key"}
	if err := c.SendCommand("Ender", "set_bed_temp", map[string]interface{}{"celsius": 65.0}); err != nil {
		t.Fatal(err)
	}
	if got != `POST /api/printer/bed {"command":"target","target":65}` {
		t.Fatalf("sent %q", got)
	}
}

func TestOctoPrintConsole_409IsNotConnected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	c := NewController()
	c.printers["Ender"] = configpkg.Printer{Name: "Ender", OctoPrintURL: srv.URL, APIKey: "k"}
	err := c.SendCommand("Ender", "home", nil)
	if err == nil || !strings.Contains(err.Error(), "not connected to the printer") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrusaLinkConsole_Refused(t *testing.T) {
	var write string
	srv := prusaLinkServer(t, "IDLE", &write, nil)
	defer srv.Close()
	c := NewController()
	c.printers["MK4"] = configpkg.Printer{Name: "MK4", PrusaLinkURL: srv.URL, APIKey: "prusa-key"}
	for _, cmd := range []string{"set_nozzle_temp", "set_bed_temp", "home"} {
		err := c.SendCommand("MK4", cmd, map[string]interface{}{"celsius": 60.0})
		if err != errPrusaNoConsole {
			t.Fatalf("%s: err = %v", cmd, err)
		}
		if write != "" {
			t.Fatalf("%s: a request was sent: %q", cmd, write)
		}
	}
}
