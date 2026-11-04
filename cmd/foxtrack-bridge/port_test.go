package main

import (
	"errors"
	"flag"
	"testing"
)

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
