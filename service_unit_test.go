//go:build headless

package main

import (
	"os"
	"strings"
	"testing"
)

// The update path exits 0 on purpose so the replacement binary takes over.
// With Restart=on-failure systemd read that clean exit as "it meant to stop"
// and left the bridge down until someone started it by hand.
func TestShippedSystemdUnitsRestartAfterCleanExit(t *testing.T) {
	for _, path := range []string{
		"linux/foxtrack-bridge-user.service",
		"linux/foxtrack-bridge@.service",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(body), "Restart=on-failure") {
			t.Errorf("%s: Restart=on-failure leaves the bridge down after an update exits 0", path)
		}
		if !strings.Contains(string(body), "Restart=always") {
			t.Errorf("%s: want Restart=always so a restart-to-update comes back up", path)
		}
	}
}
