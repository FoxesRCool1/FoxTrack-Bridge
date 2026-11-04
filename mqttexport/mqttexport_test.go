package mqttexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func cfg() config.MQTTExport {
	c, err := Resolve(config.MQTTExport{Enabled: true, Broker: "192.168.1.10", Discovery: true, BridgeID: "abcd1234"})
	if err != nil {
		panic(err)
	}
	return c
}

var bambu = config.Printer{ID: "p1", Name: "Monsieur", Serial: "01P00C580801716", IP: "192.168.1.5", LANCode: "x"}

func str(s string) *string { return &s }

func TestDiscoveryGolden(t *testing.T) {
	tele := &mqttpkg.TelemetryData{Status: "idle", PrinterModel: "P1S"}
	got := map[string]json.RawMessage{}
	for k, v := range Discovery(cfg(), bambu, tele) {
		got[k] = json.RawMessage(v)
	}
	for k, v := range bridgeDiscovery(cfg(), "2.6.0") {
		got[k] = json.RawMessage(v)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(got); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	const path = "testdata/discovery_bambu.golden.json"
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if string(want) != string(b) {
		t.Errorf("discovery payloads changed; if that is intended run go test -update\n got: %s", b)
	}
}

func objects(t *testing.T, m map[string]string) []string {
	t.Helper()
	var out []string
	for topic, payload := range m {
		if !json.Valid([]byte(payload)) {
			t.Fatalf("%s is not JSON: %s", topic, payload)
		}
		out = append(out, strings.Split(topic, "/")[1]) // the component
	}
	sort.Strings(out)
	return out
}

func TestDiscoveryByPrinterKind(t *testing.T) {
	c := cfg()
	count := func(p config.Printer, comp string) int {
		n := 0
		for _, got := range objects(t, Discovery(c, p, nil)) {
			if got == comp {
				n++
			}
		}
		return n
	}
	cloud := bambu
	cloud.Connection = config.ConnectionCloud
	cases := []struct {
		name            string
		p               config.Printer
		buttons, lights int
	}{
		{"bambu lan", bambu, 3, 1},
		{"klipper", config.Printer{ID: "p2", Name: "Voron", MoonrakerURL: "http://v:7125"}, 3, 1},
		{"octoprint", config.Printer{ID: "p3", Name: "Ender", OctoPrintURL: "http://o"}, 3, 0},
		{"bambu cloud", cloud, 0, 1},
		{"home assistant", config.Printer{ID: "p4", Name: "HA", HomeAssistantURL: "http://h"}, 0, 0},
	}
	for _, tc := range cases {
		if n := count(tc.p, "button"); n != tc.buttons {
			t.Errorf("%s: %d buttons, want %d", tc.name, n, tc.buttons)
		}
		if n := count(tc.p, "light"); n != tc.lights {
			t.Errorf("%s: %d lights, want %d", tc.name, n, tc.lights)
		}
		if n := count(tc.p, "sensor"); n != 6 {
			t.Errorf("%s: %d sensors, want 6", tc.name, n)
		}
		if n := count(tc.p, "binary_sensor"); n != 3 {
			t.Errorf("%s: %d binary sensors, want 3", tc.name, n)
		}
	}
}

func TestParseCommand(t *testing.T) {
	c := cfg()
	topic := "foxtrack_bridge/p1/command"
	for _, w := range []string{"pause", "resume", "stop", "light_on", "light_off", " STOP\n"} {
		id, cmd, ok := ParseCommand(c, topic, w, false)
		if !ok || id != "p1" || cmd != strings.ToLower(strings.TrimSpace(w)) {
			t.Errorf("%q -> %q %q %v", w, id, cmd, ok)
		}
	}
	bad := []struct {
		name, topic, payload string
		retained             bool
	}{
		{"retained stop", topic, "stop", true},
		{"unknown word", topic, "home", false},
		{"plug is not offered", topic, "plug_off", false},
		{"other prefix", "other/p1/command", "stop", false},
		{"state topic", "foxtrack_bridge/p1/state", "stop", false},
		{"extra level", "foxtrack_bridge/p1/x/command", "stop", false},
		{"wildcard id", "foxtrack_bridge/+/command", "stop", false},
		{"no id", "foxtrack_bridge//command", "stop", false},
	}
	for _, b := range bad {
		if _, _, ok := ParseCommand(c, b.topic, b.payload, b.retained); ok {
			t.Errorf("%s was accepted", b.name)
		}
	}
}

func TestBuildState(t *testing.T) {
	tele := &mqttpkg.TelemetryData{
		Status: "printing", FileName: "benchy.3mf", Progress: 42, NozzleTemp: 209.96, NozzleTarget: 210, BedTemp: 59.04, BedTarget: 60,
		TimeRemaining: 35, LightOn: true, SpeedLevel: 2, PrinterModel: "P1S",
		Alarms: []webhook.RelayAlarm{
			{Source: "hms", Severity: webhook.AlarmInfo, Message: str("just chatter")},
			{Source: "hms", Severity: webhook.AlarmSerious, Message: str("Filament ran out")},
			{Source: "print_error", Severity: webhook.AlarmCommon, Message: str("second")},
		},
	}
	want := State{Status: "printing", Progress: 42, File: "benchy.3mf", NozzleTemp: 210, NozzleTarget: 210, BedTemp: 59, BedTarget: 60,
		TimeRemaining: 35, Light: true, SpeedLevel: 2, Model: "P1S", Alarms: 2, Alarm: "Filament ran out", Online: true}
	if got := BuildState(bambu, tele); got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if got := BuildState(bambu, nil); got.Status != "disconnected" || got.Online || got.Model != "Bambu Lab" {
		t.Errorf("unreported printer: %+v", got)
	}
	if BuildState(bambu, &mqttpkg.TelemetryData{Status: "connected"}).Online {
		t.Error("connected (before the first report) must not count as online")
	}
}

type sent struct {
	topic, payload string
	retain         bool
}

func run(s *syncer, now time.Time, ps []config.Printer, st map[string]*mqttpkg.TelemetryData) []sent {
	var out []sent
	s.step(now, ps, st, func(topic, payload string, retain bool) { out = append(out, sent{topic, payload, retain}) })
	return out
}

func states(progress int) map[string]*mqttpkg.TelemetryData {
	return map[string]*mqttpkg.TelemetryData{"Monsieur": {Status: "printing", Progress: progress, PrinterModel: "P1S"}}
}

func TestSyncDiffing(t *testing.T) {
	c := cfg()
	s := newSyncer(c, "2.6.0")
	now := time.Now()
	ps := []config.Printer{bambu}

	first := run(&s, now, ps, states(10))
	var discovery, state int
	for _, m := range first {
		if !m.retain {
			t.Errorf("%s was not retained", m.topic)
		}
		switch {
		case strings.HasSuffix(m.topic, "/config"):
			discovery++
		case m.topic == "foxtrack_bridge/p1/state":
			state++
		}
	}
	if discovery != len(Discovery(c, bambu, nil))+1 || state != 1 {
		t.Fatalf("first step sent %d configs and %d states", discovery, state)
	}

	if again := run(&s, now.Add(3*time.Second), ps, states(10)); len(again) != 0 {
		t.Errorf("nothing changed but %d messages went out: %v", len(again), again)
	}

	changed := run(&s, now.Add(6*time.Second), ps, states(11))
	if len(changed) != 1 || changed[0].topic != "foxtrack_bridge/p1/state" || !strings.Contains(changed[0].payload, `"progress":11`) {
		t.Errorf("a progress change should send only the state: %v", changed)
	}

	if resend := run(&s, now.Add(6*time.Second+refreshEvery), ps, states(11)); len(resend) != 1 {
		t.Errorf("an unchanged state is resent after a minute, got %v", resend)
	}

	// The printer is deleted: its configs and its retained state are emptied.
	gone := run(&s, now.Add(5*time.Minute), nil, nil)
	if len(gone) != len(Discovery(c, bambu, nil))+1 {
		t.Errorf("deleting the printer sent %d messages: %v", len(gone), gone)
	}
	for _, m := range gone {
		if m.payload != "" || !m.retain {
			t.Errorf("%s should be an empty retained message, got %q", m.topic, m.payload)
		}
	}
	for _, m := range gone {
		if strings.Contains(m.topic, "/bridge/config") {
			t.Error("the bridge's own device must stay while discovery is on")
		}
	}
}

func TestSyncLearnsFromTheBroker(t *testing.T) {
	c := cfg()
	s := newSyncer(c, "2.6.0")
	now := time.Now()
	ps := []config.Printer{bambu}
	// What an earlier run left retained: this printer's configs (identical),
	// and one for a printer that has since been deleted.
	for k, v := range Discovery(c, bambu, states(0)["Monsieur"]) {
		s.known[k] = v
	}
	for k, v := range bridgeDiscovery(c, "2.6.0") {
		s.known[k] = v
	}
	stale := c.DiscoveryPrefix + "/sensor/" + nodeID(c) + "/deleted_status/config"
	s.known[stale] = "{}"

	out := run(&s, now, ps, states(0))
	for _, m := range out {
		if strings.HasSuffix(m.topic, "/config") && m.topic != stale {
			t.Errorf("an identical config was sent again: %s", m.topic)
		}
	}
	found := false
	for _, m := range out {
		if m.topic == stale {
			found = m.payload == ""
		}
	}
	if !found {
		t.Error("the config of a deleted printer was not removed")
	}
}

func TestSyncDiscoveryOffClearsEverything(t *testing.T) {
	c := cfg()
	c.Discovery = false
	s := newSyncer(c, "2.6.0")
	topics := []string{}
	for k := range Discovery(cfg(), bambu, nil) {
		s.known[k] = "{}"
		topics = append(topics, k)
	}
	out := run(&s, time.Now(), []config.Printer{bambu}, nil)
	cleared := 0
	for _, m := range out {
		if strings.HasSuffix(m.topic, "/config") {
			if m.payload != "" {
				t.Errorf("discovery is off but %s was published", m.topic)
			}
			cleared++
		}
	}
	if cleared != len(topics) {
		t.Errorf("cleared %d of %d configs", cleared, len(topics))
	}
}

func TestSyncSkipsUnsafePrinterIDs(t *testing.T) {
	s := newSyncer(cfg(), "2.6.0")
	bad := config.Printer{ID: "a/b", Name: "Odd"}
	for _, m := range run(&s, time.Now(), []config.Printer{bad}, nil) {
		if strings.Contains(m.topic, "a/b") {
			t.Errorf("a printer ID with a slash reached a topic: %s", m.topic)
		}
	}
}

func TestEventJSON(t *testing.T) {
	e := events.Event{Kind: events.PrintFinished, Time: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), PrinterID: "p1", Printer: "Monsieur", File: "a.3mf", Progress: 100,
		Record: &history.Record{Duration: 3600}}
	var got map[string]any
	if err := json.Unmarshal(EventJSON(e), &got); err != nil {
		t.Fatal(err)
	}
	if got["event"] != "print_finished" || got["time"] != "2026-05-01T12:00:00Z" || got["progress"] != float64(100) || got["duration_seconds"] != float64(3600) {
		t.Errorf("got %v", got)
	}
	e.Progress = -1
	if strings.Contains(string(EventJSON(e)), "progress") {
		t.Error("an unknown progress must be left out")
	}
	e.Kind = events.Test
	if EventJSON(e) != nil {
		t.Error("the dashboard's test event must not reach the broker")
	}
	e.Kind, e.PrinterID = events.PrintStarted, ""
	if EventJSON(e) != nil {
		t.Error("an event with no printer has no topic")
	}
}

