//go:build headless

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// filesPrinters adds the given printers to the environment's config and to the
// LAN controller (Klipper, PrusaLink, OctoPrint or Home Assistant only: the
// Bambu path goes over FTPS and MQTT, which the print_env does not fake).
// The controller polls each printer's fake server, which is how the tests
// set a printer's state. It returns the printers' external ids.
func filesPrinters(e *printEnv, printers ...config.Printer) []string {
	var ids []string
	for _, p := range printers {
		ids = append(ids, p.Name)
	}
	configMutex.Lock()
	configStore.Printers = append(configStore.Printers, printers...)
	configMutex.Unlock()
	lanCtrl.SyncPrinters(configStore.Printers, "", "test-key")
	for _, p := range printers {
		lanCtrl.RemovePrinter(p.Name)
		lanCtrl.AddOrUpdatePrinter(p, "", "test-key")
	}
	return ids
}

// printerFilesServer is the Moonraker or OctoPrint side of an HTTP printer:
// the file list, the start endpoint, and the telemetry the controller polls.
func printerFilesServer(t *testing.T, listBody, startBody string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	return printerFilesServerIn(t, "standby", listBody, startBody)
}

// printerFilesServerIn is printerFilesServer for a printer whose Moonraker
// print_stats state is state ("printing" for a busy one).
func printerFilesServerIn(t *testing.T, state, listBody, startBody string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	starts := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/printer/objects/query"):
			_, _ = w.Write([]byte(`{"result":{"status":{"print_stats":{"state":"` + state + `"}}}}`))
		case strings.HasPrefix(r.URL.Path, "/server/files/list"):
			_, _ = w.Write([]byte(listBody))
		case strings.HasPrefix(r.URL.Path, "/api/files/local"):
			_, _ = w.Write([]byte(listBody))
		case strings.HasPrefix(r.URL.Path, "/printer/print/start"), strings.HasPrefix(r.URL.Path, "/api/files/local/"):
			starts.Add(1)
			_, _ = w.Write([]byte(startBody))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, starts
}

// printerStarts counts the start requests the printer server got.
func printerStarts(starts *atomic.Int32) int { return int(starts.Load()) }

// filesPoll runs one files command the way the poll loop does, then waits for
// its ack to land.
func filesPoll(t *testing.T, e *printEnv, cmd bridgeCommand) {
	t.Helper()
	startFilesCommand("test-key", cmd)
	printJobs.wg.Wait()
}

// The done ack carries the listing in result.data, newest first.
func TestListFiles_KlipperAcksDoneWithTheListing(t *testing.T) {
	e := newPrintEnv(t)
	srv, _ := printerFilesServer(t, `{"result":[
		{"path":"old.gcode","modified":1700000000,"size":10},
		{"path":"new.gcode","modified":1760000000,"size":20}
	]}`, `{}`)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv.URL})[0]

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "list_files"})

	done := false
	for _, ev := range e.snapshot() {
		if strings.HasPrefix(ev, "ack:done:") {
			done = true
			var files []mqttpkg.PrinterFile
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "ack:done:")), &files); err != nil {
				t.Fatalf("data is not a file list: %q (%v)", ev, err)
			}
			if len(files) != 2 || files[0].Name != "new.gcode" || files[0].SizeBytes != 20 || files[1].Name != "old.gcode" {
				t.Fatalf("files = %+v", files)
			}
		}
	}
	if !done {
		t.Fatalf("no done ack with data: %v", e.snapshot())
	}
}

// An empty listing is an empty list in the ack, not a missing field.
func TestListFiles_EmptyListingAcksAnEmptyList(t *testing.T) {
	e := newPrintEnv(t)
	srv, _ := printerFilesServer(t, `{"result":[]}`, `{}`)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv.URL})[0]

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "list_files"})

	if e.count("ack:done:[]") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

// A printer the LAN controller does not know answers with its own message.
func TestListFiles_PrinterUnknownToTheLANControllerFails(t *testing.T) {
	e := newPrintEnv(t)
	configMutex.Lock()
	configStore.Printers = append(configStore.Printers, config.Printer{Name: "Ghost", MoonrakerURL: "http://127.0.0.1:1/"})
	configMutex.Unlock()

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: "Ghost", Command: "list_files"})

	if e.count("ack:failed:printer \"Ghost\" not found") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

// PrusaLink and Home Assistant printers have no file listing: the command
// fails with the message the LAN driver uses, and nothing is acked done.
func TestListFiles_UnsupportedPrinterTypeFails(t *testing.T) {
	for _, kind := range []string{"prusalink", "homeassistant"} {
		t.Run(kind, func(t *testing.T) {
			e := newPrintEnv(t)
			var p config.Printer
			switch kind {
			case "prusalink":
				p = config.Printer{Name: "MK4", PrusaLinkURL: "http://127.0.0.1:1/"}
			default:
				p = config.Printer{Name: "HA Printer", HomeAssistantURL: "http://127.0.0.1:1/"}
			}
			external := filesPrinters(e, p)[0]
			filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "list_files"})
			if e.count("ack:failed:This printer does not list its files through the Bridge.") != 1 || e.count("ack:done") != 0 {
				t.Fatalf("events = %v", e.snapshot())
			}
		})
	}
}

