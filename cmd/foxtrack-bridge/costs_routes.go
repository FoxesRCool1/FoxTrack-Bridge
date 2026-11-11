package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"foxtrack-bridge/config"
	"foxtrack-bridge/costs"
	"foxtrack-bridge/history"
)

// Cost settings and the spending summary. The prices live in config.Costs and
// are written only here, so a Settings save elsewhere never touches them.

func init() {
	http.HandleFunc("/api/costs", handleCosts)                // GET/POST: prices
	http.HandleFunc("/api/costs/summary", handleCostsSummary) // GET: totals per printer, ?days=30
}

const (
	maxPrice         = 1_000_000.0
	maxCostMaterials = 40
)

// costsView is what the dashboard reads: every field present, the table never
// null.
type costsView struct {
	Currency      string             `json:"currency"`
	FilamentPerKg float64            `json:"filament_per_kg"`
	MaterialPerKg map[string]float64 `json:"material_per_kg"`
	EnergyPerKWh  float64            `json:"energy_per_kwh"`
}

func costsViewOf(c *config.Costs) costsView {
	v := costsView{MaterialPerKg: map[string]float64{}}
	if c == nil {
		return v
	}
	v.Currency, v.FilamentPerKg, v.EnergyPerKWh = c.Currency, c.FilamentPerKg, c.EnergyPerKWh
	for k, p := range c.MaterialPerKg {
		v.MaterialPerKg[k] = p
	}
	return v
}

func validPrice(p float64) bool {
	return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= maxPrice
}

func handleCosts(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		configMutex.RLock()
		view := costsViewOf(configStore.Costs)
		configMutex.RUnlock()
		json.NewEncoder(w).Encode(view)
	case "POST":
		if !requireJSON(w, r) {
			return
		}
		// A field left out keeps what is stored; a material table that is
		// sent replaces the stored one.
		var req struct {
			Currency      *string             `json:"currency"`
			FilamentPerKg *float64            `json:"filament_per_kg"`
			MaterialPerKg *map[string]float64 `json:"material_per_kg"`
			EnergyPerKWh  *float64            `json:"energy_per_kwh"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "The request could not be read.")
			return
		}
		bad := func(msg string) { writeErr(w, http.StatusBadRequest, msg) }
		var currency string
		if req.Currency != nil {
			currency = strings.TrimSpace(*req.Currency)
			if utf8.RuneCountInString(currency) > 8 {
				bad("The currency can be at most 8 characters, for example $ or EUR.")
				return
			}
		}
		if (req.FilamentPerKg != nil && !validPrice(*req.FilamentPerKg)) || (req.EnergyPerKWh != nil && !validPrice(*req.EnergyPerKWh)) {
			bad("Prices must be numbers from 0 up to 1000000.")
			return
		}
		var materials map[string]float64
		if req.MaterialPerKg != nil {
			if len(*req.MaterialPerKg) > maxCostMaterials {
				bad("Too many materials. Remove some.")
				return
			}
			materials = map[string]float64{}
			for name, price := range *req.MaterialPerKg {
				key := strings.ToUpper(strings.TrimSpace(name))
				switch {
				case key == "" || utf8.RuneCountInString(key) > 24:
					bad("A material name must be 1 to 24 characters.")
					return
				case !validPrice(price):
					bad("Prices must be numbers from 0 up to 1000000.")
					return
				}
				if _, dup := materials[key]; dup {
					bad("Each material can be listed once.")
					return
				}
				materials[key] = price
			}
		}
		err := mutateConfig(func(c *config.Config) error {
			if c.Costs == nil {
				c.Costs = &config.Costs{}
			}
			if req.Currency != nil {
				c.Costs.Currency = currency
			}
			if req.FilamentPerKg != nil {
				c.Costs.FilamentPerKg = *req.FilamentPerKg
			}
			if req.EnergyPerKWh != nil {
				c.Costs.EnergyPerKWh = *req.EnergyPerKWh
			}
			if req.MaterialPerKg != nil {
				c.Costs.MaterialPerKg = materials
			}
			return nil
		})
		if err != nil {
			writeSaveFailed(w, err)
			return
		}
		configMutex.RLock()
		view := costsViewOf(configStore.Costs)
		configMutex.RUnlock()
		json.NewEncoder(w).Encode(view)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleCostsSummary totals the filament and electricity cost of the prints
// that ended in the last ?days (default 30), per printer and overall. Prints
// filed under a printer's old name count for the printer's current name.
func handleCostsSummary(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	days := 30
	if s := r.URL.Query().Get("days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 3650 {
			writeErr(w, http.StatusBadRequest, "days must be a number from 1 to 3650.")
			return
		}
		days = n
	}
	recs, err := history.Load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "The print history could not be read.")
		return
	}
	configMutex.RLock()
	var prices *config.Costs
	if configStore.Costs != nil {
		cp := *configStore.Costs
		prices = &cp
	}
	current := map[string]string{}
	for _, p := range configStore.Printers {
		for _, old := range p.PreviousNames {
			current[old] = p.Name
		}
	}
	configMutex.RUnlock()
	sum := costs.Summarize(prices, recs, time.Now().AddDate(0, 0, -days).Unix(), func(name string) string {
		if cur, ok := current[name]; ok {
			return cur
		}
		return name
	})
	out := struct {
		Days     int    `json:"days"`
		Currency string `json:"currency"`
		costs.Summary
	}{Days: days, Summary: sum}
	if prices != nil {
		out.Currency = prices.Currency
	}
	json.NewEncoder(w).Encode(out)
}
