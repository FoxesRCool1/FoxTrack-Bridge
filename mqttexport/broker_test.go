package mqttexport

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	mqttpkg "foxtrack-bridge/mqtt"
)

// fakeBroker speaks just enough MQTT 3.1.1 for the exporter: connect (with a
// username and password check), subscribe, publish, ping and disconnect.
type fakeBroker struct {
	ln         net.Listener
	user, pass string

	mu        sync.Mutex
	conn      net.Conn
	wmu       sync.Mutex
	clientIDs []string
	subs      []string
	got       []sent
}

func newBroker(t *testing.T, user, pass string) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBroker{ln: ln, user: user, pass: pass}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(c)
		}
	}()
	return b
}

func (b *fakeBroker) addr() string { return "tcp://" + b.ln.Addr().String() }

func (b *fakeBroker) write(c net.Conn, p packets.ControlPacket) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_ = p.Write(c)
}

func (b *fakeBroker) serve(c net.Conn) {
	defer c.Close()
	for {
		pkt, err := packets.ReadPacket(c)
		if err != nil {
			return
		}
		switch p := pkt.(type) {
		case *packets.ConnectPacket:
			ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
			if b.user != "" && (p.Username != b.user || string(p.Password) != b.pass) {
				ack.ReturnCode = packets.ErrRefusedBadUsernameOrPassword
				b.write(c, ack)
				return
			}
			b.mu.Lock()
			b.conn = c
			b.clientIDs = append(b.clientIDs, p.ClientIdentifier)
			b.mu.Unlock()
			b.write(c, ack)
		case *packets.SubscribePacket:
			b.mu.Lock()
			b.subs = append(b.subs, p.Topics...)
			b.mu.Unlock()
			ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
			ack.MessageID, ack.ReturnCodes = p.MessageID, p.Qoss
			b.write(c, ack)
		case *packets.PublishPacket:
			b.mu.Lock()
			b.got = append(b.got, sent{p.TopicName, string(p.Payload), p.Retain})
			b.mu.Unlock()
			if p.Qos == 1 {
				ack := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
				ack.MessageID = p.MessageID
				b.write(c, ack)
			}
		case *packets.PingreqPacket:
			b.write(c, packets.NewControlPacket(packets.Pingresp))
		case *packets.DisconnectPacket:
			return
		}
	}
}

// inject delivers a message to the connected client, as the broker would to a
// subscriber.
func (b *fakeBroker) inject(topic, payload string, retained bool) {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()
	p := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	p.TopicName, p.Payload, p.Retain = topic, []byte(payload), retained
	b.write(c, p)
}

func (b *fakeBroker) has(topic, payload string, retain bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.got {
		if m.topic == topic && m.payload == payload && m.retain == retain {
			return true
		}
	}
	return false
}

func (b *fakeBroker) hasTopic(topic string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.got {
		if m.topic == topic && m.payload != "" {
			return true
		}
	}
	return false
}

