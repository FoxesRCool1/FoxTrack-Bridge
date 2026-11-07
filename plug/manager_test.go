package plug

import (
	"context"
	"errors"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

// fakeManager returns a manager with a fake clock and a fake plug whose
// reading and switch calls the test controls. Pollers are not started: tests
// call poll by hand.
func fakeManager(t *testing.T) (*Manager, *Reading, *[]bool, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	reading := Reading{On: true}
	var switches []bool
	m := NewManager()
	m.now = func() time.Time { return now }
	m.read = func(context.Context, config.Plug) (Reading, error) { return reading, nil }
	m.switchF = func(_ context.Context, _ config.Plug, on bool) error {
		switches = append(switches, on)
		reading.On = on
		return nil
	}
	return m, &reading, &switches, &now
}

func plugged(auto bool) []config.Printer {
	return []config.Printer{{Name: "P1", Plug: &config.Plug{Kind: "shelly", Address: "10.0.0.9", AutoOff: auto, AutoOffDelayMinutes: 2}}}
}

// addNoLoop registers the plug without a goroutine, so the test owns every poll.
func addNoLoop(m *Manager, printers []config.Printer) {
	m.mu.Lock()
	for _, p := range printers {
		m.plugs[p.Name] = &entry{cfg: *p.Plug, stop: make(chan struct{})}
	}
	m.mu.Unlock()
}

func TestSet_RefusesOffWhilePrintingOrHot(t *testing.T) {
	m, _, switches, _ := fakeManager(t)
	addNoLoop(m, plugged(false))
	inPrint, nozzle := true, 210.0
	m.SetStatusSource(func(string) (bool, float64, bool) { return inPrint, nozzle, true })

	if err := m.Set(context.Background(), "P1", false); !errors.Is(err, ErrPrinting) {
		t.Fatalf("printing: err = %v", err)
	}
	inPrint, nozzle = false, 120
	if err := m.Set(context.Background(), "P1", false); !errors.Is(err, ErrHot) {
		t.Fatalf("hot: err = %v", err)
	}
	nozzle = 30
	if err := m.Set(context.Background(), "P1", false); err != nil {
		t.Fatalf("cool: err = %v", err)
	}
	// On is never refused.
	inPrint = true
	if err := m.Set(context.Background(), "P1", true); err != nil {
		t.Fatalf("on while printing: err = %v", err)
	}
	if len(*switches) != 2 || (*switches)[0] || !(*switches)[1] {
		t.Fatalf("switches = %v", *switches)
	}
	if err := m.Set(context.Background(), "nope", true); !errors.Is(err, ErrNoPlug) {
		t.Fatalf("no plug: err = %v", err)
	}
}

func TestAutoOff_WaitsForIdleDelayAndCoolAndCancelsOnNewPrint(t *testing.T) {
	m, reading, switches, now := fakeManager(t)
	addNoLoop(m, plugged(true))
	inPrint, nozzle := true, 215.0
	m.SetStatusSource(func(string) (bool, float64, bool) { return inPrint, nozzle, true })
	reading.On = true

	m.poll("P1") // printing
	inPrint = false
	m.poll("P1") // print ended: the wait starts
	*now = now.Add(3 * time.Minute)
	m.poll("P1") // idle long enough but hot
	if len(*switches) != 0 {
		t.Fatalf("switched while hot: %v", *switches)
	}
	nozzle = 40
	m.poll("P1")
	if len(*switches) != 1 || (*switches)[0] {
		t.Fatalf("expected one off switch, got %v", *switches)
	}

	// A new print during the wait cancels the auto off.
	inPrint, nozzle = true, 200
	reading.On = true
	m.poll("P1")
	inPrint = false
	m.poll("P1")
	inPrint = true // started again within the delay
	*now = now.Add(time.Minute)
	m.poll("P1")
	inPrint, nozzle = false, 30
	*now = now.Add(5 * time.Minute)
	m.poll("P1") // this end starts a fresh wait; not long enough yet
	if len(*switches) != 1 {
		t.Fatalf("auto off fired without the delay after a new print: %v", *switches)
	}
}

func TestPrintEnergy_CounterDeltaAndFallbackSum(t *testing.T) {
	m, reading, _, now := fakeManager(t)
	addNoLoop(m, plugged(false))
	inPrint := false
	m.SetStatusSource(func(string) (bool, float64, bool) { return inPrint, 30, true })

	// With a counter: end minus start.
	w, e := 100.0, 5000.0
	reading.Watts, reading.EnergyWh = &w, &e
	m.poll("P1")
	inPrint = true
	m.poll("P1") // start: counter 5000
	e = 5123.46
	*now = now.Add(time.Hour)
	inPrint = false
	m.poll("P1") // end: counter 5123.46
	if got := m.PrintEnergy("P1"); got == nil || *got != 123.5 {
		t.Fatalf("counter delta = %v", got)
	}
	if got := m.PrintEnergy("P1"); got != nil {
		t.Fatalf("second take = %v, want nil", *got)
	}

	// Without a counter: watts x time between polls.
	reading.EnergyWh = nil
	inPrint = true
	m.poll("P1") // start
	*now = now.Add(30 * time.Minute)
	m.poll("P1") // 100 W for 0.5 h = 50 Wh
	*now = now.Add(30 * time.Minute)
	w = 200
	m.poll("P1") // another 100 W x 0.5 h (the rate before this sample) = 50 Wh
	inPrint = false
	// The history sender asks before the poller noticed the end.
	if got := m.PrintEnergy("P1"); got == nil || *got != 100 {
		t.Fatalf("fallback sum = %v", got)
	}
}

func TestSync_StartsAndStopsByConfig(t *testing.T) {
	m, _, _, _ := fakeManager(t)
	m.Sync(plugged(false))
	if !m.Has("P1") || m.Current("P1") == nil || m.Current("P1").Kind != "shelly" {
		t.Fatal("plug not registered")
	}
	m.Sync([]config.Printer{{Name: "P1"}})
	if m.Has("P1") || m.Current("P1") != nil {
		t.Fatal("plug not removed")
	}
}

func TestSet_OffWithPrinterLinkDownUsesLastKnownState(t *testing.T) {
	m, _, switches, _ := fakeManager(t)
	addNoLoop(m, plugged(false))
	inPrint, nozzle, ok := false, 30.0, false
	m.SetStatusSource(func(string) (bool, float64, bool) { return inPrint, nozzle, ok })

	// Never saw the printer: can't confirm it is idle.
	if err := m.Set(context.Background(), "P1", false); !errors.Is(err, ErrUnknown) {
		t.Fatalf("never known: err = %v", err)
	}
	// Saw it printing, then the link dropped.
	ok, inPrint, nozzle = true, true, 210
	m.poll("P1")
	ok = false
	if err := m.Set(context.Background(), "P1", false); !errors.Is(err, ErrPrinting) {
		t.Fatalf("was printing: err = %v", err)
	}
	// Saw it idle but hot.
	ok, inPrint = true, false
	m.poll("P1")
	ok = false
	if err := m.Set(context.Background(), "P1", false); !errors.Is(err, ErrHot) {
		t.Fatalf("was hot: err = %v", err)
	}
	// Saw it idle and cool.
	ok, nozzle = true, 30
	m.poll("P1")
	ok = false
	if err := m.Set(context.Background(), "P1", false); err != nil {
		t.Fatalf("was idle and cool: err = %v", err)
	}
	if len(*switches) != 1 {
		t.Fatalf("switches = %v", *switches)
	}
}

// A remembered state goes stale: a print may have started on the printer's
// own screen while the link was down.
func TestSet_OffWithPrinterLinkDownRefusesStaleState(t *testing.T) {
	m, _, switches, now := fakeManager(t)
	addNoLoop(m, plugged(false))
	ok := true
	m.SetStatusSource(func(string) (bool, float64, bool) { return false, 30, ok })
	m.poll("P1") // seen idle and cool
	ok = false
	*now = now.Add(staleState + time.Second)
	if err := m.Set(context.Background(), "P1", false); !errors.Is(err, ErrUnknown) {
		t.Fatalf("stale state: err = %v", err)
	}
	if len(*switches) != 0 {
		t.Fatalf("switches = %v", *switches)
	}
}

func TestPoll_LinkDownMidPrintKeepsPrintAndEnergy(t *testing.T) {
	m, reading, switches, now := fakeManager(t)
	addNoLoop(m, plugged(true))
	inPrint, nozzle, ok := true, 215.0, true
	m.SetStatusSource(func(string) (bool, float64, bool) { return inPrint, nozzle, ok })
	w := 100.0
	reading.Watts = &w

	m.poll("P1") // print starts
	*now = now.Add(30 * time.Minute)
	ok, inPrint, nozzle = false, false, 0 // link down: status unknown
	m.poll("P1")                          // 100 W x 0.5 h
	*now = now.Add(3 * time.Minute)
	m.poll("P1")
	if len(*switches) != 0 || !m.plugs["P1"].pendingOff.IsZero() || !m.plugs["P1"].inPrint {
		t.Fatalf("link drop ended the print: switches %v, entry %+v", *switches, *m.plugs["P1"])
	}
	*now = now.Add(30*time.Minute - 3*time.Minute)
	ok, inPrint, nozzle = true, true, 215 // link back, still printing
	m.poll("P1")                          // another 100 W x 0.5 h
	inPrint = false
	m.poll("P1")
	if got := m.PrintEnergy("P1"); got == nil || *got != 100 {
		t.Fatalf("energy over the blip = %v, want 100", got)
	}
}

func TestPrintEnergy_NilWithoutUsableReading(t *testing.T) {
	m, _, _, _ := fakeManager(t) // readings carry no watts and no counter
	addNoLoop(m, plugged(false))
	inPrint := false
	m.SetStatusSource(func(string) (bool, float64, bool) { return inPrint, 30, true })
	m.poll("P1")
	inPrint = true
	m.poll("P1")
	inPrint = false
	m.poll("P1")
	if got := m.PrintEnergy("P1"); got != nil {
		t.Fatalf("energy = %v, want nil", *got)
	}
}

func TestAutoOff_PendingClearedWhenPlugOffOrSwitchedOnByHand(t *testing.T) {
	m, reading, switches, now := fakeManager(t)
	addNoLoop(m, plugged(true))
	inPrint := true
	m.SetStatusSource(func(string) (bool, float64, bool) { return inPrint, 30, true })
	m.poll("P1")
	inPrint = false
	m.poll("P1")       // wait starts
	reading.On = false // someone switched it off by hand
	*now = now.Add(3 * time.Minute)
	m.poll("P1")      // due, but the plug is off: wait ends
	reading.On = true // later switched on by hand
	*now = now.Add(time.Minute)
	m.poll("P1")
	if len(*switches) != 0 {
		t.Fatalf("Bridge cut a plug that was switched on by hand: %v", *switches)
	}

	// Off then on again before the delay is due: also no auto-off.
	inPrint = true
	m.poll("P1")
	inPrint = false
	m.poll("P1")
	reading.On = false
	m.poll("P1")
	reading.On = true
	*now = now.Add(3 * time.Minute)
	m.poll("P1")
	if len(*switches) != 0 {
		t.Fatalf("Bridge cut a plug that was switched on by hand: %v", *switches)
	}
}

func TestAutoOff_FailedSwitchIsRetriedOnNextPoll(t *testing.T) {
	m, reading, _, now := fakeManager(t)
	addNoLoop(m, plugged(true))
	inPrint := true
	m.SetStatusSource(func(string) (bool, float64, bool) { return inPrint, 30, true })
	reading.On = true
	m.poll("P1") // printing
	inPrint = false
	m.poll("P1") // print ended: the wait starts
	*now = now.Add(3 * time.Minute)

	calls := 0
	m.switchF = func(_ context.Context, _ config.Plug, on bool) error {
		calls++
		if calls == 1 {
			return errors.New("plug offline")
		}
		reading.On = on
		return nil
	}
	m.poll("P1") // due, but the switch fails
	if m.plugs["P1"].pendingOff.IsZero() {
		t.Fatalf("pendingOff cleared after a failed switch")
	}
	m.poll("P1") // retried
	if calls != 2 || m.plugs["P1"].on {
		t.Fatalf("calls = %d, on = %v; want the retry to switch off", calls, m.plugs["P1"].on)
	}
}

func TestAutoOff_RechecksRightBeforeSwitching(t *testing.T) {
	m, _, switches, now := fakeManager(t)
	addNoLoop(m, plugged(true))
	calls, printing := 0, true
	m.SetStatusSource(func(string) (bool, float64, bool) {
		calls++
		if calls == 4 { // poll 3's re-check: a new print has started
			return true, 200, true
		}
		return printing, 30, true
	})
	m.poll("P1")
	printing = false
	m.poll("P1")
	*now = now.Add(3 * time.Minute)
	m.poll("P1") // would switch off; the re-check sees a print
	if len(*switches) != 0 {
		t.Fatalf("switched despite a new print: %v", *switches)
	}
}
