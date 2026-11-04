package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/history"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/mqttexport"
	"foxtrack-bridge/version"
)

// Prometheus metrics at /metrics, in the text format (version 0.0.4) written
// by hand: a client library would be a lot of code for a dozen gauges. It is
// off until the user turns it on.

func init() {
	http.HandleFunc("/metrics", handleMetrics)
	http.HandleFunc("/api/metrics", handleMetricsSettings)
}

func metricsEnabled() bool {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configStore != nil && configStore.Metrics != nil && configStore.Metrics.Enabled
}

func handleMetricsSettings(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		json.NewEncoder(w).Encode(map[string]bool{"enabled": metricsEnabled()})
	case "POST":
		if !requireJSON(w, r) {
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, "That request was not valid JSON.", http.StatusBadRequest)
			return
		}
		if err := mutateConfig(func(c *config.Config) error {
			c.Metrics = &config.Metrics{Enabled: req.Enabled}
			return nil
		}); err != nil {
			writeSaveFailed(w, err)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"enabled": req.Enabled})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !metricsEnabled() {
		http.Error(w, "Metrics are turned off. Turn them on in Settings, under Prometheus.", http.StatusNotFound)
		return
	}
	configMutex.RLock()
	printers := append([]config.Printer(nil), configStore.Printers...)
	configMutex.RUnlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, renderMetrics(version.AppVersion, printers, liveStates(), printStatsByPrinter(printers, time.Now())))
}

// printStats is one printer's totals from history.
type printStats struct {
	Jobs     map[string]int // by result: finished, cancelled, error
	Seconds  int64
	EnergyWh float64
}

var metricsHistory = struct {
	sync.Mutex
	at    time.Time
	stats map[string]printStats // by printer name
}{}

// loadHistory is history.Load, replaceable in tests.
var loadHistory = history.Load

// printStatsByPrinter totals history per printer, cached for 30 seconds so a
// frequent scrape does not read the file each time. Records made under an
// earlier name count for the printer's current one.
func printStatsByPrinter(printers []config.Printer, now time.Time) map[string]printStats {
	metricsHistory.Lock()
	defer metricsHistory.Unlock()
	if metricsHistory.stats != nil && now.Sub(metricsHistory.at) < 30*time.Second {
		return metricsHistory.stats
	}
	current := map[string]string{} // any name a printer had -> its current name
	for _, p := range printers {
		current[p.Name] = p.Name
		for _, old := range p.PreviousNames {
			if _, taken := current[old]; !taken {
				current[old] = p.Name
			}
		}
	}
	records, err := loadHistory()
	if err != nil {
		// Zeros would look like a counter reset to Prometheus: keep the last
		// totals and try again on the next scrape.
		log.Printf("[metrics] print history not readable: %v", err)
		return metricsHistory.stats
	}
	stats := map[string]printStats{}
	for _, rec := range records {
		name, ok := current[rec.PrinterName]
		if !ok {
			continue
		}
		s := stats[name]
		if s.Jobs == nil {
			s.Jobs = map[string]int{}
		}
		s.Jobs[rec.Result]++
		s.Seconds += rec.Duration
		if rec.EnergyWh != nil {
			s.EnergyWh += *rec.EnergyWh
		}
		stats[name] = s
	}
	metricsHistory.at, metricsHistory.stats = now, stats
	return stats
}

