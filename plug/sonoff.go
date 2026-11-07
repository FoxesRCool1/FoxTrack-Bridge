package plug

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var (
	errSonoffNotSonoff = errors.New("That address answers, but not like a Sonoff in DIY mode.")
	errSonoffEWeLink   = errors.New("This Sonoff is in eWeLink mode, which encrypts its local traffic. Put it in DIY mode (see the plug help), or use Home Assistant.")
	errSonoffNoState   = errors.New("The Sonoff answered without an on/off state. DIY mode needs firmware 3.5 or later.")
)

// sonoffAddr returns host with port 8081, the DIY mode port, when the address
// has no port. A host typed without a port would otherwise hit port 80.
func sonoffAddr(host string) string {
	if _, _, err := net.SplitHostPort(host); err != nil {
		bare := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		return net.JoinHostPort(bare, "8081")
	}
	return host
}

// sonoffPost POSTs data to the zeroconf endpoint of a Sonoff in DIY mode and
// returns the reply's "data". A host without a port uses 8081 (see sonoffAddr).
func sonoffPost(ctx context.Context, host, path string, data any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"deviceid": "", "data": data})
	if err != nil {
		return nil, errSonoffNotSonoff
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+sonoffAddr(host)+"/zeroconf"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errNoAnswer
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, errSonoffNotSonoff
	}
	var rep struct {
		Error int             `json:"error"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, errSonoffNotSonoff
	}
	switch rep.Error {
	case 0:
		return rep.Data, nil
	case 401:
		return nil, errSonoffEWeLink
	default:
		return nil, fmt.Errorf("The Sonoff refused the request (error %d).", rep.Error)
	}
}

// sonoffInfo posts /zeroconf/info and returns the reply's data.
func sonoffInfo(ctx context.Context, host string) (json.RawMessage, error) {
	return sonoffPost(ctx, host, "/info", map[string]any{})
}

// sonoffState reads the on/off state from an /zeroconf/info reply: the
// "switch" field, or outlet 0 of "switches" on multi-outlet firmware.
// It reports whether a state was present.
func sonoffState(info json.RawMessage) (bool, bool) {
	var single struct {
		Switch *string `json:"switch"`
	}
	var multi struct {
		Switches []struct {
			Switch *string `json:"switch"`
			Outlet *int    `json:"outlet"`
		} `json:"switches"`
	}
	if err := json.Unmarshal(info, &multi); err == nil && len(multi.Switches) > 0 {
		for _, s := range multi.Switches {
			if s.Switch != nil && (s.Outlet == nil || *s.Outlet == 0) {
				return *s.Switch == "on", true
			}
		}
		return false, false
	}
	if err := json.Unmarshal(info, &single); err == nil && single.Switch != nil {
		return *single.Switch == "on", true
	}
	return false, false
}

// sonoffRead polls a Sonoff in DIY mode. It reports the relay state only;
// DIY mode has no power meter, so Watts and EnergyWh stay nil.
func sonoffRead(ctx context.Context, host string) (Reading, error) {
	info, err := sonoffInfo(ctx, host)
	if err != nil {
		return Reading{}, err
	}
	on, ok := sonoffState(info)
	if !ok {
		return Reading{}, errSonoffNoState
	}
	return Reading{On: on}, nil
}

// sonoffSwitch turns the relay on or off. It first reads /zeroconf/info to
// choose the endpoint: "switch" for single outlets, "switches" with outlet 0
// for the multi-outlet firmware.
func sonoffSwitch(ctx context.Context, host string, on bool) error {
	state := "off"
	if on {
		state = "on"
	}
	info, err := sonoffInfo(ctx, host)
	if err != nil {
		return err
	}
	// The device wants at least 200 ms between requests.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(250 * time.Millisecond):
	}
	var multi struct {
		Switches []any `json:"switches"`
	}
	if err := json.Unmarshal(info, &multi); err == nil && len(multi.Switches) > 0 {
		_, err = sonoffPost(ctx, host, "/switches", map[string]any{
			"switches": []map[string]any{{"switch": state, "outlet": 0}},
		})
		return err
	}
	_, err = sonoffPost(ctx, host, "/switch", map[string]any{"switch": state})
	return err
}
