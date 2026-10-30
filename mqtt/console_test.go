package mqtt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConsoleGcode_MapsCommands(t *testing.T) {
	cases := []struct {
		cmd  string
		args map[string]interface{}
		want string
	}{
		{"home", nil, "G28"},
		{"set_nozzle_temp", map[string]interface{}{"celsius": 210.0}, "M104 S210"},
		{"set_nozzle_temp", map[string]interface{}{"celsius": 0.0}, "M104 S0"},
		{"set_nozzle_temp", map[string]interface{}{"celsius": "320"}, "M104 S320"},
		{"set_bed_temp", map[string]interface{}{"celsius": 60.0}, "M140 S60"},
		{"set_bed_temp", map[string]interface{}{"celsius": json.Number("120")}, "M140 S120"},
	}
	for _, tc := range cases {
		got, err := ConsoleGcode(tc.cmd, tc.args)
		if err != nil || got != tc.want {
			t.Errorf("%s %v: got %q, %v; want %q", tc.cmd, tc.args, got, err, tc.want)
		}
	}
}

func TestConsoleGcode_RefusesOutOfRange(t *testing.T) {
	bad := []struct {
		cmd  string
		args map[string]interface{}
	}{
		{"set_nozzle_temp", map[string]interface{}{"celsius": 321.0}},
		{"set_bed_temp", map[string]interface{}{"celsius": 121.0}},
		{"set_bed_temp", map[string]interface{}{"celsius": -1.0}},
		{"set_bed_temp", map[string]interface{}{"celsius": 60.5}},
		{"set_bed_temp", map[string]interface{}{"celsius": "hot"}},
		{"set_bed_temp", map[string]interface{}{"celsius": true}},
		{"set_bed_temp", map[string]interface{}{}},
		{"set_bed_temp", nil},
	}
	for _, tc := range bad {
		if _, err := ConsoleGcode(tc.cmd, tc.args); err == nil {
			t.Errorf("%s %v: want a refusal", tc.cmd, tc.args)
		} else if !strings.Contains(err.Error(), "whole number from 0 to") {
			t.Errorf("%s %v: error %q is not the plain sentence", tc.cmd, tc.args, err)
		}
	}
	if _, err := ConsoleGcode("pause", nil); err == nil {
		t.Fatal("pause is not a console command")
	}
	if IsConsoleCommand("pause") || !IsConsoleCommand("home") || !IsConsoleCommand("set_bed_temp") {
		t.Fatal("IsConsoleCommand")
	}
	if ConsoleMaxC("set_nozzle_temp") != 320 || ConsoleMaxC("set_bed_temp") != 120 || ConsoleMaxC("home") != 0 {
		t.Fatal("ConsoleMaxC")
	}
}

func TestConsolePayload_IsGcodeLine(t *testing.T) {
	got, err := consolePayload("set_bed_temp", map[string]interface{}{"celsius": 60.0})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]interface{}
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	if m["print"]["command"] != "gcode_line" || m["print"]["param"] != "M140 S60" {
		t.Fatalf("payload = %s", got)
	}
	if _, err := consolePayload("set_bed_temp", map[string]interface{}{"celsius": 200.0}); err == nil {
		t.Fatal("200 C bed must be refused before any publish")
	}
}
