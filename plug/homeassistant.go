package plug

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"foxtrack-bridge/config"
)

// --- Home Assistant (REST API, bearer token) ---

// haBase trims spaces and trailing "/" from an HA address and prefixes
// "http://" when no scheme is present.
func haBase(addr string) string {
	a := strings.TrimRight(strings.TrimSpace(addr), "/")
	if a == "" {
		return a
	}
	if !strings.Contains(a, "://") {
		a = "http://" + a
	}
	return a
}

var (
	errHAEntityToken = errors.New("Enter the Home Assistant switch entity (e.g. switch.printer_plug) and a long-lived access token.")
	errHAToken       = errors.New("Home Assistant refused the token. Create a long-lived access token in your Home Assistant profile.")
	errHAFormat      = errors.New("That address answers, but not like Home Assistant.")
	errHAUnavailable = errors.New("Home Assistant reports the plug as unavailable.")
	errHARefused     = errors.New("Home Assistant refused the switch command.")
)

// haState fetches one entity state from the HA REST API.
func haState(ctx context.Context, base, token, entity string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/api/states/"+entity, nil)
	if err != nil {
		return 0, nil, errNoAnswer
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, errNoAnswer
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, body, nil
}

// haPlugRead reads the switch entity and, when configured, the power sensor.
func haPlugRead(ctx context.Context, g config.Plug) (Reading, error) {
	if strings.TrimSpace(g.Entity) == "" || strings.TrimSpace(g.Token) == "" {
		return Reading{}, errHAEntityToken
	}
	base := haBase(g.Address)
	status, body, err := haState(ctx, base, g.Token, g.Entity)
	if err != nil {
		return Reading{}, err
	}
	if status == http.StatusUnauthorized {
		return Reading{}, errHAToken
	}
	if status == http.StatusNotFound {
		return Reading{}, fmt.Errorf("Home Assistant has no entity named %s.", g.Entity)
	}
	if status != http.StatusOK {
		return Reading{}, errHAFormat
	}
	var v struct {
		State string `json:"state"`
	}
	if json.Unmarshal(body, &v) != nil {
		return Reading{}, errHAFormat
	}
	switch v.State {
	case "on":
		// on
	case "off":
		// off
	case "unavailable", "unknown":
		return Reading{}, errHAUnavailable
	default:
		return Reading{}, errHAFormat
	}
	r := Reading{On: v.State == "on"}
	if pe := strings.TrimSpace(g.PowerEntity); pe != "" {
		if status, body, err := haState(ctx, base, g.Token, pe); err == nil && status == http.StatusOK {
			r.Watts = parseHAPower(body)
		}
	}
	return r, nil
}

// parseHAPower reads a power sensor state: a number in the state, scaled by
// 1000 when unit_of_measurement is "kW". Any failure gives nil.
func parseHAPower(body []byte) *float64 {
	var v struct {
		State      string `json:"state"`
		Attributes struct {
			Unit string `json:"unit_of_measurement"`
		} `json:"attributes"`
	}
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	var w float64
	if _, err := fmt.Sscanf(v.State, "%f", &w); err != nil {
		return nil
	}
	if strings.EqualFold(v.Attributes.Unit, "kW") {
		w *= 1000
	}
	return &w
}

// haPlugSwitch turns the switch entity on or off via the homeassistant
// turn_on / turn_off services.
func haPlugSwitch(ctx context.Context, g config.Plug, on bool) error {
	if strings.TrimSpace(g.Entity) == "" || strings.TrimSpace(g.Token) == "" {
		return errHAEntityToken
	}
	svc := "turn_off"
	if on {
		svc = "turn_on"
	}
	payload := []byte(`{"entity_id":` + mustQuote(g.Entity) + `}`)
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", haBase(g.Address)+"/api/services/homeassistant/"+svc, bytes.NewReader(payload))
	if err != nil {
		return errNoAnswer
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return errNoAnswer
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusUnauthorized:
		return errHAToken
	default:
		return errHARefused
	}
}

// mustQuote renders s as a JSON string literal.
func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
