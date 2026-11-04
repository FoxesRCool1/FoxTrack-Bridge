package mqttexport

import (
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	mqttpkg "foxtrack-bridge/mqtt"
)

const refreshEvery = 60 * time.Second // a state is resent at least this often

var (
	tickEvery   = 3 * time.Second // how often printers are compared with what was published
	settleAfter = 2 * time.Second // after connecting, wait for the broker's retained discovery configs
)

// Host is what the exporter needs from the Bridge.
type Host struct {
	Printers func() []config.Printer
	States   func() map[string]*mqttpkg.TelemetryData // by printer name
	Control  func(printerName, command string) error  // the same path as /api/control
	Version  string
}

// Exporter runs one broker connection for the current settings.
type Exporter struct {
	host    Host
	applyMu sync.Mutex // one Apply at a time; only Apply changes cur
	mu      sync.Mutex // guards cur for Status and OnEvent
	cur     *session
}

func New(h Host) *Exporter { return &Exporter{host: h} }

// Apply starts, restarts or stops the export for c (already Resolved).
func (e *Exporter) Apply(c config.MQTTExport) {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()
	if old := e.cur; old != nil {
		if old.cfg == c {
			return
		}
		e.mu.Lock()
		e.cur = nil
		e.mu.Unlock()
		// Take the printers out of Home Assistant only when they would be
		// left behind: discovery turned off, or a different broker or prefix.
		// A new topic prefix also leaves the old one's retained states behind.
		// This runs outside e.mu: closing can take seconds.
		old.close(
			old.cfg.Discovery && (!c.Enabled || !c.Discovery || c.Broker != old.cfg.Broker || c.DiscoveryPrefix != old.cfg.DiscoveryPrefix),
			c.Enabled && c.TopicPrefix != old.cfg.TopicPrefix,
		)
	}
	if c.Enabled {
		s := e.start(c)
		e.mu.Lock()
		e.cur = s
		e.mu.Unlock()
	}
}

// Status says whether the broker is connected and, if not, why.
func (e *Exporter) Status() (connected bool, errText string) {
	e.mu.Lock()
	s := e.cur
	e.mu.Unlock()
	if s == nil {
		return false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected, s.err
}

// OnEvent publishes an event for automations to react to. Not retained.
func (e *Exporter) OnEvent(ev events.Event) {
	body := EventJSON(ev)
	e.mu.Lock()
	s := e.cur
	e.mu.Unlock()
	if body == nil || s == nil || !s.client.IsConnectionOpen() {
		return
	}
	s.client.Publish(eventTopic(s.cfg, ev.PrinterID), 1, false, body)
}

type session struct {
	host   Host
	cfg    config.MQTTExport
	client paho.Client
	stop   chan struct{}
	done   chan struct{}

	mu        sync.Mutex
	connected bool
	err       string
	readyAt   time.Time
	syn       syncer
}

func (e *Exporter) start(c config.MQTTExport) *session {
	s := &session{host: e.host, cfg: c, stop: make(chan struct{}), done: make(chan struct{}), syn: newSyncer(c, e.host.Version)}
	o := options(c, ClientID(c.BridgeID)).
		SetAutoReconnect(true).SetMaxReconnectInterval(time.Minute).
		SetWill(availabilityTopic(c), "offline", 1, true).
		SetOnConnectHandler(s.onConnect).
		SetConnectionLostHandler(s.onLost)
	s.client = paho.NewClient(o)
	go s.run()
	return s
}

// run keeps trying to connect, backing off, then compares printers with what
// the broker has every few seconds. paho reconnects by itself after a drop.
func (s *session) run() {
	defer close(s.done)
	for wait := time.Second; ; wait = min(wait*2, time.Minute) {
		tok := s.client.Connect()
		select {
		case <-tok.Done():
		case <-s.stop:
			go func() { // a connect still in flight must not be left open
				<-tok.Done()
				if tok.Error() == nil {
					s.client.Disconnect(0)
				}
			}()
			return
		}
		err := tok.Error()
		if err == nil {
			break
		}
		s.setErr(Friendly(err).Error())
		select {
		case <-time.After(wait):
		case <-s.stop:
			return
		}
	}
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			s.tick(now)
		case <-s.stop:
			return
		}
	}
}

func (s *session) setErr(msg string) {
	s.mu.Lock()
	s.connected, s.err = false, msg
	s.mu.Unlock()
}

func (s *session) onLost(_ paho.Client, err error) {
	log.Printf("[mqtt-export] lost the connection to the broker: %v", err)
	s.setErr("The connection to the broker was lost. Trying again.")
}