func (b *fakeBroker) subscribedTo(topic string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		if s == topic {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTestReportsPlainErrors(t *testing.T) {
	b := newBroker(t, "fox", "secret")
	c, _ := Resolve(config.MQTTExport{Enabled: true, Broker: b.addr(), Username: "fox", Password: "wrong"})
	if err := Test(c); err == nil || err.Error() != "The broker refused the username or password." {
		t.Errorf("wrong password: %v", err)
	}
	c.Password = "secret"
	if err := Test(c); err != nil {
		t.Errorf("right password: %v", err)
	}
	b.mu.Lock()
	ids := append([]string(nil), b.clientIDs...)
	b.mu.Unlock()
	if len(ids) != 1 || ids[0] == ClientID() || !strings.HasPrefix(ids[0], ClientID()) {
		t.Errorf("the test must use its own client ID so it cannot knock off the live one: %v", ids)
	}

	b.ln.Close()
	if err := Test(c); err == nil || !strings.Contains(err.Error(), "Nothing is listening") {
		t.Errorf("closed port: %v", err)
	}
}

func TestExporterEndToEnd(t *testing.T) {
	tickEvery, settleAfter = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { tickEvery, settleAfter = 3*time.Second, 2*time.Second })

	b := newBroker(t, "", "")
	var mu sync.Mutex
	var ran []string
	ex := New(Host{
		Printers: func() []config.Printer { return []config.Printer{bambu} },
		States: func() map[string]*mqttpkg.TelemetryData {
			return map[string]*mqttpkg.TelemetryData{"Monsieur": {Status: "printing", Progress: 7, PrinterModel: "P1S"}}
		},
		Control: func(name, command string) error {
			mu.Lock()
			defer mu.Unlock()
			ran = append(ran, name+" "+command)
			return nil
		},
		Version: "2.6.0",
	})
	c, err := Resolve(config.MQTTExport{Enabled: true, Broker: b.addr(), Discovery: true, BridgeID: "abcd1234"})
	if err != nil {
		t.Fatal(err)
	}
	ex.Apply(c)

	waitFor(t, "the connection", func() bool { ok, _ := ex.Status(); return ok })
	waitFor(t, "availability, state and discovery", func() bool {
		return b.has("foxtrack_bridge/bridge/availability", "online", true) &&
			b.hasTopic("foxtrack_bridge/p1/state") &&
			b.hasTopic("homeassistant/sensor/ftb_abcd1234/p1_status/config")
	})
	if !b.has("foxtrack_bridge/p1/state", `{"status":"printing","progress":7,"file":"","nozzle_temp":0,"nozzle_target":0,"bed_temp":0,"bed_target":0,"time_remaining":0,"light":false,"speed_level":0,"model":"P1S","alarms":0,"alarm":"","online":true}`, true) {
		t.Errorf("state payload not as expected: %v", b.got)
	}

	// Commands run through the host; a retained one never does.
	waitFor(t, "the subscriptions", func() bool {
		return b.subscribedTo("foxtrack_bridge/+/command") && b.subscribedTo("homeassistant/+/ftb_abcd1234/+/config")
	})
	b.inject("foxtrack_bridge/p1/command", "stop", true)
	b.inject("foxtrack_bridge/p1/command", "pause", false)
	waitFor(t, "the pause command", func() bool { mu.Lock(); defer mu.Unlock(); return len(ran) > 0 })
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(ran) != 1 || ran[0] != "Monsieur pause" {
		t.Errorf("ran %v, want only the live pause", ran)
	}
	mu.Unlock()

	// A config left by a printer that no longer exists is removed.
	stale := "homeassistant/sensor/ftb_abcd1234/gone_status/config"
	b.inject(stale, `{"name":"old"}`, true)
	waitFor(t, "the stale config to be cleared", func() bool { return b.has(stale, "", true) })

	// Events are published, not retained.
	ex.OnEvent(events.Event{Kind: events.PrintFinished, PrinterID: "p1", Printer: "Monsieur", Progress: 100, Time: time.Unix(0, 0)})
	waitFor(t, "the event", func() bool {
		return b.has("foxtrack_bridge/p1/event", `{"event":"print_finished","time":"1970-01-01T00:00:00Z","printer":"Monsieur","progress":100}`, false)
	})

	// Turning the export off takes the printers out of Home Assistant.
	c.Enabled = false
	ex.Apply(c)
	waitFor(t, "discovery to be removed", func() bool { return b.has("homeassistant/sensor/ftb_abcd1234/p1_status/config", "", true) })
	waitFor(t, "offline", func() bool { return b.has("foxtrack_bridge/bridge/availability", "offline", true) })
	if ok, _ := ex.Status(); ok {
		t.Error("still connected after turning it off")
	}
}

func TestExporterReportsAnUnreachableBroker(t *testing.T) {
	b := newBroker(t, "", "")
	b.ln.Close()
	ex := New(Host{})
	c, _ := Resolve(config.MQTTExport{Enabled: true, Broker: b.addr()})
	ex.Apply(c)
	defer ex.Apply(config.MQTTExport{})
	waitFor(t, "the error", func() bool { _, e := ex.Status(); return strings.Contains(e, "Nothing is listening") })
}
