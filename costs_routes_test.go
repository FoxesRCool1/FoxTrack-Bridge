//go:build headless

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
)

// withBridgeConfig installs cfg as the live config with the config directory
// pointed at a temp dir, so a handler that saves cannot touch a real file.
func withBridgeConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	isolateConfigDir(t)
	configMutex.Lock()
	prev := configStore
	configStore = cfg
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = prev
		configMutex.Unlock()
	})
}

func serveHandler(h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestCostsSettingsRoundTrip(t *testing.T) {
	cfg := &config.Config{Printers: []config.Printer{{ID: "p1", Name: "Fox"}}}
	withBridgeConfig(t, cfg)

	rec := serveHandler(handleCosts, httptest.NewRequest("GET", "/api/costs", nil))
	var v costsView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 200 || v.MaterialPerKg == nil {
		t.Fatalf("GET: %d %s (the material table must be {} not null)", rec.Code, rec.Body)
	}

	rec = serveHandler(handleCosts, jsonPost("/api/costs", `{"currency":" EUR ","filament_per_kg":20,"energy_per_kwh":0.3,"material_per_kg":{"pla":18,"Petg ":25}}`))
	if rec.Code != 200 {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body)
	}
	json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Currency != "EUR" || v.FilamentPerKg != 20 || v.EnergyPerKWh != 0.3 || v.MaterialPerKg["PLA"] != 18 || v.MaterialPerKg["PETG"] != 25 {
		t.Fatalf("saved view = %+v", v)
	}
	if len(cfg.Printers) != 1 || cfg.Printers[0].Name != "Fox" {
		t.Fatalf("saving prices touched the printers: %+v", cfg.Printers)
	}

	// A field left out keeps what is stored.
	serveHandler(handleCosts, jsonPost("/api/costs", `{"currency":"$"}`))
	if c := cfg.Costs; c.Currency != "$" || c.FilamentPerKg != 20 || len(c.MaterialPerKg) != 2 {
		t.Fatalf("partial update changed other fields: %+v", c)
	}
}

func TestCostsSettingsRefuseBadInput(t *testing.T) {
	cfg := &config.Config{Costs: &config.Costs{Currency: "$", FilamentPerKg: 20}}
	withBridgeConfig(t, cfg)
	for name, body := range map[string]string{
		"negative price":      `{"filament_per_kg":-1}`,
		"absurd price":        `{"energy_per_kwh":1e9}`,
		"long currency":       `{"currency":"123456789"}`,
		"empty material name": `{"material_per_kg":{" ":5}}`,
		"material price":      `{"material_per_kg":{"PLA":-3}}`,
		"duplicate material":  `{"material_per_kg":{"pla":5,"PLA":6}}`,
		"not json":            `nope`,
	} {
		rec := serveHandler(handleCosts, jsonPost("/api/costs", body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if c := cfg.Costs; c.Currency != "$" || c.FilamentPerKg != 20 || c.EnergyPerKWh != 0 {
		t.Errorf("a refused request changed the prices: %+v", c)
	}
	req := httptest.NewRequest("POST", "/api/costs", nil)
	req.Header.Set("Content-Type", "text/plain")
	if rec := serveHandler(handleCosts, req); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain POST: status %d, want 415", rec.Code)
	}
}

func TestCostsSummaryRoute(t *testing.T) {
	cfg := &config.Config{
		Printers: []config.Printer{{ID: "p1", Name: "Fox", PreviousNames: []string{"Old fox"}}},
		Costs:    &config.Costs{Currency: "$", FilamentPerKg: 20, EnergyPerKWh: 0.5},
	}
	withBridgeConfig(t, cfg)
	now := time.Now().Unix()
	wh := 2000.0
	for _, r := range []history.Record{
		{PrinterName: "Fox", EndTime: now - 3600, Result: "finished", Filaments: []history.FilamentUse{{Slot: 0, Material: "PLA", Grams: 100}}, EnergyWh: &wh},
		{PrinterName: "Old fox", EndTime: now - 7200, Result: "cancelled", Filaments: []history.FilamentUse{{Slot: 0, Material: "PLA", Grams: 50}}},
		{PrinterName: "Fox", EndTime: now - 90*86400, Result: "finished", Filaments: []history.FilamentUse{{Slot: 0, Material: "PLA", Grams: 999}}},
	} {
		if err := history.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	rec := serveHandler(handleCostsSummary, httptest.NewRequest("GET", "/api/costs/summary", nil))
	var got struct {
		Days     int    `json:"days"`
		Currency string `json:"currency"`
		Printers []struct {
			Printer string  `json:"printer"`
			Prints  int     `json:"prints"`
			Grams   float64 `json:"grams"`
			Cost    float64 `json:"cost"`
		} `json:"printers"`
		Overall struct {
			Prints int     `json:"prints"`
			KWh    float64 `json:"kwh"`
			Cost   float64 `json:"cost"`
		} `json:"overall"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, err)
	}
	if got.Days != 30 || got.Currency != "$" || len(got.Printers) != 1 || got.Printers[0].Printer != "Fox" {
		t.Fatalf("summary = %+v (the old name must merge into Fox, the 90-day-old print must not count)", got)
	}
	// 150 g at 20/kg = 3.00, plus 2 kWh at 0.5 = 1.00
	if p := got.Printers[0]; p.Prints != 2 || p.Grams != 150 || got.Overall.Cost < 3.999 || got.Overall.Cost > 4.001 || got.Overall.KWh != 2 {
		t.Fatalf("totals wrong: %+v overall %+v", p, got.Overall)
	}

	for _, q := range []string{"days=0", "days=abc", "days=99999"} {
		if rec := serveHandler(handleCostsSummary, httptest.NewRequest("GET", "/api/costs/summary?"+q, nil)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, rec.Code)
		}
	}
	rec = serveHandler(handleCostsSummary, httptest.NewRequest("GET", "/api/costs/summary?days=365", nil))
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Printers[0].Prints != 3 {
		t.Errorf("365 days should include the old print, got %+v", got.Printers)
	}
}
