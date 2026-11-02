package mqtt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"foxtrack-bridge/ftps"
)

func TestNewestFirst_SortsAndCaps(t *testing.T) {
	in := []PrinterFile{
		{Name: "b.3mf", ModifiedAt: "2026-10-01T00:00:00Z"},
		{Name: "a.3mf", ModifiedAt: "2026-10-29T00:00:00Z"},
		{Name: "c.3mf", ModifiedAt: "2026-10-29T00:00:00Z"},
	}
	got := NewestFirst(in, 2)
	if len(got) != 2 || got[0].Name != "a.3mf" || got[1].Name != "c.3mf" {
		t.Fatalf("got %+v", got)
	}
	if in[0].Name != "b.3mf" {
		t.Fatal("input must not be reordered")
	}
}

func TestListPrinterFiles_RootAndCache3mfOnly(t *testing.T) {
	old := ftpsList
	defer func() { ftpsList = old }()
	ftpsList = func(_ context.Context, _ ftps.Config, dir string) ([]ftps.Entry, error) {
		switch dir {
		case "/":
			return []ftps.Entry{
				{Name: "part.gcode.3mf", Size: 100, Modified: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
				{Name: "readme.txt", Size: 5, Modified: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)},
			}, nil
		case "/cache":
			return []ftps.Entry{
				{Name: "newer.3mf", Size: 200, Modified: time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)},
			}, nil
		}
		return nil, errors.New("no such dir")
	}
	got, err := ListPrinterFiles(context.Background(), Printer{Name: "P1S", IP: "10.0.0.5", LANCode: "x", Serial: "01P00A000000009"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "cache/newer.3mf" || got[0].ModifiedAt != "2026-10-20T08:30:00Z" || got[1].Name != "part.gcode.3mf" {
		t.Fatalf("got %+v", got)
	}

	// No cache folder: the root listing still answers.
	ftpsList = func(_ context.Context, _ ftps.Config, dir string) ([]ftps.Entry, error) {
		if dir == "/" {
			return []ftps.Entry{{Name: "a.3mf", Size: 1}}, nil
		}
		return nil, fmt.Errorf("%w: 550 Failed to open directory", ftps.ErrNotFound)
	}
	got, err = ListPrinterFiles(context.Background(), Printer{Name: "P1S", IP: "10.0.0.5", LANCode: "x"})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v %v", got, err)
	}

	// A refused access code is the access-code sentence.
	ftpsList = func(context.Context, ftps.Config, string) ([]ftps.Entry, error) { return nil, ftps.ErrLogin }
	if _, err := ListPrinterFiles(context.Background(), Printer{Name: "P1S", IP: "10.0.0.5"}); err != errPrintAccess {
		t.Fatalf("err = %v", err)
	}
	// Cloud: never listed.
	if _, err := ListPrinterFiles(context.Background(), Printer{Name: "C", Cloud: true}); err != errPrintCloud {
		t.Fatalf("err = %v", err)
	}
}

func TestPrintPrinterFile_RefusesBadNamesBeforeAnyPublish(t *testing.T) {
	published := false
	old := publishRequestFn
	defer func() { publishRequestFn = old }()
	publishRequestFn = func(string, string, []byte) error { published = true; return nil }
	for _, name := range []string{"", "../etc", "cache/notes.txt", "   "} {
		err := PrintPrinterFile(context.Background(), Printer{Name: "none", Serial: "01P00A000000009"}, name, 0)
		if err == nil || published {
			t.Fatalf("%q: err=%v published=%v", name, err, published)
		}
	}
	// A good name on a printer with no MQTT session is refused by the preflight, not published.
	err := PrintPrinterFile(context.Background(), Printer{Name: "no-session-printer", Serial: "01P00A000000009"}, "cache/part.3mf", 0)
	if err == nil || published {
		t.Fatalf("err=%v published=%v", err, published)
	}
}

