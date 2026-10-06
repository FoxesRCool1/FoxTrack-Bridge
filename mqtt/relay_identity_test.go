package mqtt

import (
	"testing"

	"foxtrack-bridge/webhook"
)

func TestStampRelayIdentity_LANBambuAnnouncesPrintFile(t *testing.T) {
	var pr webhook.RelayPrint
	stampRelayIdentity(&pr, "NO-SUCH-PREFIX", "P1S")
	if pr.PrinterKind != "bambu" || pr.BridgeVersion == "" {
		t.Fatalf("identity = %+v", pr)
	}
	if want := ModelFromSerial("NO-SUCH-PREFIX"); want == "" && pr.PrinterModel != "P1S" {
		t.Fatalf("model = %q, want the telemetry model when the serial is unknown", pr.PrinterModel)
	}
	if len(pr.BridgeCapabilities) != 1 || pr.BridgeCapabilities[0] != "print_file" {
		t.Fatalf("capabilities = %v", pr.BridgeCapabilities)
	}
}
