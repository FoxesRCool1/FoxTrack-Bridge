package costs

import (
	"math"
	"testing"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
)

// closeEnough reports whether a and b are within tol.
func closeEnough(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestDensity(t *testing.T) {
	tests := []struct {
		material string
		want     float64
	}{
		{"pla", 1.24},
		{"PLA-CF", 1.24},
		{"PETG HF", 1.27},
		{"ABS", 1.04},
		{"ASA", 1.07},
		{"TPU", 1.21},
		{"PC", 1.20},
		{"PA12", 1.14},
		{"Nylon", 1.14},
		{"", 1.24},
		{"wood", 1.24},
	}
	for _, tt := range tests {
		if got := Density(tt.material); got != tt.want {
			t.Errorf("Density(%q) = %v, want %v", tt.material, got, tt.want)
		}
	}
}

func TestGramsFromMM(t *testing.T) {
	if got := GramsFromMM(1000, ""); !closeEnough(got, 2.9826, 0.001) {
		t.Errorf("GramsFromMM(1000, \"\") = %v, want 2.9826 within 0.001", got)
	}
	if got := GramsFromMM(1000, "PETG"); !closeEnough(got, 3.0548, 0.001) {
		t.Errorf("GramsFromMM(1000, \"PETG\") = %v, want 3.0548 within 0.001", got)
	}
}

func TestPricePerKg(t *testing.T) {
	c := &config.Costs{
		FilamentPerKg: 18,
		MaterialPerKg: map[string]float64{"PLA": 20, "petg": 25},
	}
	if got, ok := PricePerKg(c, "pla"); !ok || got != 20 {
		t.Errorf("PricePerKg(c, \"pla\") = %v, %v; want 20, true", got, ok)
	}
	if got, ok := PricePerKg(c, " PETG "); !ok || got != 25 {
		t.Errorf("PricePerKg(c, \" PETG \") = %v, %v; want 25, true", got, ok)
	}
	if got, ok := PricePerKg(c, "ABS"); !ok || got != 18 {
		t.Errorf("PricePerKg(c, \"ABS\") = %v, %v; want 18, true", got, ok)
	}
	if got, ok := PricePerKg(c, "PLA-CF"); !ok || got != 20 {
		t.Errorf("PricePerKg(c, \"PLA-CF\") = %v, %v; want 20, true (the PLA price)", got, ok)
	}
	c.MaterialPerKg["PLA-CF"] = 40
	if got, ok := PricePerKg(c, "pla-cf"); !ok || got != 40 {
		t.Errorf("PricePerKg(c, \"pla-cf\") = %v, %v; want 40, true (an exact name wins)", got, ok)
	}
	delete(c.MaterialPerKg, "PLA-CF")
	if got, ok := PricePerKg(c, ""); !ok || got != 18 {
		t.Errorf("PricePerKg(c, \"\") = %v, %v; want 18, true", got, ok)
	}

	noFallback := &config.Costs{MaterialPerKg: map[string]float64{"PLA": 20, "petg": 25}}
	if got, ok := PricePerKg(noFallback, "ABS"); ok || got != 0 {
		t.Errorf("PricePerKg(noFallback, \"ABS\") = %v, %v; want 0, false", got, ok)
	}

	if got, ok := PricePerKg(nil, "PLA"); ok || got != 0 {
		t.Errorf("PricePerKg(nil, \"PLA\") = %v, %v; want 0, false", got, ok)
	}
}

func TestForRecordBambu(t *testing.T) {
	c := &config.Costs{
		MaterialPerKg: map[string]float64{"PLA": 20, "PETG": 30},
		FilamentPerKg: 10,
	}
	r := history.Record{
		Filaments: []history.FilamentUse{
			{Slot: 0, Material: "PLA", Grams: 100},
			{Slot: 1, Material: "PETG", Grams: 50},
		},
	}
	b := ForRecord(c, r)
	if !closeEnough(b.FilamentGrams, 150, 1e-6) {
		t.Errorf("FilamentGrams = %v, want 150", b.FilamentGrams)
	}
	if !closeEnough(b.FilamentCost, 3.5, 1e-6) {
		t.Errorf("FilamentCost = %v, want 3.5", b.FilamentCost)
	}
	if !b.FilamentKnown {
		t.Errorf("FilamentKnown = false, want true")
	}
}

func TestForRecordBambuFallback(t *testing.T) {
	// A material with no price falls back to FilamentPerKg.
	c := &config.Costs{
		MaterialPerKg: map[string]float64{"PLA": 20},
		FilamentPerKg: 10,
	}
	r := history.Record{
		Filaments: []history.FilamentUse{
			{Slot: 0, Material: "PLA", Grams: 100},
			{Slot: 1, Material: "ABS", Grams: 50},
		},
	}
	b := ForRecord(c, r)
	if !closeEnough(b.FilamentGrams, 150, 1e-6) {
		t.Errorf("FilamentGrams = %v, want 150", b.FilamentGrams)
	}
	if !closeEnough(b.FilamentCost, 2.5, 1e-6) {
		t.Errorf("FilamentCost = %v, want 2.5 (PLA at 20/kg, ABS at the 10/kg fallback)", b.FilamentCost)
	}
	if !b.FilamentKnown {
		t.Errorf("FilamentKnown = false, want true")
	}

	// With no fallback, the unpriced entry is left out of the cost and
	// the breakdown is not known.
	c = &config.Costs{MaterialPerKg: map[string]float64{"PLA": 20}}
	b = ForRecord(c, r)
	if !closeEnough(b.FilamentGrams, 150, 1e-6) {
		t.Errorf("FilamentGrams = %v, want 150", b.FilamentGrams)
	}
	if !closeEnough(b.FilamentCost, 2.0, 1e-6) {
		t.Errorf("FilamentCost = %v, want 2.0 (only the priced PLA entry)", b.FilamentCost)
	}
	if b.FilamentKnown {
		t.Errorf("FilamentKnown = true, want false")
	}
}

func TestForRecordNoFilamentInfo(t *testing.T) {
	r := history.Record{
		Filaments: []history.FilamentUse{
			{Slot: 0, Material: "PLA", Grams: 0},
			{Slot: 1, Material: "PETG", Grams: 0},
		},
	}
	b := ForRecord(&config.Costs{FilamentPerKg: 20}, r)
	if b.FilamentGrams != 0 || b.FilamentCost != 0 || b.FilamentKnown {
		t.Errorf("expected all-zero filament, got grams=%v cost=%v known=%v", b.FilamentGrams, b.FilamentCost, b.FilamentKnown)
	}
}

func TestForRecordKlipper(t *testing.T) {
	r := history.Record{FilamentMM: 1000}

	b := ForRecord(&config.Costs{FilamentPerKg: 20}, r)
	if !closeEnough(b.FilamentGrams, 2.9826, 1e-4) {
		t.Errorf("FilamentGrams = %v, want ~2.9826", b.FilamentGrams)
	}
	if !closeEnough(b.FilamentCost, 0.059652, 1e-4) {
		t.Errorf("FilamentCost = %v, want ~0.059652", b.FilamentCost)
	}
	if !b.FilamentKnown {
		t.Errorf("FilamentKnown = false, want true")
	}

	b = ForRecord(&config.Costs{}, r)
	if !closeEnough(b.FilamentGrams, 2.9826, 1e-4) {
		t.Errorf("FilamentGrams = %v, want ~2.9826", b.FilamentGrams)
	}
	if b.FilamentCost != 0 {
		t.Errorf("FilamentCost = %v, want 0", b.FilamentCost)
	}
	if b.FilamentKnown {
		t.Errorf("FilamentKnown = true, want false")
	}
}

func wh(v float64) *float64 { return &v }

func TestForRecordEnergy(t *testing.T) {
	c := &config.Costs{EnergyPerKWh: 0.30}
	r := history.Record{EnergyWh: wh(1500)}
	b := ForRecord(c, r)
	if !closeEnough(b.EnergyKWh, 1.5, 1e-6) {
		t.Errorf("EnergyKWh = %v, want 1.5", b.EnergyKWh)
	}
	if !closeEnough(b.EnergyCost, 0.45, 1e-6) {
		t.Errorf("EnergyCost = %v, want 0.45", b.EnergyCost)
	}
	if !b.EnergyKnown {
		t.Errorf("EnergyKnown = false, want true")
	}
	if !closeEnough(b.Total, 0.45, 1e-6) {
		t.Errorf("Total = %v, want 0.45 (energy only)", b.Total)
	}

	b = ForRecord(c, history.Record{})
	if b.EnergyKnown {
		t.Errorf("EnergyKnown = true, want false when EnergyWh is nil")
	}
	if b.EnergyKWh != 0 {
		t.Errorf("EnergyKWh = %v, want 0", b.EnergyKWh)
	}

	b = ForRecord(&config.Costs{}, history.Record{EnergyWh: wh(1500)})
	if !closeEnough(b.EnergyKWh, 1.5, 1e-6) {
		t.Errorf("EnergyKWh = %v, want 1.5", b.EnergyKWh)
	}
	if b.EnergyKnown {
		t.Errorf("EnergyKnown = true, want false when EnergyPerKWh is 0")
	}
	if b.EnergyCost != 0 {
		t.Errorf("EnergyCost = %v, want 0", b.EnergyCost)
	}
}

func TestForRecordNilCosts(t *testing.T) {
	r := history.Record{
		Filaments: []history.FilamentUse{
			{Slot: 0, Material: "PLA", Grams: 100},
			{Slot: 1, Material: "PETG", Grams: 50},
		},
	}
	b := ForRecord(nil, r)
	if !closeEnough(b.FilamentGrams, 150, 1e-6) {
		t.Errorf("FilamentGrams = %v, want 150", b.FilamentGrams)
	}
	if b.FilamentKnown {
		t.Errorf("FilamentKnown = true, want false with nil costs")
	}
	if b.EnergyKnown {
		t.Errorf("EnergyKnown = true, want false with nil costs")
	}
	if b.Total != 0 {
		t.Errorf("Total = %v, want 0", b.Total)
	}
}

func TestSummarize(t *testing.T) {
	c := &config.Costs{
		MaterialPerKg: map[string]float64{"PLA": 20},
		EnergyPerKWh:  0.30,
	}
	recent := []history.Record{
		{PrinterName: "Old name", EndTime: 200, Filaments: []history.FilamentUse{{Material: "PLA", Grams: 100}}, EnergyWh: wh(1000)},
		{PrinterName: "Old name", EndTime: 300, FilamentMM: 1000, Result: "cancelled"},
		{PrinterName: "Zed", EndTime: 400, Filaments: []history.FilamentUse{{Material: "PLA", Grams: 50}}, EnergyWh: wh(500)},
		{PrinterName: "Zed", EndTime: 100, Filaments: []history.FilamentUse{{Material: "PLA", Grams: 999}}}, // older than since: skipped
	}
	canon := func(name string) string {
		if name == "Old name" {
			return "Fox"
		}
		return name
	}
	s := Summarize(c, recent, 150, canon)

	if len(s.Printers) != 2 {
		t.Fatalf("len(Printers) = %d, want 2", len(s.Printers))
	}
	if s.Printers[0].Printer != "Fox" || s.Printers[1].Printer != "Zed" {
		t.Fatalf("printer order = %q, %q; want Fox, Zed", s.Printers[0].Printer, s.Printers[1].Printer)
	}

	fox := s.Printers[0].Total
	if fox.Prints != 2 {
		t.Errorf("Fox Prints = %d, want 2", fox.Prints)
	}
	if !closeEnough(fox.Grams, 100+2.9826, 1e-4) {
		t.Errorf("Fox Grams = %v, want ~102.9826", fox.Grams)
	}
	if !closeEnough(fox.KWh, 1.0, 1e-6) {
		t.Errorf("Fox KWh = %v, want 1.0", fox.KWh)
	}
	if !closeEnough(fox.Cost, 2.0+0.30, 1e-6) {
		t.Errorf("Fox Cost = %v, want 2.30", fox.Cost)
	}

	zed := s.Printers[1].Total
	if zed.Prints != 1 {
		t.Errorf("Zed Prints = %d, want 1", zed.Prints)
	}
	if !closeEnough(zed.Grams, 50, 1e-6) {
		t.Errorf("Zed Grams = %v, want 50", zed.Grams)
	}
	if !closeEnough(zed.KWh, 0.5, 1e-6) {
		t.Errorf("Zed KWh = %v, want 0.5", zed.KWh)
	}
	if !closeEnough(zed.Cost, 1.0+0.15, 1e-6) {
		t.Errorf("Zed Cost = %v, want 1.15", zed.Cost)
	}

	o := s.Overall
	if o.Prints != 3 {
		t.Errorf("Overall Prints = %d, want 3", o.Prints)
	}
	if !closeEnough(o.Grams, 150+2.9826, 1e-4) {
		t.Errorf("Overall Grams = %v, want ~152.9826", o.Grams)
	}
	if !closeEnough(o.KWh, 1.5, 1e-6) {
		t.Errorf("Overall KWh = %v, want 1.5", o.KWh)
	}
	if !closeEnough(o.Cost, 3.45, 1e-6) {
		t.Errorf("Overall Cost = %v, want 3.45", o.Cost)
	}

	empty := Summarize(nil, nil, 0, nil)
	if empty.Printers == nil || len(empty.Printers) != 0 {
		t.Errorf("Summarize(nil, nil, 0, nil).Printers = %#v, want a non-nil empty slice", empty.Printers)
	}
}
