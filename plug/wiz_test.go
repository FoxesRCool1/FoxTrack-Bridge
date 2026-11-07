package plug

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// wizFake is a fake WiZ plug on UDP: it answers getPilot and getPower from
// its state and logs every request it receives.
type wizFake struct {
	conn     net.PacketConn
	mu       sync.Mutex
	on       bool
	pc       *float64
	power    *float64
	drops    int
	requests []string
}

func newWizFake(t *testing.T, on bool, pc, power *float64, drops int) *wizFake {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	f := &wizFake{conn: conn, on: on, pc: pc, power: power, drops: drops}
	go f.serve()
	return f
}

func (f *wizFake) host() string { return f.conn.LocalAddr().String() }

func (f *wizFake) Close() { f.conn.Close() }

func (f *wizFake) pilotReply() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := map[string]any{"state": f.on}
	if f.pc != nil {
		res["pc"] = *f.pc
	}
	b, _ := json.Marshal(map[string]any{"method": "getPilot", "env": "pro", "result": res})
	return string(b)
}

func (f *wizFake) powerReply() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.power == nil {
		return `{"method":"getPower","env":"pro","error":{"code":-32601,"message":"Method not found"}}`
	}
	b, _ := json.Marshal(map[string]any{"method": "getPower", "env": "pro", "result": map[string]any{"power": *f.power}})
	return string(b)
}

func (f *wizFake) serve() {
	buf := make([]byte, wizReplySize)
	for {
		n, addr, err := f.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		var req struct {
			Method string `json:"method"`
			Params struct {
				State *bool `json:"state"`
			} `json:"params"`
		}
		if err := json.Unmarshal(buf[:n], &req); err != nil {
			continue
		}
		f.mu.Lock()
		f.requests = append(f.requests, req.Method)
		drop := f.drops > 0
		if drop {
			f.drops--
		}
		f.mu.Unlock()
		if drop {
			continue
		}
		var reply string
		switch req.Method {
		case "getPilot":
			reply = f.pilotReply()
		case "getPower":
			reply = f.powerReply()
		case "setPilot":
			if req.Params.State != nil {
				f.mu.Lock()
				f.on = *req.Params.State
				f.mu.Unlock()
			}
			reply = `{"method":"setPilot","env":"pro","result":{"success":true}}`
		default:
			continue
		}
		if _, err := f.conn.WriteTo([]byte(reply), addr); err != nil {
			return
		}
	}
}

func (f *wizFake) saw(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.requests {
		if m == method {
			return true
		}
	}
	return false
}

func TestWizReadMetered(t *testing.T) {
	pc, power := float64(12500), float64(12500)
	f := newWizFake(t, true, &pc, &power, 0)
	defer f.Close()

	r, err := wizRead(context.Background(), f.host())
	if err != nil {
		t.Fatalf("wizRead: %v", err)
	}
	if !r.On {
		t.Errorf("On = false, want true")
	}
	if r.Watts == nil || *r.Watts != 12.5 {
		t.Errorf("Watts = %v, want 12.5", r.Watts)
	}
	if r.EnergyWh != nil {
		t.Errorf("EnergyWh = %v, want nil", r.EnergyWh)
	}
	if !f.saw("getPilot") || !f.saw("getPower") {
		t.Errorf("requests = %v, want getPilot and getPower", f.requests)
	}
}

func TestWizReadNoMeterFallsBackToPc(t *testing.T) {
	pc := float64(3000)
	f := newWizFake(t, true, &pc, nil, 0)
	defer f.Close()

	r, err := wizRead(context.Background(), f.host())
	if err != nil {
		t.Fatalf("wizRead: %v", err)
	}
	if !r.On {
		t.Errorf("On = false, want true")
	}
	if r.Watts == nil || *r.Watts != 3 {
		t.Errorf("Watts = %v, want 3", r.Watts)
	}
}

func TestWizReadNoMeterNoPc(t *testing.T) {
	f := newWizFake(t, false, nil, nil, 0)
	defer f.Close()

	r, err := wizRead(context.Background(), f.host())
	if err != nil {
		t.Fatalf("wizRead: %v", err)
	}
	if r.On {
		t.Errorf("On = true, want false")
	}
	if r.Watts != nil {
		t.Errorf("Watts = %v, want nil", *r.Watts)
	}
}

func TestWizSwitchRoundTrip(t *testing.T) {
	f := newWizFake(t, false, nil, nil, 0)
	defer f.Close()

	if err := wizSwitch(context.Background(), f.host(), true); err != nil {
		t.Fatalf("wizSwitch on: %v", err)
	}
	r, err := wizRead(context.Background(), f.host())
	if err != nil {
		t.Fatalf("wizRead: %v", err)
	}
	if !r.On {
		t.Errorf("On = false after setPilot true, want true")
	}
	if err := wizSwitch(context.Background(), f.host(), false); err != nil {
		t.Fatalf("wizSwitch off: %v", err)
	}
	r, err = wizRead(context.Background(), f.host())
	if err != nil {
		t.Fatalf("wizRead: %v", err)
	}
	if r.On {
		t.Errorf("On = true after setPilot false, want false")
	}
}

func TestWizSwitchRefused(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, wizReplySize)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var req struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(buf[:n], &req) != nil {
				continue
			}
			reply := `{"method":"getPilot","env":"pro","result":{"state":true}}`
			if req.Method == "setPilot" {
				reply = `{"method":"setPilot","env":"pro","result":{"success":false}}`
			}
			if _, err := conn.WriteTo([]byte(reply), addr); err != nil {
				return
			}
		}
	}()

	err = wizSwitch(context.Background(), conn.LocalAddr().String(), true)
	if !errors.Is(err, errWizRefused) {
		t.Errorf("err = %v, want the refuse error", err)
	}
}

func TestWizExchangeResendsThroughDrops(t *testing.T) {
	pc := float64(12500)
	f := newWizFake(t, true, &pc, nil, 1) // the first datagram is dropped
	defer f.Close()

	r, err := wizRead(context.Background(), f.host())
	if err != nil {
		t.Fatalf("wizRead: %v", err)
	}
	if !r.On || r.Watts == nil || *r.Watts != 12.5 {
		t.Errorf("got %+v, want On true and 12.5 W", r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) < 2 {
		t.Errorf("requests = %v, want the resend to reach the plug", f.requests)
	}
}

func TestWizExchangeNoAnswer(t *testing.T) {
	// A port that nothing listens on: the datagrams go out, no reply comes back.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	conn.Close()
	host := conn.LocalAddr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = wizExchange(ctx, host, []byte(`{"method":"getPilot","params":{}}`))
	if !errors.Is(err, errNoAnswer) {
		t.Errorf("err = %v, want errNoAnswer", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v, want under 2 s", time.Since(start))
	}
}
