package mqtt

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"

	"foxtrack-bridge/webhook"
)

const verifyFailed = `{"print":{"command":"pause","result":"fail","reason":"mqtt message verify failed","sequence_id":"0"}}`

func TestControlBlocked_MarkedByVerifyFailedAndClearedBySuccess(t *testing.T) {
	const name = "cap-mark"
	t.Cleanup(func() { clearControlBlocked(name) })
	if ControlBlocked(name) {
		t.Fatal("blocked before any reply")
	}

	NoteControlReply(name, []byte(verifyFailed))
	if !ControlBlocked(name) {
		t.Fatal("a verify failed reply did not mark the printer")
	}

	// A report or an answer about something else changes nothing.
	NoteControlReply(name, []byte(`{"print":{"command":"push_status","gcode_state":"IDLE"}}`))
	NoteControlReply(name, []byte(`{"print":{"command":"gcode_line","result":"fail","reason":"param error"}}`))
	NoteControlReply(name, []byte(`not json "result"`))
	if !ControlBlocked(name) {
		t.Fatal("an unrelated message cleared the mark")
	}

	NoteControlReply(name, []byte(`{"print":{"command":"gcode_line","param":"M104 S0","reason":"success","result":"success","sequence_id":"0"}}`))
	if ControlBlocked(name) {
		t.Fatal("a successful command did not clear the mark")
	}
}

func TestControlBlocked_OtherReplyShapes(t *testing.T) {
	const name = "cap-shapes"
	t.Cleanup(func() { clearControlBlocked(name) })

	// Light commands answer under "system", and "failed" is spelled both ways.
	NoteControlReply(name, []byte(`{"system":{"command":"ledctrl","result":"failed","reason":"mqtt message verify failed"}}`))
	if !ControlBlocked(name) {
		t.Fatal("a refused light command did not mark the printer")
	}
	NoteControlReply(name, []byte(`{"system":{"command":"ledctrl","led_mode":"on","reason":"success","result":"success"}}`))
	if ControlBlocked(name) {
		t.Fatal("a light command that worked did not clear the mark")
	}

	// A refusal for another reason is not this problem.
	NoteControlReply(name, []byte(`{"print":{"command":"pause","result":"fail","reason":"busy"}}`))
	if ControlBlocked(name) {
		t.Fatal("a refusal for another reason marked the printer")
	}
	// A result that is not text must not panic or mark.
	NoteControlReply(name, []byte(`{"print":{"command":"pause","result":0,"reason":["x"]}}`))
	if ControlBlocked(name) {
		t.Fatal("a numeric result marked the printer")
	}
}

func TestControlBlocked_ExpiresAndClearsOnNewSession(t *testing.T) {
	const name = "cap-ttl"
	t.Cleanup(func() { clearControlBlocked(name) })

	NoteControlReply(name, []byte(verifyFailed))
	controlBlockedMu.Lock()
	controlBlockedAt[name] = time.Now().Add(-controlBlockedFor - time.Second)
	controlBlockedMu.Unlock()
	if ControlBlocked(name) {
		t.Fatal("an old mark still blocks: a fixed printer could never be tried again")
	}

	NoteControlReply(name, []byte(verifyFailed))
	clearControlBlocked(name) // what a new MQTT session does
	if ControlBlocked(name) {
		t.Fatal("clearControlBlocked left the mark")
	}
}

func TestControlBlocked_ShownInStateOnlyWhileConnected(t *testing.T) {
	const name = "cap-state"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name); clearControlBlocked(name) })

	UpdatePrinterState(name, TelemetryData{Status: "idle"})
	if GetPrinterState(name).ControlBlocked || GetPrintersState()[name].ControlBlocked {
		t.Fatal("state says blocked before a refusal")
	}
	b, _ := json.Marshal(GetPrinterState(name))
	if strings.Contains(string(b), "control_blocked") {
		t.Fatalf("an unblocked printer sends control_blocked: %s", b)
	}

	NoteControlReply(name, []byte(verifyFailed))
	if !GetPrinterState(name).ControlBlocked || !GetPrintersState()[name].ControlBlocked {
		t.Fatal("state does not say blocked after a refusal")
	}
	b, _ = json.Marshal(GetPrinterState(name))
	if !strings.Contains(string(b), `"control_blocked":true`) {
		t.Fatalf("dashboard JSON lacks control_blocked: %s", b)
	}

	// An offline printer shows why it is offline, not what it refused earlier.
	UpdatePrinterState(name, TelemetryData{Status: "disconnected"})
	if GetPrinterState(name).ControlBlocked || GetPrintersState()[name].ControlBlocked {
		t.Fatal("a disconnected printer still says blocked")
	}
}

