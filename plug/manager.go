package plug

import (
	"context"
	"errors"
	"log"
	"math"
	"sync"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/webhook"
)

// Manager polls every configured plug every 10 s, keeps the last reading for
// the relay payload and the dashboard, turns the plug off after a print when
// asked, and counts the energy each print used.
//
// Printer status (printing or not, nozzle temperature) comes from a function
// the server installs: the mqtt and lan packages own that state and this
// package must not import them.
type Manager struct {
	mu      sync.Mutex
	plugs   map[string]*entry // by printer name
	status  func(name string) (inPrint bool, nozzle float64, ok bool)
	now     func() time.Time
	read    func(ctx context.Context, g config.Plug) (Reading, error)
	switchF func(ctx context.Context, g config.Plug, on bool) error
	stopAll chan struct{}
}

type entry struct {
	cfg  config.Plug
	stop chan struct{}

	// last reading
	on        bool
	watts     *float64
	energyWh  *float64
	reachable bool
	readAt    time.Time
	warned    bool // an outage was logged and has not ended

	// energy over the current or last print
	inPrint     bool
	startWh     *float64 // counter at print start, when the plug has one
	sumWh       float64  // watts x time, the fallback
	lastSampleT time.Time
	lastWatts   float64
	printWh     *float64 // the finished print's use, until the history sender takes it
	printEndAt  time.Time

	// auto off
	pendingOff time.Time // zero while no print has just ended
}

// Poll is how often a plug is read.
const Poll = 10 * time.Second

// coolNozzleC is the nozzle temperature under which a plug may be cut.
const coolNozzleC = 50.0

var (
	ErrPrinting = errors.New("The printer is printing. Stop the print first.")
	ErrHot      = errors.New("The hotend is still hot. Try again in a few minutes.")
	ErrNoPlug   = errors.New("This printer has no smart plug set in the Bridge.")
)

// Default is the manager the server uses.
var Default = NewManager()

func NewManager() *Manager {
	return &Manager{
		plugs:   map[string]*entry{},
		now:     time.Now,
		read:    Read,
		switchF: Switch,
	}
}

// SetStatusSource installs the printer status lookup.
func (m *Manager) SetStatusSource(fn func(name string) (inPrint bool, nozzle float64, ok bool)) {
	m.mu.Lock()
	m.status = fn
	m.mu.Unlock()
}

// Sync starts a poller for every printer with a plug and stops the rest. A
// printer whose plug settings did not change keeps its poller and its print
// energy count.
func (m *Manager) Sync(printers []config.Printer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := map[string]bool{}
	for _, p := range printers {
		if !p.HasPlug() {
			continue
		}
		keep[p.Name] = true
		if e, ok := m.plugs[p.Name]; ok {
			if e.cfg == *p.Plug {
				continue
			}
			close(e.stop)
			// Same plug address: keep the readings and the print count.
			if e.cfg.Address == p.Plug.Address && e.cfg.Kind == p.Plug.Kind {
				e.cfg = *p.Plug
				e.stop = make(chan struct{})
				go m.loop(p.Name, e.stop)
				continue
			}
		}
		e := &entry{cfg: *p.Plug, stop: make(chan struct{})}
		m.plugs[p.Name] = e
		go m.loop(p.Name, e.stop)
	}
	for name, e := range m.plugs {
		if !keep[name] {
			close(e.stop)
			delete(m.plugs, name)
		}
	}
}

// Current is the plug state for the relay payload and the dashboard, nil for
// a printer without a plug.
func (m *Manager) Current(name string) *webhook.RelayPlug {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.plugs[name]
	if !ok {
		return nil
	}
	return &webhook.RelayPlug{On: e.on, Watts: e.watts, Kind: e.cfg.Kind, AutoOff: e.cfg.AutoOff, Reachable: e.reachable}
}

// Has reports whether the printer has a plug.
func (m *Manager) Has(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.plugs[name]
	return ok
}

// Set turns a printer's plug on or off. Off is refused while the printer
// prints, is paused, or is still hot: the app disables the button then too,
// but the Bridge is the last word.
func (m *Manager) Set(ctx context.Context, name string, on bool) error {
	m.mu.Lock()
	e, ok := m.plugs[name]
	if !ok {
		m.mu.Unlock()
		return ErrNoPlug
	}
	cfg := e.cfg
	status := m.status
	m.mu.Unlock()
	if !on && status != nil {
		if inPrint, nozzle, ok := status(name); ok {
			if inPrint {
				return ErrPrinting
			}
			if nozzle > coolNozzleC {
				return ErrHot
			}
		}
	}
	if err := m.switchF(ctx, cfg, on); err != nil {
		return err
	}
	m.mu.Lock()
	if e, ok := m.plugs[name]; ok {
		e.on = on
		e.reachable = true
		e.pendingOff = time.Time{} // a hand switch ends any auto-off wait
	}
	m.mu.Unlock()
	m.poll(name) // read the meter right away so the app sees the change
	return nil
}

