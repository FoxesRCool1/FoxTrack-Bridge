package mqtt

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The printer console (Bridge 2.8, FoxTrack migration 20261031000290): the
// nozzle and bed targets and homing, sent by FoxTrack as bridge_commands
// set_nozzle_temp / set_bed_temp (args {"celsius": n}) and home. Every driver
// maps them through ConsoleGcode or its own API; the range check lives here
// so each kind refuses the same way.

const (
	NozzleMaxC = 350 // H2D-class hot ends; the printer's own firmware enforces its limit too
	BedMaxC    = 120
)

// IsConsoleCommand reports whether command is one of the console commands.
func IsConsoleCommand(command string) bool {
	switch command {
	case "set_nozzle_temp", "set_bed_temp", "home":
		return true
	}
	return false
}

// ConsoleMaxC is the highest target a console command takes, 0 for home.
func ConsoleMaxC(command string) int {
	switch command {
	case "set_nozzle_temp":
		return NozzleMaxC
	case "set_bed_temp":
		return BedMaxC
	}
	return 0
}

// CelsiusArg reads args["celsius"] (a JSON number, or a numeric string from
// the dashboard) as a whole number from 0 to max. The error is shown to the
// FoxTrack user as is.
func CelsiusArg(args map[string]interface{}, max int) (int, error) {
	bad := fmt.Errorf("The temperature must be a whole number from 0 to %d.", max)
	if args == nil {
		return 0, bad
	}
	var f float64
	switch v := args["celsius"].(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case json.Number:
		p, err := v.Float64()
		if err != nil {
			return 0, bad
		}
		f = p
	case string:
		p, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, bad
		}
		f = p
	default:
		return 0, bad
	}
	if math.IsNaN(f) || f != math.Trunc(f) || f < 0 || f > float64(max) {
		return 0, bad
	}
	return int(f), nil
}

// ConsoleGcode is the G-code line for a console command: M104 (nozzle), M140
// (bed), G28 (home). 0 turns a heater off.
func ConsoleGcode(command string, args map[string]interface{}) (string, error) {
	switch command {
	case "home":
		return "G28", nil
	case "set_nozzle_temp":
		c, err := CelsiusArg(args, NozzleMaxC)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("M104 S%d", c), nil
	case "set_bed_temp":
		c, err := CelsiusArg(args, BedMaxC)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("M140 S%d", c), nil
	}
	return "", fmt.Errorf("not a console command: %q", command)
}

// consolePayload is the Bambu MQTT request for a console command: the
// gcode_line command the GCode console already uses.
func consolePayload(command string, args map[string]interface{}) (string, error) {
	line, err := ConsoleGcode(command, args)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(map[string]interface{}{
		"print": map[string]interface{}{
			"sequence_id": "0",
			"command":     "gcode_line",
			"param":       line,
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to build %s payload: %w", command, err)
	}
	return string(b), nil
}