// A printer that is not in the config is left pending for another Bridge:
// no ack at all.
func TestListFiles_UnknownPrinterIsLeftAlone(t *testing.T) {
	e := newPrintEnv(t)
	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: "Someone Elses Printer", Command: "list_files"})
	if len(e.snapshot()) != 0 {
		t.Fatalf("expected no ack, got %v", e.snapshot())
	}
}

// print_printer_file starts the named file on the printer and acks done.
func TestPrintPrinterFile_KlipperStartsTheFileAndAcksDone(t *testing.T) {
	e := newPrintEnv(t)
	srv, starts := printerFilesServer(t, `{}`, `{}`)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv.URL})[0]

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "print_printer_file",
		Args: map[string]interface{}{"name": "sub/vase.gcode"}})

	if e.count("ack:done") != 1 || e.count("ack:failed") != 0 || printerStarts(starts) != 1 || len(e.snapshot()) != 1 {
		t.Fatalf("starts = %d, events = %v", printerStarts(starts), e.snapshot())
	}
}

// A file the printer does not have is refused with the driver's message.
func TestPrintPrinterFile_MissingFileFails(t *testing.T) {
	e := newPrintEnv(t)
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv404.Close)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv404.URL})[0]

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "print_printer_file",
		Args: map[string]interface{}{"name": "gone.gcode"}})

	if e.count("ack:failed:The printer has no file with that name. Check the name in the printer's file list.") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

// A printer that cannot print files at all fails with the driver's message.
func TestPrintPrinterFile_UnsupportedPrinterTypeFails(t *testing.T) {
	e := newPrintEnv(t)
	external := filesPrinters(e, config.Printer{Name: "MK4", PrusaLinkURL: "http://127.0.0.1:1/"})[0]

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "print_printer_file",
		Args: map[string]interface{}{"name": "a.gcode"}})

	if e.count("ack:failed:This printer does not list its files through the Bridge.") != 1 || e.count("ack:done") != 0 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

// The plate argument is for Bambu printers only: a Klipper printer prints the
// file as it is, so any plate number is accepted.
func TestPrintPrinterFile_PlateArgumentIsIgnoredOnKlipper(t *testing.T) {
	e := newPrintEnv(t)
	srv, starts := printerFilesServer(t, `{}`, `{}`)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv.URL})[0]

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "print_printer_file",
		Args: map[string]interface{}{"name": "vase.gcode", "plate": float64(3)}})

	if e.count("ack:done") != 1 || printerStarts(starts) != 1 {
		t.Fatalf("starts = %d, events = %v", printerStarts(starts), e.snapshot())
	}
}

// A plate number below 1 is refused before the printer is asked for anything.
func TestPrintPrinterFile_PlateBelowOneIsRefused(t *testing.T) {
	e := newPrintEnv(t)
	srv, starts := printerFilesServer(t, `{}`, `{}`)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv.URL})[0]

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "print_printer_file",
		Args: map[string]interface{}{"name": "vase.gcode", "plate": float64(0)}})

	if printerStarts(starts) != 0 {
		t.Fatalf("the printer was asked to start a file with plate 0")
	}
	if e.count("ack:failed:Pick a plate number from 1 up.") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

// A name that is not a file from the printer's list is refused before the
// printer is asked for anything.
func TestPrintPrinterFile_BadNameIsRefused(t *testing.T) {
	e := newPrintEnv(t)
	srv, starts := printerFilesServer(t, `{}`, `{}`)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv.URL})[0]

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "print_printer_file",
		Args: map[string]interface{}{"name": "../escape.gcode"}})

	if printerStarts(starts) != 0 {
		t.Fatalf("the printer was asked to start a path with ..")
	}
	if e.count("ack:failed:Pick a file from the printer's list.") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

// A printer that is printing is refused before the start is sent.
func TestPrintPrinterFile_BusyPrinterFails(t *testing.T) {
	e := newPrintEnv(t)
	srv, starts := printerFilesServerIn(t, "printing", `{}`, `{}`)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv.URL})[0]
	// The busy check reads the state the controller last polled.
	waitBridge(t, 10*time.Second, func() bool {
		st := lanCtrl.GetStates()[external]
		return st != nil && st.Status == "printing"
	})

	filesPoll(t, e, bridgeCommand{ID: "f1", ExternalID: external, Command: "print_printer_file",
		Args: map[string]interface{}{"name": "a.gcode"}})

	if printerStarts(starts) != 0 {
		t.Fatalf("a busy printer was asked to start a file")
	}
	if e.count("ack:failed:The printer is busy. Wait until it is idle, then try again.") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

// The same pending command fetched twice runs and acks once.
func TestFilesCommand_SameCommandTwiceRunsOnce(t *testing.T) {
	e := newPrintEnv(t)
	srv, _ := printerFilesServer(t, `{"result":[]}`, `{}`)
	external := filesPrinters(e, config.Printer{Name: "Voron Files", MoonrakerURL: srv.URL})[0]
	cmd := bridgeCommand{ID: "f1", ExternalID: external, Command: "list_files"}
	startFilesCommand("test-key", cmd)
	startFilesCommand("test-key", cmd)
	printJobs.wg.Wait()
	if e.count("ack:done") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}
