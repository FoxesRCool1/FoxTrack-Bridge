package mqtt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

func TestCloudIPFromReport(t *testing.T) {
	private := binary.LittleEndian.Uint32([]byte{192, 168, 1, 20})
	public := binary.LittleEndian.Uint32([]byte{8, 8, 8, 8})
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"private ip", fmt.Sprintf(`{"print":{"net":{"conf":16,"info":[{"ip":%d,"mask":4294967040}]}}}`, private), "192.168.1.20"},
		{"public ip ignored", fmt.Sprintf(`{"print":{"net":{"info":[{"ip":%d}]}}}`, public), ""},
		{"zero ignored", `{"print":{"net":{"info":[{"ip":0}]}}}`, ""},
		{"no net block", `{"print":{"gcode_state":"IDLE"}}`, ""},
		{"garbage", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cloudIPFromReport([]byte(tc.payload)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCloudBackoff_GrowsAndCaps(t *testing.T) {
	prevBase := time.Duration(0)
	for n := 1; n <= 12; n++ {
		base := cloudBackoffMin << uint(n-1)
		if n > 10 || base > cloudBackoffMax {
			base = cloudBackoffMax
		}
		got := cloudBackoff(n)
		if got < base || got > base+base/4 {
			t.Errorf("attempt %d: %v outside [%v, %v]", n, got, base, base+base/4)
		}
		if base < prevBase {
			t.Errorf("attempt %d: base %v shrank from %v", n, base, prevBase)
		}
		prevBase = base
	}
	if cloudBackoff(1) < 5*time.Second {
		t.Error("first retry must wait at least 5 seconds")
	}
}

func TestCloudErrorClassification(t *testing.T) {
	if !isCloudAuthErr(packets.ErrorRefusedNotAuthorised) || !isCloudAuthErr(fmt.Errorf("%w : boom", packets.ErrorRefusedBadUsernameOrPassword)) {
		t.Error("broker auth refusals must be terminal")
	}
	if isCloudAuthErr(errCloudLost) || isCloudAuthErr(nil) {
		t.Error("a dropped connection is not an auth failure")
	}
	if !isCloudConnectivityErr(errCloudTimeout) || !isCloudConnectivityErr(errors.New("dial tcp 1.2.3.4:8883: connect: connection refused")) {
		t.Error("timeouts and refusals count towards the ban heuristic")
	}
	if isCloudConnectivityErr(errCloudLost) || isCloudConnectivityErr(packets.ErrorRefusedNotAuthorised) {
		t.Error("a normal drop or an auth refusal is not a connectivity failure")
	}
}

func TestCloudCommandAllowed(t *testing.T) {
	for _, ok := range []string{"light", "light_on", "light_off", "toggle_light"} {
		if !cloudCommandAllowed(ok) {
			t.Errorf("%s should be allowed", ok)
		}
	}
	for _, no := range []string{"pause", "resume", "stop", "start", "print_speed", "fan_speed", "gcode", ""} {
		if cloudCommandAllowed(no) {
			t.Errorf("%q must be refused over the cloud", no)
		}
	}
}

func TestCloudSerialFromTopic(t *testing.T) {
	if got := cloudSerialFromTopic("device/01P00A123/report"); got != "01P00A123" {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{"device/01P00A123/request", "device/report", "x/y/z", ""} {
		if cloudSerialFromTopic(bad) != "" {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestSetCloudPrinters_NoSessionStaysOffline(t *testing.T) {
	printers := []Printer{{Name: "Cloudy", Serial: "S1"}}
	SetCloudPrinters(nil, printers, nil)
	if st := GetCloudStatus(); st.State != CloudStateOff {
		t.Errorf("state = %q, want off", st.State)
	}
	if IsCloudSerial("S1") {
		t.Error("no runner means no cloud serials")
	}
	if s := GetPrinterState("Cloudy"); s.Status != "disconnected" {
		t.Errorf("printer status = %q, want disconnected", s.Status)
	}
	if getSerial("Cloudy") != "S1" {
		t.Error("serial mapping must be registered even while offline so the UI can resolve it")
	}
	if err := RetryCloudNow(); err == nil {
		t.Error("retry with no connection must report an error")
	}
	RemovePrinterState("Cloudy")
}

func TestCloudClientID_StableAndSane(t *testing.T) {
	if cloudClientID == "" || len(cloudClientID) > 40 || cloudClientID[:9] != "foxtrack-" {
		t.Errorf("client id = %q", cloudClientID)
	}
	if newCloudClientID() == cloudClientID {
		t.Error("a fresh id should differ; the process-wide one must stay fixed")
	}
}
