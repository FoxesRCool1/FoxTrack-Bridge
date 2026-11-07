package plug

import (
	"errors"
	"testing"

	"foxtrack-bridge/config"
)

func TestKasaDoPicksProtocol(t *testing.T) {
	kasaKLAPHosts.Range(func(k, _ any) bool { kasaKLAPHosts.Delete(k); return true })
	var calls []string
	legacy := func(err error) func() error {
		return func() error { calls = append(calls, "legacy"); return err }
	}
	klap := func(err error) func(string) error {
		return func(string) error { calls = append(calls, "klap"); return err }
	}
	run := func(host string, g config.Plug, l, k error) (error, []string) {
		calls = nil
		err := kasaDo(host, g, legacy(l), klap(k))
		return err, calls
	}

	// Old firmware: legacy answers, KLAP never tried.
	if err, c := run("h1", config.Plug{}, nil, errNoAnswer); err != nil || len(c) != 1 || c[0] != "legacy" {
		t.Fatalf("old firmware: %v %v", err, c)
	}
	// New firmware: 9999 silent, KLAP works and is remembered.
	if err, c := run("h2", config.Plug{}, errNoAnswer, nil); err != nil || len(c) != 2 {
		t.Fatalf("new firmware: %v %v", err, c)
	}
	if err, c := run("h2", config.Plug{}, errNoAnswer, nil); err != nil || len(c) != 1 || c[0] != "klap" {
		t.Fatalf("remembered: %v %v", err, c)
	}
	// A login set: KLAP first.
	if _, c := run("h3", config.Plug{Username: "me@example.com"}, nil, nil); c[0] != "klap" {
		t.Fatalf("login: %v", c)
	}
	// A wrong login is reported, not hidden behind the other protocol.
	if err, c := run("h4", config.Plug{Username: "me@example.com"}, nil, errKlapLogin); !errors.Is(err, errKlapLogin) || len(c) != 1 {
		t.Fatalf("wrong login: %v %v", err, c)
	}
	// Nothing answers either way.
	if err, _ := run("h5", config.Plug{}, errNoAnswer, errKlapNoKLAP); !errors.Is(err, errNoAnswer) {
		t.Fatalf("nothing: %v", err)
	}
}
