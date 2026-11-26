package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

// A second Bridge on a port that is taken stops with a message the user can
// act on, before it loads the config, connects to a printer or reports that
// the dashboard is up.
func TestStartServerPortInUseStartsNothing(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	prevWait, prevConnect := listenWait, connectPrinters
	t.Cleanup(func() { listenWait, connectPrinters = prevWait, prevConnect })
	listenWait = 300 * time.Millisecond
	connected := false
	connectPrinters = func(_, _ *config.Config) { connected = true }

	configMutex.RLock()
	before := configStore
	configMutex.RUnlock()

	listening := false
	err = StartServer(port, func() { listening = true })
	if err == nil {
		t.Fatal("StartServer on a taken port returned no error")
	}
	want := fmt.Sprintf("Port %d is in use. Close the other program, or start the Bridge with --port or FOXTRACK_BRIDGE_PORT.", port)
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if !addrInUse(err) {
		t.Errorf("error %v does not unwrap to address-in-use", err)
	}
	if connected || listening {
		t.Errorf("connected = %v, listening = %v; want neither when the port is taken", connected, listening)
	}
	configMutex.RLock()
	after := configStore
	configMutex.RUnlock()
	if after != before {
		t.Error("StartServer replaced the stored config although it could not open the port")
	}
}

func TestDefaultPortFromEnv(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv("FOXTRACK_BRIDGE_PORT", "")
		got, err := defaultPortFromEnv()
		if err != nil || got != defaultPort {
			t.Fatalf("got (%d, %v), want (%d, nil)", got, err, defaultPort)
		}
	})

	t.Run("set", func(t *testing.T) {
		t.Setenv("FOXTRACK_BRIDGE_PORT", "9191")
		got, err := defaultPortFromEnv()
		if err != nil || got != 9191 {
			t.Fatalf("got (%d, %v), want (9191, nil)", got, err)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Setenv("FOXTRACK_BRIDGE_PORT", "not-a-port")
		if _, err := defaultPortFromEnv(); err == nil {
			t.Fatal("expected error for invalid FOXTRACK_BRIDGE_PORT")
		}
	})
}

func TestResolvePort(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		got, err := resolvePort(nil, 8080)
		if err != nil || got != 8080 {
			t.Fatalf("got (%d, %v), want (8080, nil)", got, err)
		}
	})

	t.Run("flag overrides default", func(t *testing.T) {
		got, err := resolvePort([]string{"--port", "9090"}, 8080)
		if err != nil || got != 9090 {
			t.Fatalf("got (%d, %v), want (9090, nil)", got, err)
		}
	})

	t.Run("unknown flag errors", func(t *testing.T) {
		if _, err := resolvePort([]string{"--prot", "9090"}, 8080); err == nil {
			t.Fatal("expected error for unknown flag")
		}
	})

	t.Run("help returns ErrHelp", func(t *testing.T) {
		_, err := resolvePort([]string{"-h"}, 8080)
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("got %v, want flag.ErrHelp", err)
		}
	})
}
