package plug

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

var (
	errMyStrom      = errors.New("That address answers, but not like a myStrom plug.")
	errMyStromToken = errors.New("The myStrom plug wants an API token. Copy it from the myStrom app (device settings, Security) into the plug settings.")
)

// mystromGet is getJSON plus the optional API token a secured myStrom plug
// wants in a "Token" header (as python-mystrom sends it).
func mystromGet(ctx context.Context, u, token string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0, nil, errBadAddress
	}
	if token != "" {
		req.Header.Set("Token", token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, errNoAnswer
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, nil, errMyStromToken
	}
	return resp.StatusCode, body, nil
}

// mystromRead polls a myStrom WiFi Switch. The plug serves a JSON report at
// /report; the relay state is the on/off state and power the current draw.
// It has no lifetime energy counter, so EnergyWh stays nil.
func mystromRead(ctx context.Context, host, token string) (Reading, error) {
	status, body, err := mystromGet(ctx, "http://"+host+"/report", token)
	if err != nil {
		return Reading{}, err
	}
	if status != http.StatusOK {
		return Reading{}, errMyStrom
	}
	var rep struct {
		Relay *bool    `json:"relay"`
		Power *float64 `json:"power"`
	}
	if json.Unmarshal(body, &rep) != nil || rep.Relay == nil {
		return Reading{}, errMyStrom
	}
	return Reading{On: *rep.Relay, Watts: rep.Power}, nil
}

// mystromSwitch sets the relay with GET /relay?state=1|0.
func mystromSwitch(ctx context.Context, host, token string, on bool) error {
	state := "0"
	if on {
		state = "1"
	}
	status, _, err := mystromGet(ctx, "http://"+host+"/relay?state="+state, token)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errors.New("The myStrom plug refused the switch command.")
	}
	return nil
}
