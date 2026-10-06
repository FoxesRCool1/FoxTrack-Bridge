package mqtt

import (
	"foxtrack-bridge/version"
	"foxtrack-bridge/webhook"
)

// stampRelayIdentity adds what FoxTrack's Print dialog needs to the relay
// payload: kind, model, Bridge version and capabilities. Over Bambu Cloud the
// printer cannot be sent a file, so it announces no capability.
func stampRelayIdentity(pr *webhook.RelayPrint, serial, telemetryModel string) {
	pr.PrinterKind = "bambu"
	pr.PrinterModel = ModelFromSerial(serial)
	if pr.PrinterModel == "" {
		pr.PrinterModel = telemetryModel
	}
	pr.BridgeVersion = version.AppVersion
	if !IsCloudSerial(serial) {
		pr.BridgeCapabilities = []string{"print_file"}
	}
}