// metricLabelValue escapes a label value: backslash, double quote and newline.
func metricLabelValue(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func metricNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// renderMetrics writes every metric family. A printer that is not reporting
// gets only up and printing: its other numbers would be stale, and a missing
// series is more honest than a zero.
func renderMetrics(appVersion string, printers []config.Printer, states map[string]*mqttpkg.TelemetryData, stats map[string]printStats) string {
	type sample struct{ labels, value string }
	type family struct {
		name, typ, help string
		samples         []sample
	}
	fams := []*family{
		{name: "foxtrack_bridge_info", typ: "gauge", help: "The Bridge version, as a label. Always 1."},
		{name: "foxtrack_printer_up", typ: "gauge", help: "1 when the printer is reporting to the Bridge, 0 when it is not."},
		{name: "foxtrack_printer_printing", typ: "gauge", help: "1 while the printer is printing, 0 otherwise."},
		{name: "foxtrack_printer_progress_ratio", typ: "gauge", help: "Progress of the current print, from 0 to 1."},
		{name: "foxtrack_printer_nozzle_temperature_celsius", typ: "gauge", help: "Nozzle temperature in degrees Celsius."},
		{name: "foxtrack_printer_nozzle_target_temperature_celsius", typ: "gauge", help: "Nozzle target temperature in degrees Celsius."},
		{name: "foxtrack_printer_bed_temperature_celsius", typ: "gauge", help: "Bed temperature in degrees Celsius."},
		{name: "foxtrack_printer_bed_target_temperature_celsius", typ: "gauge", help: "Bed target temperature in degrees Celsius."},
		{name: "foxtrack_printer_time_remaining_seconds", typ: "gauge", help: "Estimated seconds left in the current print."},
		{name: "foxtrack_printer_alarms", typ: "gauge", help: "Alarms the printer reports right now, not counting info-level ones."},
		{name: "foxtrack_printer_print_jobs_total", typ: "counter", help: "Prints that ended, by result, from the Bridge's history."},
		{name: "foxtrack_printer_print_seconds_total", typ: "counter", help: "Seconds spent printing, from the Bridge's history."},
		{name: "foxtrack_printer_energy_wh_total", typ: "counter", help: "Watt-hours used while printing, from the printer's smart plug."},
	}
	f := map[string]*family{}
	for _, x := range fams {
		f[x.name] = x
	}
	add := func(name, labels string, v float64) {
		f[name].samples = append(f[name].samples, sample{labels, metricNumber(v)})
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	add("foxtrack_bridge_info", `version="`+metricLabelValue(appVersion)+`"`, 1)
	for _, p := range printers {
		t := states[p.Name]
		model := p.KindLabel()
		if t != nil && t.PrinterModel != "" {
			model = t.PrinterModel
		}
		l := `printer="` + metricLabelValue(p.Name) + `",printer_id="` + metricLabelValue(p.ID) +
			`",model="` + metricLabelValue(model) + `",kind="` + metricLabelValue(p.Kind()) + `"`
		up := t != nil && mqttexport.Reporting(t.Status)
		add("foxtrack_printer_up", l, b2f(up))
		add("foxtrack_printer_printing", l, b2f(t != nil && t.Status == "printing"))
		if up {
			alarms, _ := mqttexport.CountAlarms(t.Alarms)
			add("foxtrack_printer_progress_ratio", l, float64(t.Progress)/100)
			add("foxtrack_printer_nozzle_temperature_celsius", l, t.NozzleTemp)
			add("foxtrack_printer_nozzle_target_temperature_celsius", l, t.NozzleTarget)
			add("foxtrack_printer_bed_temperature_celsius", l, t.BedTemp)
			add("foxtrack_printer_bed_target_temperature_celsius", l, t.BedTarget)
			add("foxtrack_printer_time_remaining_seconds", l, float64(t.TimeRemaining)*60)
			add("foxtrack_printer_alarms", l, float64(alarms))
		}
		s := stats[p.Name]
		for _, result := range []string{"finished", "cancelled", "error"} {
			add("foxtrack_printer_print_jobs_total", l+`,result="`+result+`"`, float64(s.Jobs[result]))
		}
		add("foxtrack_printer_print_seconds_total", l, float64(s.Seconds))
		if p.HasPlug() || s.EnergyWh > 0 {
			add("foxtrack_printer_energy_wh_total", l, s.EnergyWh)
		}
	}

	var b strings.Builder
	for _, x := range fams {
		if len(x.samples) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", x.name, x.help, x.name, x.typ)
		for _, s := range x.samples {
			fmt.Fprintf(&b, "%s{%s} %s\n", x.name, s.labels, s.value)
		}
	}
	return b.String()
}
