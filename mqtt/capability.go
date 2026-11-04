package mqtt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

// What a Bambu printer lets the Bridge do, and why it cannot connect.

// ControlBlockedMsg is what /api/control and the dashboard card say about a
// printer that refuses control commands.
const ControlBlockedMsg = "This printer refuses control commands. Turn on LAN Only Mode and Developer Mode on the printer (Settings, Network), then try again."

// controlBlockedFor is how long a refusal counts. Nothing is sent to a printer
// that is known to refuse, so no reply could ever clear it; after this long the
// next command is tried again. A new MQTT session clears it at once (changing
// the printer's modes restarts its network), and so does any command that works.
const controlBlockedFor = 10 * time.Minute

var (
	controlBlockedAt = map[string]time.Time{} // printer name -> when a command was last refused
	controlBlockedMu sync.Mutex
)

// ControlBlocked reports whether the printer recently answered a command with
// "mqtt message verify failed": current Bambu firmware does that to every
// control command unless the printer is in LAN Only Mode with Developer Mode on.
func ControlBlocked(name string) bool {
	controlBlockedMu.Lock()
	defer controlBlockedMu.Unlock()
	t, ok := controlBlockedAt[name]
	return ok && time.Since(t) < controlBlockedFor
}

func clearControlBlocked(name string) {
	controlBlockedMu.Lock()
	delete(controlBlockedAt, name)
	controlBlockedMu.Unlock()
}

// controlReply is the part of a command's answer that says whether it worked.
// Bambu answers print commands under "print" and light commands under "system".
type controlReply struct {
	Command string `json:"command"`
	Result  any    `json:"result"`
	Reason  any    `json:"reason"`
}

// NoteControlReply reads the printer's answer to a command: a refusal with the
// "verify failed" reason marks the printer as blocked, a success clears it.
// Reports and other answers (no result, or a failure for another reason)
// change nothing.
func NoteControlReply(name string, payload []byte) {
	if !bytes.Contains(payload, []byte(`"result"`)) {
		return // every report passes through here: skip the parse for the ones that are no answer
	}
	var probe struct {
		Print  controlReply `json:"print"`
		System controlReply `json:"system"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return
	}
	for _, r := range []controlReply{probe.Print, probe.System} {
		if r.Command == "" || r.Result == nil {
			continue
		}
		switch res := strings.ToLower(fmt.Sprint(r.Result)); {
		case res == "success":
			clearControlBlocked(name)
		case (res == "fail" || res == "failed") && r.Reason != nil &&
			strings.Contains(strings.ToLower(fmt.Sprint(r.Reason)), "verify failed"):
			controlBlockedMu.Lock()
			controlBlockedAt[name] = time.Now()
			controlBlockedMu.Unlock()
		}
	}
}

// Why a connection failed. connectAndListen returns these two itself.
var (
	errConnectionLost = errors.New("connection lost")
	// errBadSerial: the broker refused the subscription, or hung up right after
	// login before sending a single report. The printer closes the link on a
	// serial number it does not own.
	errBadSerial = errors.New("the printer closed the connection right after login")
)

// connectProblem words, for the printer card, why the Bridge could not connect
// or keep its connection to the printer at ip. The wording follows the
// connection test (probe.go). err nil gives "". Anything it cannot place gets
// a general sentence; the raw error is in the log.
func connectProblem(ip string, err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	var ne net.Error
	switch {
	case errors.Is(err, packets.ErrorRefusedBadUsernameOrPassword), errors.Is(err, packets.ErrorRefusedNotAuthorised):
		return "The printer refused the access code. Check it on the printer screen (Settings, Network) and edit the printer."
	case errors.Is(err, errBadSerial):
		return "The printer closed the connection right after login. Check the serial number on the printer screen (Settings, Device) and edit the printer."
	case errors.Is(err, errConnectionLost):
		return "The connection to the printer dropped. Bridge is reconnecting."
	case strings.Contains(msg, "subscribe to "):
		return "The printer accepted the login but did not answer. Bridge keeps trying."
	case errors.As(err, &ne) && ne.Timeout(),
		containsAny(msg, "timed out", "timeout", "deadline exceeded", "connection refused", "no route to host", "unreachable", "host is down", "no such host", "dial tcp"):
		if ip == "" {
			return "Can't reach the printer. Check that it is on and on the same network."
		}
		return fmt.Sprintf("Can't reach the printer at %s. Check that it is on and on the same network.", ip)
	case containsAny(msg, "tls:", "x509", "certificate", "handshake"):
		return "The secure connection to the printer failed. Check that the IP address is the printer's, or restart the printer."
	}
	return "Bridge could not connect to the printer. It keeps trying."
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