// PrintEnergy returns what the plug measured over the print that just ended,
// in Wh to one decimal, and forgets it. nil for a printer without a plug or
// without a usable reading. Called by the history senders right when a print
// ends, so a print the poller has not yet seen end is closed here.
func (m *Manager) PrintEnergy(name string) *float64 {
	m.mu.Lock()
	e, ok := m.plugs[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	if e.inPrint {
		cfg := e.cfg
		m.mu.Unlock()
		r, err := m.read(context.Background(), cfg)
		m.mu.Lock()
		if e2, ok := m.plugs[name]; ok && e2 == e {
			if err == nil {
				m.applyReading(e, r)
			}
			m.endPrint(e)
		}
	}
	out := e.printWh
	e.printWh = nil
	m.mu.Unlock()
	return out
}

// Test reads one plug once, for the dashboard's connection check.
func Test(ctx context.Context, g config.Plug) (Reading, error) {
	return Read(ctx, g)
}

func (m *Manager) loop(name string, stop <-chan struct{}) {
	m.poll(name)
	t := time.NewTicker(Poll)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.poll(name)
		}
	}
}

// poll reads the plug once and runs the print accounting and auto-off step.
func (m *Manager) poll(name string) {
	m.mu.Lock()
	e, ok := m.plugs[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	cfg := e.cfg
	status := m.status
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout+time.Second)
	r, err := m.read(ctx, cfg)
	cancel()

	inPrint, nozzle, haveStatus := false, 0.0, false
	if status != nil {
		inPrint, nozzle, haveStatus = status(name)
	}

	m.mu.Lock()
	e, ok = m.plugs[name]
	if !ok || e.cfg != cfg {
		m.mu.Unlock()
		return
	}
	if err != nil {
		if !e.warned {
			log.Printf("[plug/%s] %v", name, err)
			e.warned = true // once per outage, not every 10 s
		}
		e.reachable = false
	} else {
		if e.warned {
			log.Printf("[plug/%s] plug is answering again", name)
			e.warned = false
		}
		m.applyReading(e, r)
	}

	turnOff := false
	if haveStatus {
		switch {
		case inPrint && !e.inPrint:
			m.beginPrint(e)
		case !inPrint && e.inPrint:
			m.endPrint(e)
		}
		if !e.pendingOff.IsZero() && cfg.AutoOff {
			switch {
			case inPrint:
				e.pendingOff = time.Time{}
			case m.now().Sub(e.pendingOff) >= cfg.Delay() && nozzle < coolNozzleC && e.on && e.reachable:
				turnOff = true
				e.pendingOff = time.Time{}
			}
		}
	}
	m.mu.Unlock()

	if turnOff {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout+time.Second)
		err := m.switchF(ctx, cfg, false)
		cancel()
		if err != nil {
			log.Printf("[plug/%s] auto off failed: %v", name, err)
			return
		}
		log.Printf("[plug/%s] plug turned off after the print (auto off, %d min idle, nozzle %.0f C)", name, int(cfg.Delay().Minutes()), nozzle)
		m.mu.Lock()
		if e, ok := m.plugs[name]; ok {
			e.on = false
		}
		m.mu.Unlock()
	}
}

// applyReading stores one reading and adds to the running watts x time sum.
// Caller holds m.mu.
func (m *Manager) applyReading(e *entry, r Reading) {
	now := m.now()
	if e.inPrint && !e.lastSampleT.IsZero() {
		dt := now.Sub(e.lastSampleT).Hours()
		if dt > 0 && dt < 1 { // a gap over an hour is a sleeping laptop, not a print
			e.sumWh += e.lastWatts * dt
		}
	}
	e.on, e.watts, e.energyWh, e.reachable, e.readAt = r.On, r.Watts, r.EnergyWh, true, now
	e.lastSampleT = now
	if r.Watts != nil {
		e.lastWatts = *r.Watts
	}
}

// beginPrint marks the counters at the start of a print. Caller holds m.mu.
// ponytail: the poll runs every 10 s, so up to 10 s of heating is missed; a
// start hook from the pollers would close that if anyone needs it.
func (m *Manager) beginPrint(e *entry) {
	e.inPrint = true
	e.startWh = nil
	if e.reachable && e.energyWh != nil {
		v := *e.energyWh
		e.startWh = &v
	}
	e.sumWh = 0
	e.lastSampleT = m.now()
	e.pendingOff = time.Time{}
	e.printWh = nil
}

// endPrint closes the print's energy count and starts the auto-off wait.
// Caller holds m.mu.
func (m *Manager) endPrint(e *entry) {
	e.inPrint = false
	wh := e.sumWh
	if e.startWh != nil && e.energyWh != nil && e.reachable && *e.energyWh >= *e.startWh {
		wh = *e.energyWh - *e.startWh
	}
	wh = math.Round(wh*10) / 10
	e.printWh = &wh
	e.printEndAt = m.now()
	e.pendingOff = m.now()
}
