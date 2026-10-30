package main

import (
	"testing"

	mqttpkg "foxtrack-bridge/mqtt"
)

// A printer that is not reporting (link down, or no report yet) is "unknown"
// to the plug guards, never "idle".
func TestTelemetryReporting(t *testing.T) {
	for status, want := range map[string]bool{"disconnected": false, "connected": false, "printing": true, "idle": true, "paused": true} {
		if got := telemetryReporting(&mqttpkg.TelemetryData{Status: status, Timestamp: 1}); got != want {
			t.Errorf("status %q: reporting = %v, want %v", status, got, want)
		}
	}
	if telemetryReporting(nil) {
		t.Error("nil telemetry reported as live")
	}
}
