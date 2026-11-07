package lan

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// klipperUploadServer accepts the preflight and the upload, and reports the file
// as started.
func klipperUploadServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/server/info":
			w.WriteHeader(http.StatusOK)
		case "/server/files/upload":
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"item": {}, "print_started": true, "print_queued": false}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// recordDeletes swaps deleteMoonrakerFile for a recorder and returns the channel
// that receives each deleted remote name.
func recordDeletes(t *testing.T) chan string {
	t.Helper()
	deleted := make(chan string, 4)
	old := deleteMoonrakerFile
	deleteMoonrakerFile = func(_ configpkg.Printer, remote string) error {
		deleted <- remote
		return nil
	}
	t.Cleanup(func() { deleteMoonrakerFile = old })
	return deleted
}

// runPrint polls a printing status for file, then an idle status, so the print ends.
func runPrint(c *Controller, file string, start int64) {
	c.updateSession("voron", &mqttpkg.TelemetryData{Status: "printing", FileName: file}, nil, start)
	c.updateSession("voron", &mqttpkg.TelemetryData{Status: "idle"}, nil, start+100)
}

func expectDelete(t *testing.T, deleted chan string, want string) {
	t.Helper()
	select {
	case got := <-deleted:
		if got != want {
			t.Fatalf("deleted %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no delete of %q within 2s", want)
	}
}

func expectNoDelete(t *testing.T, deleted chan string) {
	t.Helper()
	select {
	case got := <-deleted:
		t.Fatalf("unexpected delete of %q", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestUploadedPrintEnd_DeletesFileOnce(t *testing.T) {
	srv := klipperUploadServer(t)
	defer srv.Close()
	deleted := recordDeletes(t)

	c := newUploadController(t, srv.URL, "idle")
	if err := c.UploadAndPrint(context.Background(), "voron", writeTemp(t, "G28\n"), "My Cube.gcode"); err != nil {
		t.Fatalf("UploadAndPrint: %v", err)
	}

	runPrint(c, "My_Cube.gcode", 1000)
	expectDelete(t, deleted, "My_Cube.gcode")

	// The name is forgotten after the delete: a later print of it deletes nothing.
	runPrint(c, "My_Cube.gcode", 2000)
	expectNoDelete(t, deleted)
}

func TestUploadedPrintEnd_OtherFileLeavesUploadAlone(t *testing.T) {
	srv := klipperUploadServer(t)
	defer srv.Close()
	deleted := recordDeletes(t)

	c := newUploadController(t, srv.URL, "idle")
	if err := c.UploadAndPrint(context.Background(), "voron", writeTemp(t, "G28\n"), "a.gcode"); err != nil {
		t.Fatalf("UploadAndPrint: %v", err)
	}

	runPrint(c, "other.gcode", 1000)
	expectNoDelete(t, deleted)

	// The uploaded file's own print still deletes it afterwards.
	runPrint(c, "a.gcode", 2000)
	expectDelete(t, deleted, "a.gcode")
}