func TestResolve(t *testing.T) {
	good := map[string]string{
		"192.168.1.10":          "tcp://192.168.1.10:1883",
		"broker.local:1884":     "tcp://broker.local:1884",
		"tcp://10.0.0.2":        "tcp://10.0.0.2:1883",
		"mqtt://10.0.0.2:1999":  "tcp://10.0.0.2:1999",
		"ssl://broker.lan":      "ssl://broker.lan:8883",
		"mqtts://broker.lan:99": "ssl://broker.lan:99",
		" tcp://[::1]:1883 ":    "tcp://[::1]:1883",
	}
	for in, want := range good {
		c, err := Resolve(config.MQTTExport{Enabled: true, Broker: in})
		if err != nil || c.Broker != want {
			t.Errorf("%q -> %q, %v; want %q", in, c.Broker, err, want)
		}
	}
	for _, in := range []string{"", "http://x", "ws://x:1", "tcp://", "tcp://:1883"} {
		if _, err := Resolve(config.MQTTExport{Enabled: true, Broker: in}); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
	if _, err := Resolve(config.MQTTExport{}); err != nil {
		t.Errorf("an unused block must resolve: %v", err)
	}
	c, err := Resolve(config.MQTTExport{Enabled: true, Broker: "x", TopicPrefix: "/home/printers/", DiscoveryPrefix: ""})
	if err != nil || c.TopicPrefix != "home/printers" || c.DiscoveryPrefix != "homeassistant" {
		t.Errorf("prefixes: %+v %v", c, err)
	}
	for _, p := range []string{"a/#", "a/+/b", "has space", "a//b"} {
		if _, err := Resolve(config.MQTTExport{TopicPrefix: p}); err == nil {
			t.Errorf("prefix %q was accepted", p)
		}
	}
}

func TestFriendly(t *testing.T) {
	cases := map[string]error{
		"The broker refused the username or password.":                             packets.ErrorRefusedBadUsernameOrPassword,
		"The broker's address could not be found.":                                 errors.New("network Error : dial tcp: lookup nope: no such host"),
		"Nothing is listening at that address and port. Check the broker address.": errors.New("network Error : dial tcp 10.0.0.1:1883: connect: connection refused"),
	}
	for want, in := range cases {
		if got := Friendly(in).Error(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if got := Friendly(errors.New("tls: failed to verify certificate: x509: unknown authority")).Error(); !strings.Contains(got, "skip certificate check") {
		t.Errorf("got %q", got)
	}
}

func TestClientID(t *testing.T) {
	if id := ClientID("abcd1234"); !strings.HasPrefix(id, "foxtrack-bridge-") || len(id) != len("foxtrack-bridge-")+6 {
		t.Errorf("client ID = %q", id)
	}
	if ClientID("abcd1234") == ClientID("efgh5678") {
		t.Error("two Bridge IDs on one host must not share a client ID")
	}
}
