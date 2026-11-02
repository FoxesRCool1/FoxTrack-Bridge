package lan

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

func mustJSON(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseMoonrakerFiles(t *testing.T) {
	got := parseMoonrakerFiles(mustJSON(t, `{"result":[
		{"path":"old.gcode","modified":1700000000.5,"size":123,"permissions":"rw"},
		{"path":"sub/new.gcode","modified":1760000000,"size":456},
		{"modified":1,"size":1}
	]}`))
	if len(got) != 2 || got[0].Name != "old.gcode" || got[0].SizeBytes != 123 || got[0].ModifiedAt != "2023-11-14T22:13:20Z" {
		t.Fatalf("got %+v", got)
	}
	sorted := mqttpkg.NewestFirst(got, 0)
	if sorted[0].Name != "sub/new.gcode" {
		t.Fatalf("newest first: %+v", sorted)
	}
}

func TestParseOctoPrintFiles_SkipsFolders(t *testing.T) {
	got := parseOctoPrintFiles(mustJSON(t, `{"files":[
		{"name":"folder","type":"folder","children":[]},
		{"name":"cube.gcode","display":"Cube.gcode","size":2048,"date":1700000000,"type":"machinecode","origin":"local"}
	],"free":1}`))
	if len(got) != 1 || got[0].Name != "cube.gcode" || got[0].SizeBytes != 2048 || got[0].ModifiedAt != "2023-11-14T22:13:20Z" {
		t.Fatalf("got %+v", got)
	}
}

func TestListFiles_AndPrintFile_PerKind(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.RequestURI() + " " + string(b)
		switch r.URL.Path {
		case "/server/files/list":
			_, _ = w.Write([]byte(`{"result":[{"path":"a.gcode","modified":1760000000,"size":1}]}`))
		case "/api/files/local":
			if r.URL.Query().Get("recursive") != "true" {
				t.Errorf("OctoPrint list must be recursive: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"files":[{"name":"b.gcode","date":1760000000,"size":2,"type":"machinecode"}]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	c := NewController()
	c.printers["Voron"] = configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL}
	c.printers["Ender"] = configpkg.Printer{Name: "Ender", OctoPrintURL: srv.URL, APIKey: "k"}
	c.printers["MK4"] = configpkg.Printer{Name: "MK4", PrusaLinkURL: srv.URL, APIKey: "k"}

	files, err := c.ListFiles("Voron")
	if err != nil || len(files) != 1 || files[0].Name != "a.gcode" {
		t.Fatalf("Voron: %+v %v", files, err)
	}
	files, err = c.ListFiles("Ender")
	if err != nil || len(files) != 1 || files[0].Name != "b.gcode" {
		t.Fatalf("Ender: %+v %v", files, err)
	}
	if _, err := c.ListFiles("MK4"); err != errNoFileList {
		t.Fatalf("MK4: %v", err)
	}

	if err := c.PrintFile("Voron", "a.gcode"); err != nil || got != "POST /printer/print/start?filename=a.gcode " {
		t.Fatalf("Voron print: %v %q", err, got)
	}
	if err := c.PrintFile("Ender", "sub dir/b.gcode"); err != nil || got != `POST /api/files/local/sub%20dir/b.gcode {"command":"select","print":true}` {
		t.Fatalf("Ender print: %v %q", err, got)
	}
	if err := c.PrintFile("Ender", "../b.gcode"); err == nil {
		t.Fatal("a path with .. must be refused")
	}
}

func TestPrintFile_RefusedWhilePrinting(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true; _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c := NewController()
	c.printers["Voron"] = configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL}
	c.states["Voron"] = &mqttpkg.TelemetryData{Status: "printing"}
	if err := c.PrintFile("Voron", "a.gcode"); err != errPrinterBusy || hit {
		t.Fatalf("err=%v hit=%v", err, hit)
	}
	c.states["Voron"] = &mqttpkg.TelemetryData{Status: "paused"}
	if err := c.PrintFile("Voron", "a.gcode"); err != errPrinterBusy || hit {
		t.Fatalf("paused: err=%v hit=%v", err, hit)
	}
}
