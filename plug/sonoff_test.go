package plug

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sonoffHost(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	host, err := hostOf(srv.URL)
	if err != nil {
		t.Fatalf("hostOf(%q): %v", srv.URL, err)
	}
	return host
}

func TestSonoffRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zeroconf/info" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		body, _ := io.ReadAll(r.Body)
		var got struct {
			DeviceID string `json:"deviceid"`
			Data     any    `json:"data"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body %q is not JSON: %v", body, err)
		}
		if got.DeviceID != "" {
			t.Errorf("deviceid = %q, want empty", got.DeviceID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"seq":2,"error":0,"data":{"switch":"on","startup":"off","fwVersion":"3.5.0"}}`))
	}))
	defer srv.Close()

	r, err := sonoffRead(context.Background(), sonoffHost(t, srv))
	if err != nil {
		t.Fatalf("sonoffRead: %v", err)
	}
	if !r.On {
		t.Errorf("On = false, want true")
	}
	if r.Watts != nil {
		t.Errorf("Watts = %v, want nil", *r.Watts)
	}
	if r.EnergyWh != nil {
		t.Errorf("EnergyWh = %v, want nil", *r.EnergyWh)
	}
}

func TestSonoffReadSwitches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"seq":2,"error":0,"data":{"switches":[{"switch":"on","outlet":0}]}}`))
	}))
	defer srv.Close()

	r, err := sonoffRead(context.Background(), sonoffHost(t, srv))
	if err != nil {
		t.Fatalf("sonoffRead: %v", err)
	}
	if !r.On {
		t.Errorf("On = false, want true")
	}
	if r.Watts != nil || r.EnergyWh != nil {
		t.Errorf("Watts/EnergyWh = %v/%v, want nil", r.Watts, r.EnergyWh)
	}
}

func TestSonoffReadNoState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"seq":2,"error":0,"data":{"startup":"off","fwVersion":"3.4.0"}}`))
	}))
	defer srv.Close()

	_, err := sonoffRead(context.Background(), sonoffHost(t, srv))
	if err == nil || err.Error() != errSonoffNoState.Error() {
		t.Errorf("err = %v, want %v", err, errSonoffNoState)
	}
}

func TestSonoffSwitch(t *testing.T) {
	var gotPath, gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/zeroconf/info":
			w.Write([]byte(`{"seq":2,"error":0,"data":{"switch":"off"}}`))
		case "/zeroconf/switch":
			w.Write([]byte(`{"seq":3,"error":0,"data":{}}`))
		}
	}))
	defer srv.Close()

	if err := sonoffSwitch(context.Background(), sonoffHost(t, srv), true); err != nil {
		t.Fatalf("sonoffSwitch: %v", err)
	}
	if gotPath != "/zeroconf/switch" {
		t.Errorf("last path = %q, want /zeroconf/switch", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var got struct {
		DeviceID string `json:"deviceid"`
		Data     struct {
			Switch string `json:"switch"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("switch body %q is not JSON: %v", gotBody, err)
	}
	if got.DeviceID != "" || got.Data.Switch != "on" {
		t.Errorf("switch body = %s, want deviceid empty and switch on", gotBody)
	}
}

func TestSonoffSwitchMultiOutlet(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/zeroconf/info":
			w.Write([]byte(`{"seq":2,"error":0,"data":{"switches":[{"switch":"off","outlet":0}]}}`))
		case "/zeroconf/switches":
			gotPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			gotBody = string(body)
			w.Write([]byte(`{"seq":3,"error":0,"data":{}}`))
		}
	}))
	defer srv.Close()

	if err := sonoffSwitch(context.Background(), sonoffHost(t, srv), false); err != nil {
		t.Fatalf("sonoffSwitch: %v", err)
	}
	if gotPath != "/zeroconf/switches" {
		t.Fatalf("path = %q, want /zeroconf/switches", gotPath)
	}
	var got struct {
		Data struct {
			Switches []struct {
				Switch string `json:"switch"`
				Outlet int    `json:"outlet"`
			} `json:"switches"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(gotBody), &got); err != nil {
		t.Fatalf("switches body %q is not JSON: %v", gotBody, err)
	}
	if len(got.Data.Switches) != 1 || got.Data.Switches[0].Switch != "off" || got.Data.Switches[0].Outlet != 0 {
		t.Errorf("switches body = %s, want one entry switch off outlet 0", gotBody)
	}
}

func TestSonoffEWeLinkMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"seq":2,"error":401,"data":{}}`))
	}))
	defer srv.Close()

	_, err := sonoffRead(context.Background(), sonoffHost(t, srv))
	if err == nil || !strings.Contains(err.Error(), "eWeLink mode") {
		t.Errorf("err = %v, want the eWeLink message", err)
	}
}

func TestSonoffNoAnswer(t *testing.T) {
	_, err := sonoffRead(context.Background(), "127.0.0.1:1")
	if err != errNoAnswer {
		t.Errorf("err = %v, want %v", err, errNoAnswer)
	}
}
