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

func TestFormatWatts(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{100, "100"},
		{2000, "2000"},
		{12.34, "12.3"},
	} {
		if got := formatWatts(tc.in); got != tc.want {
			t.Errorf("formatWatts(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
