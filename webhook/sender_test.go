package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendRelay_RequiresHTTPS(t *testing.T) {
	err := SendRelay("token", "http://example.com/hook", "printer-serial", "printer-name", RelayPayload{})
	if err == nil {
		t.Fatalf("expected error for non-HTTPS relay URL")
	}
}

func TestSendRelay_PostsHeadersAndPayload(t *testing.T) {
	type receivedRequest struct {
		authHeader    string
		printerSerial string
		printerName   string
		contentType   string
		relayPayload  RelayPayload
	}

	received := receivedRequest{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.authHeader = r.Header.Get("Authorization")
		received.printerSerial = r.Header.Get("X-Printer-Serial")
		received.printerName = r.Header.Get("X-Printer-Name")
		received.contentType = r.Header.Get("Content-Type")

		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&received.relayPayload); err != nil {
			t.Fatalf("decode relay payload: %v", err)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	oldClient := relayHTTPClient
	client := srv.Client()
	client.Timeout = oldClient.Timeout
	relayHTTPClient = client
	defer func() {
		relayHTTPClient = oldClient
	}()

	payload := RelayPayload{Print: RelayPrint{
		GcodeState:         "RUNNING",
		SubTaskName:        "cube.gcode",
		McPercent:          42,
		NozzleTemper:       215.3,
		NozzleTargetTemper: 220,
		BedTemper:          58,
		BedTargetTemper:    60,
	}}

	err := SendRelay("fox-token", srv.URL, "http://192.168.1.22:7125", "Shop Klipper", payload)
	if err != nil {
		t.Fatalf("SendRelay error: %v", err)
	}

	if received.authHeader != "Bearer fox-token" {
		t.Fatalf("unexpected authorization header: %q", received.authHeader)
	}
	if received.printerSerial != "http://192.168.1.22:7125" {
		t.Fatalf("unexpected X-Printer-Serial header: %q", received.printerSerial)
	}
	if received.printerName != "Shop Klipper" {
		t.Fatalf("unexpected X-Printer-Name header: %q", received.printerName)
	}
	if received.contentType != "application/json" {
		t.Fatalf("unexpected content type: %q", received.contentType)
	}

	if received.relayPayload.Print.GcodeState != "RUNNING" ||
		received.relayPayload.Print.SubTaskName != "cube.gcode" ||
		received.relayPayload.Print.McPercent != 42 {
		t.Fatalf("unexpected relay print identity fields: %+v", received.relayPayload.Print)
	}
	if received.relayPayload.Print.NozzleTemper != 215.3 ||
		received.relayPayload.Print.NozzleTargetTemper != 220 ||
		received.relayPayload.Print.BedTemper != 58 ||
		received.relayPayload.Print.BedTargetTemper != 60 {
		t.Fatalf("unexpected relay temperature fields: %+v", received.relayPayload.Print)
	}
}
