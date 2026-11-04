package mqttexport

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

// State is what is published for one printer on <prefix>/<printer id>/state.
type State struct {
	Status        string  `json:"status"`
	Progress      int     `json:"progress"`
	File          string  `json:"file"`
	NozzleTemp    float64 `json:"nozzle_temp"`
	NozzleTarget  float64 `json:"nozzle_target"`
	BedTemp       float64 `json:"bed_temp"`
	BedTarget     float64 `json:"bed_target"`
	TimeRemaining int     `json:"time_remaining"` // minutes
	Light         bool    `json:"light"`
	SpeedLevel    int     `json:"speed_level"`
	Model         string  `json:"model"`
	Alarms        int     `json:"alarms"`
	Alarm         string  `json:"alarm"` // text of the first alarm, empty without one
	Online        bool    `json:"online"`
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// Reporting says whether a printer's status means it is talking to the Bridge.
func Reporting(status string) bool {
	return status != "" && status != "disconnected" && status != "connected"
}

// CountAlarms counts the alarms that are real problems (info-level ones are
// status chatter) and returns the text of the first.
func CountAlarms(as []webhook.RelayAlarm) (n int, first string) {
	for _, a := range as {
		if a.Severity == webhook.AlarmInfo {
			continue
		}
		n++
		if first == "" {
			switch {
			case a.Message != nil && *a.Message != "":
				first = *a.Message
			case a.Code != nil:
				first = *a.Code
			}
		}
	}
	return n, first
}

// BuildState turns a printer's live telemetry into its published state. A
// printer that has not reported (t nil) is published as disconnected.
func BuildState(p config.Printer, t *mqttpkg.TelemetryData) State {
	if t == nil {
		return State{Status: "disconnected", Progress: 0, Model: modelOf(p, nil)}
	}
	n, first := CountAlarms(t.Alarms)
	return State{
		Status: t.Status, Progress: t.Progress, File: t.FileName,
		NozzleTemp: round1(t.NozzleTemp), NozzleTarget: round1(t.NozzleTarget),
		BedTemp: round1(t.BedTemp), BedTarget: round1(t.BedTarget),
		TimeRemaining: t.TimeRemaining, Light: t.LightOn, SpeedLevel: t.SpeedLevel,
		Model: modelOf(p, t), Alarms: n, Alarm: first, Online: Reporting(t.Status),
	}
}

func modelOf(p config.Printer, t *mqttpkg.TelemetryData) string {
	if t != nil && t.PrinterModel != "" {
		return t.PrinterModel
	}
	return p.KindLabel()
}

// Topics. The printer ID is hex, so it is safe in a topic; safeID guards the
// odd hand-edited one.
func availabilityTopic(c config.MQTTExport) string { return c.TopicPrefix + "/bridge/availability" }
func stateTopic(c config.MQTTExport, id string) string {
	return c.TopicPrefix + "/" + id + "/state"
}
func eventTopic(c config.MQTTExport, id string) string {
	return c.TopicPrefix + "/" + id + "/event"
}
func commandTopic(c config.MQTTExport, id string) string {
	return c.TopicPrefix + "/" + id + "/command"
}
func commandFilter(c config.MQTTExport) string { return c.TopicPrefix + "/+/command" }

// sweepFilter matches every discovery config this Bridge ever published.
func sweepFilter(c config.MQTTExport) string {
	return c.DiscoveryPrefix + "/+/" + nodeID(c) + "/+/config"
}

func nodeID(c config.MQTTExport) string { return "ftb_" + c.BridgeID }

func safeID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// commands are the words a broker may send on <prefix>/<printer id>/command.
var commands = map[string]bool{"pause": true, "resume": true, "stop": true, "light_on": true, "light_off": true}

// ParseCommand reads a message on the command topic. It refuses retained
// messages: a retained "stop" would run again on every reconnect.
func ParseCommand(c config.MQTTExport, topic, payload string, retained bool) (printerID, command string, ok bool) {
	rest, found := strings.CutPrefix(topic, c.TopicPrefix+"/")
	if !found || retained {
		return "", "", false
	}
	id, suffix, found := strings.Cut(rest, "/")
	if !found || suffix != "command" || !safeID(id) {
		return "", "", false
	}
	command = strings.ToLower(strings.TrimSpace(payload))
	if !commands[command] {
		return "", "", false
	}
	return id, command, true
}

// EventPayload is what is published for an event on <prefix>/<printer id>/event.
type EventPayload struct {
	Event           string `json:"event"`
	Time            string `json:"time"`
	Printer         string `json:"printer"`
	File            string `json:"file,omitempty"`
	Progress        *int   `json:"progress,omitempty"`
	Message         string `json:"message,omitempty"`
	Code            string `json:"code,omitempty"`
	Severity        string `json:"severity,omitempty"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
}

// EventJSON is the body for e, or nil when the event is not published.
func EventJSON(e events.Event) []byte {
	if e.Kind == events.Test || !safeID(e.PrinterID) {
		return nil
	}
	p := EventPayload{Event: string(e.Kind), Time: e.Time.UTC().Format(time.RFC3339), Printer: e.Printer,
		File: e.File, Message: e.Message, Code: e.Code, Severity: e.Severity}
	if e.Progress >= 0 {
		p.Progress = &e.Progress
	}
	if e.Record != nil {
		p.DurationSeconds = e.Record.Duration
	}
	b, _ := json.Marshal(p)
	return b
}
