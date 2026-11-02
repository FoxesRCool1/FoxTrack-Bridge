package mqtt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
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

// fakeCloudClient records publishes; embedding the interface leaves every
// other method nil, which these tests never reach.
type fakeCloudClient struct {
	mqtt.Client
	published int
}

func (f *fakeCloudClient) Publish(string, byte, bool, interface{}) mqtt.Token {
	f.published++
	return nil
}

func TestCloudRename_KeepsStateAndAsksForDump(t *testing.T) {
	fake := &fakeCloudClient{}
	r := newCloudRunner(CloudSession{}, []Printer{{Name: "Old", Serial: "S9"}}, nil)
	r.client = fake
	UpdatePrinterState("Old", TelemetryData{Status: "printing", Progress: 42})
	t.Cleanup(func() {
		RemovePrinterState("Old")
		RemovePrinterState("New")
		clientMutex.Lock()
		delete(printerClients, "New")
		clientMutex.Unlock()
	})
	r.setPrinters([]Printer{{Name: "New", Serial: "S9"}}, nil)
	if s := GetPrinterState("New"); s.Status != "printing" || s.Progress != 42 {
		t.Errorf("renamed printer state = %+v, want the previous state carried over", s)
	}
	if _, ghost := GetPrintersState()["Old"]; ghost {
		t.Error("the old name must not keep a state entry after a rename")
	}
	if fake.published != 1 {
		t.Errorf("pushall publishes = %d, want 1", fake.published)
	}
}

func TestCloudStop_DoesNotWaitOutConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // accepts, never answers the CONNECT
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r := newCloudRunner(CloudSession{Broker: "tcp://" + ln.Addr().String()}, nil, nil)
	go r.loop()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	stopCloudRunner(r)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("stop took %v, want it to abort the connect wait promptly", d)
	}
}

func TestCloudRetry_StaleSignalIsDropped(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens: the connect is refused at once
	r := newCloudRunner(CloudSession{Broker: "tcp://" + addr}, nil, nil)
	r.retry <- struct{}{} // left over from a retry that raced the backoff timer
	go r.loop()
	defer stopCloudRunner(r)
	deadline := time.Now().Add(3 * time.Second)
	for cloudStatusOf(r).State != CloudStateBackoff {
		if time.Now().After(deadline) {
			t.Fatal("never reached backoff")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if st := cloudStatusOf(r); st.State != CloudStateBackoff || st.Failures != 1 {
		t.Errorf("status = %+v: the stale retry skipped the backoff wait", st)
	}
}

func cloudStatusOf(r *cloudRunner) CloudStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}