func (s *session) onConnect(c paho.Client) {
	s.mu.Lock()
	s.connected, s.err = true, ""
	s.syn = newSyncer(s.cfg, s.host.Version) // the broker may have lost everything
	s.readyAt = time.Now().Add(settleAfter)
	s.mu.Unlock()
	c.Publish(availabilityTopic(s.cfg), 1, true, "online")
	c.Subscribe(commandFilter(s.cfg), 1, s.onCommand)
	c.Subscribe(sweepFilter(s.cfg), 1, s.onConfig)
}

// onConfig learns which discovery configs are already on the broker, so ones
// left by a deleted printer (even before a restart) can be removed.
func (s *session) onConfig(_ paho.Client, m paho.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(m.Payload()) == 0 {
		delete(s.syn.known, m.Topic())
	} else {
		s.syn.known[m.Topic()] = string(m.Payload())
	}
}

func (s *session) onCommand(_ paho.Client, m paho.Message) {
	id, cmd, ok := ParseCommand(s.cfg, m.Topic(), string(m.Payload()), m.Retained())
	if !ok {
		return
	}
	go func() { // control can take seconds; do not hold up the broker connection
		for _, p := range s.host.Printers() {
			if p.ID == id {
				if err := s.host.Control(p.Name, cmd); err != nil {
					log.Printf("[mqtt-export] %s %s: %v", p.Name, cmd, err)
				}
				return
			}
		}
		log.Printf("[mqtt-export] command %q for an unknown printer", cmd)
	}()
}

func (s *session) tick(now time.Time) {
	printers, states := s.host.Printers(), s.host.States()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected || now.Before(s.readyAt) {
		return
	}
	s.syn.step(now, printers, states, func(topic, payload string, retain bool) {
		s.client.Publish(topic, 1, retain, payload)
	})
}

// close hangs up. With clear it first removes this Bridge's discovery configs,
// with clearStates each printer's retained state.
// A clean hang-up does not fire the last will, so "offline" is sent here.
func (s *session) close(clear, clearStates bool) {
	close(s.stop)
	<-s.done
	if s.client.IsConnectionOpen() {
		s.mu.Lock()
		if clear {
			for t := range s.syn.known {
				s.client.Publish(t, 1, true, "")
			}
		}
		if clearStates {
			for id := range s.syn.last {
				s.client.Publish(stateTopic(s.cfg, id), 1, true, "")
			}
		}
		s.mu.Unlock()
		s.client.Publish(availabilityTopic(s.cfg), 1, true, "offline").WaitTimeout(2 * time.Second)
	}
	s.client.Disconnect(500)
}

// syncer remembers what is on the broker and works out what to send. It has
// no connection of its own, so it can be tested.
type syncer struct {
	cfg     config.MQTTExport
	version string
	known   map[string]string // discovery topic -> payload on the broker
	last    map[string]string // printer ID -> last state sent
	lastAt  map[string]time.Time
}

func newSyncer(c config.MQTTExport, version string) syncer {
	return syncer{cfg: c, version: version, known: map[string]string{}, last: map[string]string{}, lastAt: map[string]time.Time{}}
}

// step sends what changed: discovery configs that are new or different, an
// empty config for each one no longer wanted, and each printer's state when
// it differs from the last or the last is a minute old.
func (s *syncer) step(now time.Time, printers []config.Printer, states map[string]*mqttpkg.TelemetryData, pub func(topic, payload string, retain bool)) {
	desired := map[string]string{}
	if s.cfg.Discovery {
		desired = bridgeDiscovery(s.cfg, s.version)
		for _, p := range printers {
			if safeID(p.ID) {
				for t, c := range Discovery(s.cfg, p, states[p.Name]) {
					desired[t] = c
				}
			}
		}
	}
	for _, t := range sortedKeys(desired) {
		if s.known[t] != desired[t] {
			pub(t, desired[t], true)
			s.known[t] = desired[t]
		}
	}
	for _, t := range sortedKeys(s.known) {
		if _, ok := desired[t]; !ok {
			pub(t, "", true)
			delete(s.known, t)
		}
	}

	seen := map[string]bool{}
	for _, p := range printers {
		if !safeID(p.ID) {
			continue
		}
		seen[p.ID] = true
		b, _ := json.Marshal(BuildState(p, states[p.Name]))
		if s.last[p.ID] == string(b) && now.Sub(s.lastAt[p.ID]) < refreshEvery {
			continue
		}
		pub(stateTopic(s.cfg, p.ID), string(b), true)
		s.last[p.ID], s.lastAt[p.ID] = string(b), now
	}
	for id := range s.last { // a deleted printer's retained state goes too
		if !seen[id] {
			pub(stateTopic(s.cfg, id), "", true)
			delete(s.last, id)
			delete(s.lastAt, id)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
