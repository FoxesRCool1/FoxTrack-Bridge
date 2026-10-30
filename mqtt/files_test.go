package mqtt

import (
	"context"
	"errors"
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
		return nil, errors.New("550 Failed to open directory")
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
		err := PrintPrinterFile(context.Background(), Printer{Name: "none", Serial: "01P00A000000009"}, name)
		if err == nil || published {
			t.Fatalf("%q: err=%v published=%v", name, err, published)
		}
	}
	// A good name on a printer with no MQTT session is refused by the preflight, not published.
	err := PrintPrinterFile(context.Background(), Printer{Name: "no-session-printer", Serial: "01P00A000000009"}, "cache/part.3mf")
	if err == nil || published {
		t.Fatalf("err=%v published=%v", err, published)
	}
}
