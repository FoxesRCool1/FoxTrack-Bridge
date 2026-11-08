package mqtt

import (
	"foxtrack-bridge/version"
	"foxtrack-bridge/webhook"
)

// stampRelayIdentity adds what FoxTrack's Print dialog needs to the relay
// payload: kind, model, Bridge version and capabilities. Over Bambu Cloud the
// printer cannot be sent a file, so it announces no capability.
func stampRelayIdentity(pr *webhook.RelayPrint, p Printer, telemetryModel string) {
	pr.PrinterKind = "bambu"
	pr.PrinterModel = ModelFromSerial(p.Serial)
	if pr.PrinterModel == "" {
		pr.PrinterModel = telemetryModel
	}
	pr.BridgeVersion = version.AppVersion
	caps := []string{"alarms"}
	if !p.Cloud && !IsCloudSerial(p.Serial) {
		caps = append(caps, "print_file", "set_filament", "filament_use", "console", "files")
	}
	pr.BridgeCapabilities = WithPlugCaps(caps, pr.Plug != nil)
}

// OfflinePayload is the report for a printer the Bridge cannot reach, for the
// lan package: gcode_state OFFLINE, remaining time unknown, and no alarms key
// (FoxTrack reads that as no change). Capabilities are left out: nothing can be
// sent to the printer now, only to its plug.
func OfflinePayload(kind, model string, plug *webhook.RelayPlug) webhook.RelayPayload {
	return webhook.RelayPayload{Print: webhook.RelayPrint{
		GcodeState:         "OFFLINE",
		McRemainingTime:    -1,
		Plug:               plug,
		PrinterKind:        kind,
		PrinterModel:       model,
		BridgeVersion:      version.AppVersion,
		BridgeCapabilities: WithPlugCaps(nil, plug != nil),
	}}
}

// WithPlugCaps adds the plug commands to a printer's capabilities when it has
// a plug. Shared with the lan package.
func WithPlugCaps(caps []string, hasPlug bool) []string {
	if !hasPlug {
		return caps
	}
	return append(append([]string(nil), caps...), "plug_on", "plug_off")
}