func TestListPrinterFiles_CacheErrorIsNotSwallowed(t *testing.T) {
	old := ftpsList
	defer func() { ftpsList = old }()
	ftpsList = func(_ context.Context, _ ftps.Config, dir string) ([]ftps.Entry, error) {
		if dir == "/" {
			return []ftps.Entry{{Name: "a.3mf", Size: 1}}, nil
		}
		return nil, errors.New("read listing: connection reset")
	}
	// A dropped link on /cache is not "no cache folder": no partial list.
	if got, err := ListPrinterFiles(context.Background(), Printer{Name: "P1S", IP: "10.0.0.5"}); err == nil {
		t.Fatalf("got %+v with no error", got)
	}
	// A deadline stays a deadline, so the command can say the printer did not answer.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ftpsList = func(context.Context, ftps.Config, string) ([]ftps.Entry, error) { return nil, errors.New("dial") }
	if _, err := ListPrinterFiles(ctx, Printer{Name: "P1S", IP: "10.0.0.5"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestListPrinterFiles_A1RetriesWithClearDataChannel(t *testing.T) {
	old := ftpsList
	defer func() { ftpsList = old }()
	var clear []bool
	ftpsList = func(_ context.Context, cfg ftps.Config, dir string) ([]ftps.Entry, error) {
		clear = append(clear, cfg.ClearData)
		if !cfg.ClearData {
			return nil, ftps.ErrStalled
		}
		return []ftps.Entry{{Name: "a.3mf", Size: 1}}, nil
	}
	got, err := ListPrinterFiles(context.Background(), Printer{Name: "A1", IP: "10.0.0.5", Serial: "03900X123"})
	// "/" fails once then works clear-text; "/cache" then starts clear-text.
	if err != nil || len(got) != 2 || fmt.Sprint(clear) != "[false true true]" {
		t.Fatalf("got %+v, %v, clear=%v", got, err, clear)
	}
	// Other models do not fall back.
	clear = nil
	if _, err := ListPrinterFiles(context.Background(), Printer{Name: "P1S", IP: "10.0.0.5", Serial: "01P00A000000009"}); err == nil || len(clear) != 1 {
		t.Fatalf("err = %v, clear=%v", err, clear)
	}
}

func TestBadFileName_OnlyADotDotSegment(t *testing.T) {
	for name, bad := range map[string]bool{
		"v1..2.3mf": false, "cache/v1..2.3mf": false, "..": true, "../x.3mf": true,
		"cache/../x.3mf": true, `cache\x.3mf`: true, "cache/x.3mf": false,
		// A line break or quote would start a second FTP or printer command.
		"a.3mf\r\nDELE /x.3mf": true, "a\n.3mf": true, `a".3mf`: true, "a\x7f.3mf": true,
	} {
		if badFileName(name) != bad {
			t.Errorf("badFileName(%q) = %v, want %v", name, !bad, bad)
		}
	}
	// A name with two dots gets past the name check (it stops at the preflight).
	err := PrintPrinterFile(context.Background(), Printer{Name: "no-session-printer", Serial: "01P00A000000009"}, "v1..2.3mf", 0)
	if err == nil || errors.Is(err, errFileName) {
		t.Fatalf("err = %v", err)
	}
}

func TestPrintPrinterFile_UsesTheGivenPlateAndFolderURL(t *testing.T) {
	const name = "ppf-plate"
	h, p, _ := setupPrint(t, name, "01P00A123", "idle")
	h.onPublish = func() { UpdatePrinterState(name, TelemetryData{Status: "printing"}) }
	if err := PrintPrinterFile(context.Background(), p, "cache/my part.3mf", 3); err != nil {
		t.Fatal(err)
	}
	var cmd struct {
		Print struct{ Param, URL, File string } `json:"print"`
	}
	if err := json.Unmarshal(h.published, &cmd); err != nil {
		t.Fatal(err)
	}
	if c := cmd.Print; c.Param != "Metadata/plate_3.gcode" || c.URL != "ftp:///cache/my%20part.3mf" || c.File != "cache/my part.3mf" {
		t.Fatalf("published %s", h.published)
	}
}

func TestPrinterFileURL(t *testing.T) {
	for name, want := range map[string]string{
		"a.3mf":             "ftp://a.3mf",
		"cache/foo.3mf":     "ftp:///cache/foo.3mf",
		"my part #1?.3mf":   "ftp://my%20part%20%231%3F.3mf",
		"cache/a b/c#d.3mf": "ftp:///cache/a%20b/c%23d.3mf",
	} {
		if got := printerFileURL(name); got != want {
			t.Errorf("printerFileURL(%q) = %q, want %q", name, got, want)
		}
	}
}

// With no plate given, the file is read off the printer for its sliced plate;
// an explicit plate wins; any failure falls back to plate 1.
func TestPrintPrinterFile_FindsTheSlicedPlate(t *testing.T) {
	const name = "ppf-auto"
	h, p, _ := setupPrint(t, name, "01P00A123", "idle")
	h.onPublish = func() { UpdatePrinterState(name, TelemetryData{Status: "printing"}) }
	plate2 := make3MF(t, map[string]string{"Metadata/plate_2.gcode": "G1"})
	old := ftpsDownload
	defer func() { ftpsDownload = old }()
	var remote string
	var tmpPath string
	downloads := 0
	var fail error
	ftpsDownload = func(_ context.Context, _ ftps.Config, r, local string, max int64) error {
		downloads++
		remote, tmpPath = r, local
		if fail != nil {
			return fail
		}
		b, err := os.ReadFile(plate2)
		if err != nil {
			return err
		}
		return os.WriteFile(local, b, 0o600)
	}
	param := func() string {
		var cmd struct {
			Print struct{ Param string } `json:"print"`
		}
		if err := json.Unmarshal(h.published, &cmd); err != nil {
			t.Fatal(err)
		}
		return cmd.Print.Param
	}
	if err := PrintPrinterFile(context.Background(), p, "cache/x.3mf", 0); err != nil {
		t.Fatal(err)
	}
	if got := param(); got != "Metadata/plate_2.gcode" || remote != "/cache/x.3mf" {
		t.Fatalf("param %q remote %q", got, remote)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
	// Explicit plate: no download.
	UpdatePrinterState(name, TelemetryData{Status: "idle"})
	downloads = 0
	if err := PrintPrinterFile(context.Background(), p, "cache/x.3mf", 5); err != nil || downloads != 0 || param() != "Metadata/plate_5.gcode" {
		t.Fatalf("err %v downloads %d param %q", err, downloads, param())
	}
	// Download fails: plate 1.
	UpdatePrinterState(name, TelemetryData{Status: "idle"})
	fail = errors.New("boom")
	if err := PrintPrinterFile(context.Background(), p, "cache/x.3mf", 0); err != nil || param() != "Metadata/plate_1.gcode" {
		t.Fatalf("err %v param %q", err, param())
	}
}
