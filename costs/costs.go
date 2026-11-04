// Package costs prices history records: filament by spool or by length, and
// the energy a print used.
package costs

import (
	"math"
	"sort"
	"strings"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
)

// Breakdown is the cost of one print, split by where the money went.
type Breakdown struct {
	FilamentGrams float64 `json:"filament_grams"` // grams the record shows, 0 when it shows none
	FilamentCost  float64 `json:"filament_cost"`
	FilamentKnown bool    `json:"filament_known"` // true when grams > 0 AND every gram has a price (so FilamentCost is real)
	EnergyKWh     float64 `json:"energy_kwh"`     // EnergyWh/1000 when the record has EnergyWh, else 0
	EnergyCost    float64 `json:"energy_cost"`
	EnergyKnown   bool    `json:"energy_known"` // true when EnergyWh != nil AND c.EnergyPerKWh > 0
	Total         float64 `json:"total"`        // FilamentCost + EnergyCost (an unknown part contributes 0)
}

// PricePerKg looks up the price of material in c. A per-material price wins
// over the FilamentPerKg fallback; (0, false) means the price is unknown.
func PricePerKg(c *config.Costs, material string) (float64, bool) {
	if c == nil {
		return 0, false
	}
	if material != "" {
		for name, price := range c.MaterialPerKg {
			if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(material)) && price > 0 {
				return price, true
			}
		}
	}
	if c.FilamentPerKg > 0 {
		return c.FilamentPerKg, true
	}
	return 0, false
}

// Density returns the grams-per-cm^3 density of material, by its leading
// letters ("PLA-CF" -> PLA). Unknown materials fall back to PLA's density.
func Density(material string) float64 {
	upper := strings.ToUpper(strings.TrimSpace(material))
	for i := 0; i < len(upper); i++ {
		if upper[i] < 'A' || upper[i] > 'Z' {
			upper = upper[:i]
			break
		}
	}
	switch upper {
	case "PLA":
		return 1.24
	case "PETG":
		return 1.27
	case "ABS":
		return 1.04
	case "ASA":
		return 1.07
	case "TPU":
		return 1.21
	case "PC":
		return 1.20
	case "PA", "NYLON":
		return 1.14
	default:
		return 1.24
	}
}

// GramsFromMM converts a filament length in millimetres of 1.75 mm filament
// to grams, using material's density (PLA's when material is empty).
func GramsFromMM(mm float64, material string) float64 {
	const radius = 1.75 / 2
	return mm * math.Pi * radius * radius / 1000 * Density(material)
}

// ForRecord prices one history record. c may be nil: then nothing is Known,
// but grams and kWh are still filled.
func ForRecord(c *config.Costs, r history.Record) Breakdown {
	var b Breakdown

	hasGrams := false
	allPriced := true
	for _, f := range r.Filaments {
		if f.Grams <= 0 {
			continue
		}
		hasGrams = true
		b.FilamentGrams += f.Grams
		if price, ok := PricePerKg(c, f.Material); ok {
			b.FilamentCost += f.Grams / 1000 * price
		} else {
			allPriced = false
		}
	}
	if !hasGrams && r.FilamentMM > 0 {
		b.FilamentGrams = GramsFromMM(r.FilamentMM, "")
		if price, ok := PricePerKg(c, ""); ok {
			b.FilamentCost = b.FilamentGrams / 1000 * price
			b.FilamentKnown = true
		}
	} else if hasGrams {
		b.FilamentKnown = allPriced
	}

	if r.EnergyWh != nil {
		b.EnergyKWh = *r.EnergyWh / 1000
		if c != nil && c.EnergyPerKWh > 0 {
			b.EnergyCost = b.EnergyKWh * c.EnergyPerKWh
			b.EnergyKnown = true
		}
	}

	b.Total = b.FilamentCost + b.EnergyCost
	return b
}

// Total is the running sum of a set of prints.
type Total struct {
	Prints int     `json:"prints"`
	Grams  float64 `json:"grams"`
	KWh    float64 `json:"kwh"`
	Cost   float64 `json:"cost"`
}

// PrinterTotal is one printer's totals; Total is embedded, so the JSON is flat.
type PrinterTotal struct {
	Printer string `json:"printer"`
	Total
}

// Summary is the totals per printer plus the overall total.
type Summary struct {
	Printers []PrinterTotal `json:"printers"`
	Overall  Total          `json:"overall"`
}

// Summarize totals the records with EndTime >= since, per printer and overall.
// Every Result counts: cancelled prints used filament too. canon maps an old
// printer name to its current one and may be nil.
func Summarize(c *config.Costs, recs []history.Record, since int64, canon func(string) string) Summary {
	byPrinter := map[string]*Total{}
	var overall Total
	printers := []PrinterTotal{}
	for _, r := range recs {
		if r.EndTime < since {
			continue
		}
		name := r.PrinterName
		if canon != nil {
			name = canon(name)
		}
		b := ForRecord(c, r)
		t := byPrinter[name]
		if t == nil {
			t = &Total{}
			byPrinter[name] = t
		}
		t.Prints++
		t.Grams += b.FilamentGrams
		t.KWh += b.EnergyKWh
		t.Cost += b.Total
		overall.Prints++
		overall.Grams += b.FilamentGrams
		overall.KWh += b.EnergyKWh
		overall.Cost += b.Total
	}
	for name, t := range byPrinter {
		printers = append(printers, PrinterTotal{Printer: name, Total: *t})
	}
	sort.Slice(printers, func(i, j int) bool { return printers[i].Printer < printers[j].Printer })
	return Summary{Printers: printers, Overall: overall}
}
