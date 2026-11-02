package mqtt

import (
	"testing"
	"time"

	"foxtrack-bridge/webhook"
)

// A slow post must not let a later one overtake it: FoxTrack replaces status
// with each post, so FINISH landing before RUNNING would leave it "printing".
func TestRelay_PostsLeaveInOrder(t *testing.T) {
	const name = "relay-order-test"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name) })

	inFlight, release := make(chan struct{}), make(chan struct{})
	sent := make(chan string, 4)
	prevSend := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error {
		if p.Print.GcodeState == "RUNNING" {
			close(inFlight)
			<-release // the first post hangs on a slow network
		}
		sent <- p.Print.GcodeState
		return nil
	}
	t.Cleanup(func() { sendRelay = prevSend })

	handle := makeHandler(Printer{Name: name, Serial: "ORDER", APIKey: "key"})
	UpdatePrinterState(name, TelemetryData{Status: "connected"})
	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","mc_percent":99,"subtask_name":"cube"}}`})
	<-inFlight
	handle(nil, fakeMessage{`{"print":{"gcode_state":"FINISH","mc_percent":100}}`})

	select {
	case got := <-sent:
		t.Fatalf("%s was posted while RUNNING was still in flight", got)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	for _, want := range []string{"RUNNING", "FINISH"} {
		select {
		case got := <-sent:
			if got != want {
				t.Fatalf("posted %s, want %s", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s never posted", want)
		}
	}
}
