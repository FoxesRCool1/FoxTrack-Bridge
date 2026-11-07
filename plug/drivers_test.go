package plug

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

func f(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}

func TestParseShellyGen2_Recorded(t *testing.T) {
	r, err := parseShellyGen2([]byte(`{"id":0,"source":"button","output":true,"apower":142.5,"voltage":231.2,"current":0.62,"aenergy":{"total":12345.678,"by_minute":[0,0,0],"minute_ts":1700000000},"temperature":{"tC":41.2,"tF":106.2}}`))
	if err != nil || !r.On || f(r.Watts) != 142.5 || f(r.EnergyWh) != 12345.678 {
		t.Fatalf("gen2 = %+v (%v)", r, err)
	}
}

func TestParseShellyGen1_WattMinutes(t *testing.T) {
	r, err := parseShellyGen1([]byte(`{"relays":[{"ison":false,"has_timer":false}],"meters":[{"power":0.0,"is_valid":true,"total":6000}]}`))
	if err != nil || r.On || f(r.Watts) != 0 || f(r.EnergyWh) != 100 {
		t.Fatalf("gen1 = %+v (%v)", r, err)
	}
}

func TestParseTasmota_Recorded(t *testing.T) {
	on, ok := parseTasmotaPower([]byte(`{"POWER":"ON"}`))
	if !ok || !on {
		t.Fatalf("power = %v %v", on, ok)
	}
	w, wh := parseTasmotaEnergy([]byte(`{"StatusSNS":{"Time":"2026-10-30T10:00:00","ENERGY":{"TotalStartTime":"2026-01-01T00:00:00","Total":12.345,"Yesterday":0.5,"Today":0.2,"Power":98,"ApparentPower":110,"ReactivePower":50,"Factor":0.89,"Voltage":230,"Current":0.478}}}`))
	if f(w) != 98 || f(wh) != 12345 {
		t.Fatalf("energy = %v %v", f(w), f(wh))
	}
	if w, wh := parseTasmotaEnergy([]byte(`{"StatusSNS":{"Time":"2026-10-30T10:00:00"}}`)); w != nil || wh != nil {
		t.Fatal("no meter must read nil")
	}
}

func TestKasaCipher_RoundTrip(t *testing.T) {
	plain := []byte(`{"system":{"get_sysinfo":{}}}`)
	enc := kasaEncrypt(plain)
	if len(enc) != 4+len(plain) || enc[0] != 0 || enc[3] != byte(len(plain)) {
		t.Fatalf("length prefix wrong: %v", enc[:4])
	}
	if got := kasaDecrypt(enc[4:]); !bytes.Equal(got, plain) {
		t.Fatalf("round trip = %q", got)
	}
	// Known first byte: 171 ^ '{' (0x7B) = 0xD0.
	if enc[4] != 0xD0 {
		t.Fatalf("first cipher byte = %#x", enc[4])
	}
}

func TestParseKasa_MilliwattsAndNoMeter(t *testing.T) {
	r, err := parseKasa([]byte(`{"system":{"get_sysinfo":{"sw_ver":"1.0.4","model":"KP115(EU)","relay_state":1,"err_code":0}},"emeter":{"get_realtime":{"voltage_mv":230123,"current_ma":450,"power_mw":103500,"total_wh":8812,"err_code":0}}}`))
	if err != nil || !r.On || f(r.Watts) != 103.5 || f(r.EnergyWh) != 8812 {
		t.Fatalf("kasa = %+v (%v)", r, err)
	}
	r, err = parseKasa([]byte(`{"system":{"get_sysinfo":{"relay_state":0,"err_code":0}},"emeter":{"get_realtime":{"err_code":-1,"err_msg":"module not support"}}}`))
	if err != nil || r.On || r.Watts != nil || r.EnergyWh != nil {
		t.Fatalf("kasa no meter = %+v (%v)", r, err)
	}
	// Old HS110 firmware: power in W, total in kWh.
	r, _ = parseKasa([]byte(`{"system":{"get_sysinfo":{"relay_state":1}},"emeter":{"get_realtime":{"power":12.5,"total":1.5,"err_code":0}}}`))
	if f(r.Watts) != 12.5 || f(r.EnergyWh) != 1500 {
		t.Fatalf("kasa old = %+v", r)
	}
}

func TestShellyRead_FallsBackToGen1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			_, _ = w.Write([]byte(`{"relays":[{"ison":true}],"meters":[{"power":60,"total":600}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r, err := Read(context.Background(), config.Plug{Kind: "shelly", Address: srv.URL})
	if err != nil || !r.On || f(r.Watts) != 60 || f(r.EnergyWh) != 10 {
		t.Fatalf("read = %+v (%v)", r, err)
	}
}

func TestHostOf_Forms(t *testing.T) {
	for in, want := range map[string]string{"192.168.1.80": "192.168.1.80", " http://plug.local/ ": "plug.local", "10.0.0.5:8080": "10.0.0.5:8080"} {
		if got, err := hostOf(in); err != nil || got != want {
			t.Errorf("hostOf(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := hostOf(""); err == nil || !strings.Contains(err.Error(), "address") {
		t.Fatal("blank address accepted")
	}
}

func TestLockPlug_SameHostSerialized(t *testing.T) {
	var holders, maxHolders int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer lockPlug("serial-test.local")()
			n := atomic.AddInt32(&holders, 1)
			for {
				m := atomic.LoadInt32(&maxHolders)
				if n <= m || atomic.CompareAndSwapInt32(&maxHolders, m, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&holders, -1)
		}()
	}
	wg.Wait()
	if maxHolders != 1 {
		t.Fatalf("max concurrent holders = %d, want 1", maxHolders)
	}
}

func TestLockPlug_DifferentHostNotBlocked(t *testing.T) {
	defer lockPlug("other-a.local")()
	done := make(chan struct{})
	go func() {
		lockPlug("other-b.local")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a different host was blocked")
	}
}
