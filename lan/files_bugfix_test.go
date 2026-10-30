package lan

import (
	"strings"
	"testing"

	configpkg "foxtrack-bridge/config"
)

func TestParseOctoPrintFiles_RecursiveMachinecodeOnly(t *testing.T) {
	got := parseOctoPrintFiles(mustJSON(t, `{"files":[
		{"name":"model.stl","path":"model.stl","type":"model","size":9},
		{"name":"sub","path":"sub","type":"folder","children":[
			{"name":"in.gcode","path":"sub/in.gcode","type":"machinecode","size":5,"date":1700000000},
			{"name":"part.stl","path":"sub/part.stl","type":"model"}
		]}
	]}`))
	if len(got) != 1 || got[0].Name != "sub/in.gcode" {
		t.Fatalf("got %+v", got)
	}
}

func TestPrintFile_OnlyRefusesDotDotSegments(t *testing.T) {
	var got string
	srv := recordingServer(t, &got)
	defer srv.Close()
	c := NewController()
	c.printers["Voron"] = configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL}
	if err := c.PrintFile("Voron", "v1..2.gcode"); err != nil || !strings.Contains(got, "filename=v1..2.gcode") {
		t.Fatalf("v1..2.gcode: err=%v sent=%q", err, got)
	}
	if err := c.PrintFile("Voron", "sub/../x.gcode"); err == nil {
		t.Fatal("a .. segment must be refused")
	}
}
