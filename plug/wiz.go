package plug

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"time"
)

// --- Philips WiZ (UDP 38899, the pywizlight protocol) ---
//
// One JSON datagram per request and reply: getPilot carries the relay state
// (and, on metered plugs, the power in milliwatts as "pc"); getPower is the
// dedicated power query; setPilot switches. A plug without a meter answers
// getPower with a "Method not found" error, so the power falls back to the
// pilot's "pc". WiZ has no lifetime energy counter, so EnergyWh stays nil.

const (
	wizDefaultPort = "38899"
	wizResendEvery = 500 * time.Millisecond
	wizPowerBudget = 1500 * time.Millisecond
	wizReplySize   = 4 << 10
)

var (
	errWizNotAPlug = errors.New("That address answers, but not like a WiZ plug.")
	errWizRefused  = errors.New("The WiZ plug refused the switch command.")
)

// wizHost fills in the default port 38899 when the address has none.
func wizHost(host string) string {
	if _, _, err := net.SplitHostPort(host); err != nil {
		return net.JoinHostPort(host, wizDefaultPort)
	}
	return host
}

// wizExchange sends one JSON datagram to the plug and waits for its reply,
// resending every 500 ms until the deadline: UDP drops packets. The deadline
// is the sooner of the caller's context deadline and now + requestTimeout.
// A reply carrying an "error" object is returned as an error with its message.
func wizExchange(ctx context.Context, host string, req []byte) ([]byte, error) {
	host = wizHost(host)
	d := net.Dialer{Timeout: requestTimeout}
	conn, err := d.DialContext(ctx, "udp", host)
	if err != nil {
		return nil, errNoAnswer
	}
	defer conn.Close()

	deadline := time.Now().Add(requestTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write(req); err != nil {
		return nil, errNoAnswer
	}
	nextResend := time.Now().Add(wizResendEvery)
	for {
		if !nextResend.Before(deadline) {
			_ = conn.SetReadDeadline(deadline)
		} else {
			_ = conn.SetReadDeadline(nextResend)
		}
		buf := make([]byte, wizReplySize)
		n, err := conn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if time.Now().Before(deadline) {
					if _, werr := conn.Write(req); werr != nil {
						return nil, errNoAnswer
					}
					nextResend = time.Now().Add(wizResendEvery)
					continue
				}
				return nil, errNoAnswer
			}
			return nil, errNoAnswer
		}
		var rep struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(buf[:n], &rep) == nil && rep.Error != nil {
			return nil, errors.New(rep.Error.Message)
		}
		return buf[:n], nil
	}
}

// wizRead polls one plug: getPilot for the state, then getPower (with its own
// 1.5 s budget) for the draw, falling back to the pilot's "pc" in milliwatts
// when the plug has no meter.
func wizRead(ctx context.Context, host string) (Reading, error) {
	pilot, err := wizExchange(ctx, host, []byte(`{"method":"getPilot","params":{}}`))
	if err != nil {
		return Reading{}, err
	}
	var p struct {
		Result *struct {
			State *bool    `json:"state"`
			Pc    *float64 `json:"pc"`
		} `json:"result"`
	}
	if err := json.Unmarshal(pilot, &p); err != nil || p.Result == nil || p.Result.State == nil {
		return Reading{}, errWizNotAPlug
	}
	r := Reading{On: *p.Result.State}

	powerCtx, cancel := context.WithTimeout(ctx, wizPowerBudget)
	defer cancel()
	if power, err := wizExchange(powerCtx, host, []byte(`{"method":"getPower","params":{}}`)); err == nil {
		var pw struct {
			Result *struct {
				Power *float64 `json:"power"`
			} `json:"result"`
		}
		if json.Unmarshal(power, &pw) == nil && pw.Result != nil && pw.Result.Power != nil {
			w := *pw.Result.Power / 1000
			r.Watts = &w
			return r, nil
		}
	}
	if p.Result.Pc != nil {
		w := *p.Result.Pc / 1000
		r.Watts = &w
	}
	return r, nil
}

// wizSwitch sends setPilot and accepts only a success reply.
func wizSwitch(ctx context.Context, host string, on bool) error {
	req := `{"method":"setPilot","params":{"state":` + strconv.FormatBool(on) + `}}`
	body, err := wizExchange(ctx, host, []byte(req))
	if err != nil {
		return err
	}
	var rep struct {
		Result *struct {
			Success bool `json:"success"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &rep) != nil || rep.Result == nil || !rep.Result.Success {
		return errWizRefused
	}
	return nil
}
