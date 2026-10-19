package lan

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

// errReadOnly answers every command and print sent to a Home Assistant printer.
var errReadOnly = errors.New("Home Assistant printers are read-only")

// fetchHomeAssistantTelemetry reads the entities the user mapped for this
// printer from the Home Assistant REST API. Only the state entity is required;
// a missing or failing optional entity leaves its value empty.
func fetchHomeAssistantTelemetry(p configpkg.Printer) (mqttpkg.TelemetryData, webhook.RelayPayload, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: insecureTransport()}
	base := strings.TrimRight(strings.TrimSpace(p.HomeAssistantURL), "/")
	headers := map[string]string{"Authorization": "Bearer " + strings.TrimSpace(p.HomeAssistantToken)}
	get := func(entity string) (state, unit string, err error) {
		entity = strings.TrimSpace(entity)
		if entity == "" {
			return "", "", errors.New("no entity set")
		}
		m, err := sendJSONRequest(client, "GET", base+"/api/states/"+url.PathEscape(entity), headers, nil)
		if err != nil {
			return "", "", err
		}
		return stringAny(m["state"]), stringAny(nestedMap(m, "attributes")["unit_of_measurement"]), nil
	}

	raw, _, err := get(p.HAStateEntity)
	if err != nil {
		return mqttpkg.TelemetryData{}, webhook.RelayPayload{}, err
	}
	t := mqttpkg.TelemetryData{Status: normalizeState(raw), PrinterModel: "Home Assistant"}
	if t.Status == "disconnected" {
		// Home Assistant answered but has lost the printer: report it offline.
		return t, webhook.RelayPayload{}, fmt.Errorf("Home Assistant reports %s as %q", p.HAStateEntity, raw)
	}
	if s, _, err := get(p.HAProgressEntity); err == nil {
		t.Progress = clampPercent(haNumber(s))
	}
	if s, _, err := get(p.HANozzleEntity); err == nil {
		t.NozzleTemp = haNumber(s)
	}
	if s, _, err := get(p.HABedEntity); err == nil {
		t.BedTemp = haNumber(s)
	}
	if s, _, err := get(p.HAFileEntity); err == nil && normalizeState(s) != "disconnected" {
		t.FileName = s
	}
	if s, unit, err := get(p.HARemainingEntity); err == nil {
		t.TimeRemaining = haMinutes(s, unit, time.Now())
	}
	return t, relayFor(t, configpkg.KindHomeAssistant, nil), nil
}

// haNumber reads a numeric entity state; "unavailable" and the like read as 0.
func haNumber(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// haMinutes reads a time-remaining entity as whole minutes. A duration is
// scaled by its unit (seconds, minutes or hours; no unit means minutes), and a
// timestamp entity (a "print finish" time) counts down to that time.
func haMinutes(state, unit string, now time.Time) int {
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(state)); err == nil {
		return max(0, int(at.Sub(now).Minutes()))
	}
	v := haNumber(state)
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "s", "sec", "seconds":
		v /= 60
	case "h", "hr", "hours":
		v *= 60
	}
	return max(0, int(v))
}
