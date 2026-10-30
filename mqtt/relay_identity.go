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
		caps = append(caps, "print_file", "set_filament", "filament_use")
	}
	pr.BridgeCapabilities = WithPlugCaps(caps, pr.Plug != nil)
}

// WithPlugCaps adds the plug commands to a printer's capabilities when it has
// a plug. Shared with the lan package.
func WithPlugCaps(caps []string, hasPlug bool) []string {
	if !hasPlug {
		return caps
	}
	return append(append([]string(nil), caps...), "plug_on", "plug_off")
}
