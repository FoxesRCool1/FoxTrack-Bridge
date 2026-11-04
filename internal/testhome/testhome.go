// Package testhome keeps tests away from the real config folder. Code under
// test saves config.json and appends to history.json through
// os.UserConfigDir, so without this a test run writes into the user's own
// Bridge settings.
package testhome

import (
	"fmt"
	"os"
	"testing"
)

// Main points every config location at a fresh temp folder, then runs the
// package's tests. Call it from TestMain. A test that needs its own folder can
// still t.Setenv("XDG_CONFIG_HOME", t.TempDir()).
func Main(m *testing.M) {
	dir, err := os.MkdirTemp("", "foxtrack-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testhome:", err)
		os.Exit(1)
	}
	for _, k := range []string{"XDG_CONFIG_HOME", "HOME", "APPDATA"} {
		os.Setenv(k, dir)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
