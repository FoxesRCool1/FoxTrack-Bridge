//go:build headless

package main

import (
	"errors"
	"testing"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// A new serial with no new access code would save a Bambu LAN printer that
// can never connect: the edit is refused instead.
func TestApplyPrinterEdit_BambuSerialChangeNeedsAccessCode(t *testing.T) {
	old := editFixture()
	_, _, err := applyPrinterEdit(old, "a", printerEdit{Serial: strp("01P00C580809999"), LANCode: strp("")}, notBusy)
	if !errors.Is(err, errInvalidPrinterEdit) {
		t.Fatalf("err = %v, want errInvalidPrinterEdit", err)
	}
	if want := "Enter the access code again when you change the serial number."; err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	// Typing the code again, or leaving the serial alone, is fine.
	got, _, err := applyPrinterEdit(old, "a", printerEdit{Serial: strp("01P00C580809999"), LANCode: strp("newcode1")}, notBusy)
	if err != nil || got.Printers[0].LANCode != "newcode1" {
		t.Fatalf("new serial with a code: err = %v", err)
	}
	if _, _, err := applyPrinterEdit(old, "a", printerEdit{Serial: strp("01p00c580801716")}, notBusy); err != nil {
		t.Fatalf("same serial in another case: err = %v, want nil", err)
	}
}

// PrusaLink, OctoPrint and Home Assistant are unusable without their key or
// token; Klipper's key is optional.
func TestApplyPrinterEdit_AddressChangeNeedsKeyAgain(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{
		{ID: "p", Name: "Prusa", PrusaLinkURL: "http://10.0.0.20", APIKey: "pkey"},
		{ID: "o", Name: "Octo", OctoPrintURL: "http://10.0.0.21", APIKey: "okey"},
		{ID: "h", Name: "Home", HomeAssistantURL: "http://10.0.0.22:8123", HomeAssistantToken: "tok", HAStateEntity: "sensor.p"},
		{ID: "k", Name: "Voron", MoonrakerURL: "http://10.0.0.7:7125", APIKey: "mkey"},
	}}
	want := "Enter the key or token again when you change the address."
	for _, tc := range []struct {
		id   string
		edit printerEdit
	}{
		{"p", printerEdit{PrusaLinkURL: strp("http://10.0.0.30")}},
		{"o", printerEdit{OctoPrintURL: strp("http://10.0.0.31")}},
		{"h", printerEdit{HomeAssistantURL: strp("http://10.0.0.32:8123")}},
	} {
		_, _, err := applyPrinterEdit(old, tc.id, tc.edit, notBusy)
		if !errors.Is(err, errInvalidPrinterEdit) || err.Error() != want {
			t.Fatalf("%s: err = %v, want errInvalidPrinterEdit %q", tc.id, err, want)
		}
	}
	// A new key, or the same machine, saves.
	got, _, err := applyPrinterEdit(old, "p", printerEdit{PrusaLinkURL: strp("http://10.0.0.30"), APIKey: strp("new")}, notBusy)
	if err != nil || got.Printers[0].APIKey != "new" {
		t.Fatalf("new key: err = %v", err)
	}
	got, _, err = applyPrinterEdit(old, "p", printerEdit{PrusaLinkURL: strp("http://10.0.0.20/")}, notBusy)
	if err != nil || got.Printers[0].APIKey != "pkey" {
		t.Fatalf("same machine: err = %v", err)
	}
	// Klipper still saves with its key dropped.
	got, _, err = applyPrinterEdit(old, "k", printerEdit{MoonrakerURL: strp("http://10.0.0.8:7125")}, notBusy)
	if err != nil || got.Printers[3].APIKey != "" {
		t.Fatalf("klipper: err = %v, key = %q", err, got.Printers[3].APIKey)
	}
}

// A "start" with a file name takes the complete file-start path. A PrusaLink
// printer has none, so the answer is errCannotStartFile and not a bare driver
// start.
func TestRunControl_StartWithFileNameUsesFileStartPath(t *testing.T) {
	configMutex.Lock()
	oldCfg := configStore
	configStore = &config.Config{Printers: []config.Printer{
		{ID: "pl", Name: "ReviewPrusa", PrusaLinkURL: "http://192.0.2.9", APIKey: "k"},
	}}
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = oldCfg
		configMutex.Unlock()
	})
	err := runControl("ReviewPrusa", "start", map[string]interface{}{"file_name": "part.gcode"})
	if !errors.Is(err, errCannotStartFile) {
		t.Fatalf("err = %v, want errCannotStartFile", err)
	}
}

// A Bambu LAN printer whose code went blank is no longer a usable Bambu
// printer, but its MQTT state under the old name must still be closed.
func TestReconnectEditedPrinter_BambuNoLongerUsableDropsOldState(t *testing.T) {
	before := config.Printer{Name: "ReviewOld", IP: "192.0.2.1", Serial: "SER1", LANCode: "code1234"}
	after := config.Printer{Name: "ReviewNew", IP: "192.0.2.1", Serial: "SER2"}
	mqttpkg.UpdatePrinterState("ReviewOld", mqttpkg.TelemetryData{Status: "idle"})
	t.Cleanup(func() { mqttpkg.ResetPrinterStateForTest("ReviewOld") })
	reconnectEditedPrinter(before, after, &config.Config{})
	if _, ok := mqttpkg.GetPrintersState()["ReviewOld"]; ok {
		t.Fatal("state under the old name is still there")
	}
}
