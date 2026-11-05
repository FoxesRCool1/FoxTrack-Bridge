package plug

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMyStromRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/report" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"power":12.5,"Ws":11.9,"relay":true,"temperature":22.1}`))
	}))
	defer srv.Close()

	host, err := hostOf(srv.URL)
	if err != nil {
		t.Fatalf("hostOf(%q): %v", srv.URL, err)
	}
	r, err := mystromRead(context.Background(), host, "")
	if err != nil {
		t.Fatalf("mystromRead: %v", err)
	}
	if !r.On {
		t.Errorf("On = false, want true")
	}
	if r.Watts == nil || *r.Watts != 12.5 {
		t.Errorf("Watts = %v, want 12.5", r.Watts)
	}
	if r.EnergyWh != nil {
		t.Errorf("EnergyWh = %v, want nil", r.EnergyWh)
	}
}

func TestMyStromReadRelayOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"power":0,"Ws":0,"relay":false,"temperature":22.1}`))
	}))
	defer srv.Close()

	host, err := hostOf(srv.URL)
	if err != nil {
		t.Fatalf("hostOf(%q): %v", srv.URL, err)
	}
	r, err := mystromRead(context.Background(), host, "")
	if err != nil {
		t.Fatalf("mystromRead: %v", err)
	}
	if r.On {
		t.Errorf("On = true, want false")
	}
}

func TestMyStromReadBadBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`hello`))
	}))
	defer srv.Close()

	host, err := hostOf(srv.URL)
	if err != nil {
		t.Fatalf("hostOf(%q): %v", srv.URL, err)
	}
	_, err = mystromRead(context.Background(), host, "")
	if !errors.Is(err, errMyStrom) {
		t.Errorf("err = %v, want %v", err, errMyStrom)
	}
}

func TestMyStromSwitch(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("state")
	}))
	defer srv.Close()

	host, err := hostOf(srv.URL)
	if err != nil {
		t.Fatalf("hostOf(%q): %v", srv.URL, err)
	}
	if err := mystromSwitch(context.Background(), host, "", true); err != nil {
		t.Fatalf("mystromSwitch on: %v", err)
	}
	if gotPath != "/relay" || gotQuery != "1" {
		t.Errorf("on: path=%q state=%q, want /relay state=1", gotPath, gotQuery)
	}
	if err := mystromSwitch(context.Background(), host, "", false); err != nil {
		t.Fatalf("mystromSwitch off: %v", err)
	}
	if gotPath != "/relay" || gotQuery != "0" {
		t.Errorf("off: path=%q state=%q, want /relay state=0", gotPath, gotQuery)
	}
}

func TestMyStromSwitchRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	host, err := hostOf(srv.URL)
	if err != nil {
		t.Fatalf("hostOf(%q): %v", srv.URL, err)
	}
	err = mystromSwitch(context.Background(), host, "", true)
	if err == nil || err.Error() != "The myStrom plug refused the switch command." {
		t.Errorf("err = %v, want the refuse error", err)
	}
}