// The answer arrives through the message handler, and the relay payload FoxTrack
// gets keeps its shape: no control_blocked, alarm text only in message.
func TestHandler_RefusalMarksPrinterAndRelayStaysUnchanged(t *testing.T) {
	const name = "cap-handler"
	RemovePrinterState(name)
	t.Cleanup(func() { RemovePrinterState(name); clearControlBlocked(name) })

	sent := make(chan webhook.RelayPayload, 4)
	prevSend := sendRelay
	sendRelay = func(_, _, _, _ string, p webhook.RelayPayload) error {
		sent <- p
		return nil
	}
	t.Cleanup(func() { sendRelay = prevSend })

	handle := makeHandler(Printer{Name: name, Serial: "TEST", APIKey: "key"})
	handle(nil, fakeMessage{verifyFailed})
	if !ControlBlocked(name) {
		t.Fatal("the handler did not note the refusal")
	}

	handle(nil, fakeMessage{`{"print":{"gcode_state":"RUNNING","mc_percent":40,"hms":[{"attr":201327360,"code":196616},{"attr":201327360,"code":262143}]}}`})
	select {
	case p := <-sent:
		b, _ := json.Marshal(p)
		if strings.Contains(string(b), "control_blocked") {
			t.Fatalf("relay payload carries control_blocked: %s", b)
		}
		var withText, withoutText int
		for _, a := range p.Print.Alarms {
			if a.Message != nil && strings.Contains(strings.ToLower(*a.Message), "spaghetti") {
				withText++
			} else if a.Message == nil {
				withoutText++
			}
		}
		if withText != 1 || withoutText != 1 {
			t.Fatalf("relayed alarms = %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no relay payload sent")
	}
}

func TestConnectProblem(t *testing.T) {
	refusedDial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	timeoutDial := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
	cases := []struct {
		name string
		err  error
		want string // a part of the sentence
	}{
		{"bad access code", packets.ErrorRefusedBadUsernameOrPassword, "refused the access code"},
		{"not authorised", packets.ErrorRefusedNotAuthorised, "refused the access code"},
		{"wrapped access code", fmt.Errorf("connect: %w", packets.ErrorRefusedBadUsernameOrPassword), "refused the access code"},
		{"dial timeout", timeoutDial, "Can't reach the printer at 192.168.1.40"},
		{"paho network error", fmt.Errorf("%w : %w", packets.ErrorNetworkError, refusedDial), "Can't reach the printer at 192.168.1.40"},
		{"our connect timeout", errors.New("connect timed out"), "Can't reach the printer at 192.168.1.40"},
		{"no route", errors.New("dial tcp 192.168.1.40:8883: connect: no route to host"), "Can't reach the printer at 192.168.1.40"},
		{"tls", errors.New("tls: first record does not look like a TLS handshake"), "secure connection"},
		{"tls alert", errors.New("remote error: tls: handshake failure"), "secure connection"},
		{"serial", errBadSerial, "serial number"},
		{"quick hang up wrapped", fmt.Errorf("x: %w", errBadSerial), "serial number"},
		{"dropped", errConnectionLost, "dropped"},
		{"subscribe timeout", errors.New("subscribe to device/X/report timed out"), "did not answer"},
		{"server unavailable", packets.ErrorRefusedServerUnavailable, "could not connect"},
	}
	for _, c := range cases {
		got := connectProblem("192.168.1.40", c.err)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q does not contain %q", c.name, got, c.want)
		}
		if strings.Contains(got, "dial tcp") || strings.Contains(got, "i/o timeout") {
			t.Errorf("%s: raw Go error in %q", c.name, got)
		}
	}
	if got := connectProblem("", timeoutDial); got != "Can't reach the printer. Check that it is on and on the same network." {
		t.Errorf("no address: %q", got)
	}
	if got := connectProblem("192.168.1.40", nil); got != "" {
		t.Errorf("nil error: %q", got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// A real paho client refused at the TCP level lands in the "can't reach" sentence.
func TestConnectProblem_RealRefusedDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens now: the connect is refused at once

	opts := mqtt.NewClientOptions().AddBroker("tcp://" + addr).SetConnectTimeout(2 * time.Second).SetAutoReconnect(false).SetProtocolVersion(4)
	token := mqtt.NewClient(opts).Connect()
	if !token.WaitTimeout(5*time.Second) || token.Error() == nil {
		t.Fatal("connect to a closed port did not fail")
	}
	got := connectProblem("127.0.0.1", token.Error())
	if !strings.Contains(got, "Can't reach the printer at 127.0.0.1") {
		t.Fatalf("real refused dial (%v) became %q", token.Error(), got)
	}
}
