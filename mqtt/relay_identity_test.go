package mqtt

import (
	"testing"

	"foxtrack-bridge/webhook"
)

func TestStampRelayIdentity_LANBambuAnnouncesPrintFile(t *testing.T) {
	var pr webhook.RelayPrint
	stampRelayIdentity(&pr, Printer{Serial: "NO-SUCH-PREFIX"}, "P1S")
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

// With no cloud connection running (start-up, dropped session) IsCloudSerial
// is false; the printer's own Connection setting still keeps the capability off.
func TestStampRelayIdentity_CloudPrinterNeverAnnouncesPrintFile(t *testing.T) {
	if IsCloudSerial("01P00A000000009") {
		t.Fatal("test expects no cloud connection")
	}
	var pr webhook.RelayPrint
	stampRelayIdentity(&pr, Printer{Serial: "01P00A000000009", Cloud: true}, "")
	if len(pr.BridgeCapabilities) != 0 {
		t.Fatalf("capabilities = %v, want none for a cloud printer", pr.BridgeCapabilities)
	}
	if err := PrintPreflight(Printer{Name: "cloud-no-session", Serial: "01P00A000000009", Cloud: true}); err != errPrintCloud {
		t.Fatalf("preflight = %v, want errPrintCloud", err)
	}
}
