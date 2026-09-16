package mqtt

import (
	"testing"
	"time"

	"foxtrack-bridge/webhook"
)

type fakeMessage struct{ payload string }

func (m fakeMessage) Duplicate() bool   { return false }
func (m fakeMessage) Qos() byte         { return 0 }
func (m fakeMessage) Retained() bool    { return false }
func (m fakeMessage) Topic() string     { return "device/TEST/report" }
func (m fakeMessage) MessageID() uint16 { return 0 }
func (m fakeMessage) Payload() []byte   { return []byte(m.payload) }
func (m fakeMessage) Ack()              {}

// A partial Bambu message carries no gcode_state. The relay payload must still
// carry the printer's known state, or FoxTrack shows the printer as Unknown.
func TestHandler_PartialMessageKeepsRelayGcodeState(t *testing.T) {
	const name = "relay-state-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })

	sent := make(chan webhook.RelayPayload, 4)
	prevSend := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error {
		sent <- p
		return nil
	}
	t.Cleanup(func() { sendRelay = prevSend })

	handle := makeHandler(Printer{Name: name, Serial: "TEST", APIKey: "key"})
	next := func() webhook.RelayPayload {
		t.Helper()
		select {
		case p := <-sent:
			return p
		case <-time.After(2 * time.Second):
			t.Fatal("no relay payload sent")
			return webhook.RelayPayload{}
		}
	}

	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","mc_percent":40,"nozzle_temper":220,"bed_temper":45}}`})
	if got := next().Print.GcodeState; got != "RUNNING" {
		t.Fatalf("full report: gcode_state = %q, want RUNNING", got)
	}

	handle(nil, fakeMessage{`{"print":{"nozzle_temper":223}}`})
	if got := next().Print.GcodeState; got != "RUNNING" {
		t.Fatalf("partial report: gcode_state = %q, want RUNNING", got)
	}
}

func TestRelayGcodeState_RoundTripsBambuStates(t *testing.T) {
	for _, s := range []string{"IDLE", "RUNNING", "PAUSE", "FINISH", "FAILED", "PREPARE", "SLICING"} {
		if got := relayGcodeState(mapGcodeState(s)); got != s {
			t.Errorf("relayGcodeState(mapGcodeState(%q)) = %q", s, got)
		}
	}
	for _, s := range []string{"", "connected", "disconnected"} {
		if got := relayGcodeState(s); got != "" {
			t.Errorf("relayGcodeState(%q) = %q, want blank", s, got)
		}
	}
}

func TestHasPrintObject(t *testing.T) {
	cases := map[string]bool{
		`{}`:                       false,
		`{"print":null}`:           false,
		`{"info":{"command":"x"}}`: false,
		`not json`:                 false,
		`{"print":{}}`:             true,
		`{"print":{"msg":0}}`:      true,
	}
	for payload, want := range cases {
		if got := hasPrintObject([]byte(payload)); got != want {
			t.Errorf("hasPrintObject(%s) = %v, want %v", payload, got, want)
		}
	}
}

func TestSnapshotBackoff_PausesAfterRepeatedFailures(t *testing.T) {
	const name = "snapshot-backoff-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })

	now := int64(1_000_000)
	for i := 1; i < snapFailLimit; i++ {
		if !snapshotDue(name, now) {
			t.Fatalf("try %d: snapshot not due", i)
		}
		if noteSnapshotFailure(name, now) {
			t.Fatalf("try %d: backoff started too early", i)
		}
		now += snapInterval
	}
	if !snapshotDue(name, now) {
		t.Fatal("last try: snapshot not due")
	}
	if !noteSnapshotFailure(name, now) {
		t.Fatalf("backoff did not start after %d failures", snapFailLimit)
	}
	if snapshotDue(name, now+snapInterval) {
		t.Fatal("snapshot due during backoff")
	}
	if !snapshotDue(name, now+snapFailBackoff) {
		t.Fatal("snapshot not due after backoff ended")
	}

	noteSnapshotSuccess(name)
	if noteSnapshotFailure(name, now+snapFailBackoff) {
		t.Fatal("success did not reset the failure count")
	}
}
