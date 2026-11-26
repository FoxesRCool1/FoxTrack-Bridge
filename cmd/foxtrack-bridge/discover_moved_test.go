package main

import (
	"encoding/json"
	"strings"
	"testing"

	"foxtrack-bridge/config"
)

// A saved printer found by serial at another IP is flagged as moved, with the
// saved printer's ID and name; the saved config is not touched.
func TestMarkAdded_MovedPrinter(t *testing.T) {
	found := []discoveredPrinter{
		{Type: "bambu", IP: "192.168.1.20", Serial: "SERMOVED"},
		{Type: "bambu", IP: "192.168.1.30", Serial: "SERSAME"},
		{Type: "bambu", IP: "192.168.1.40", Serial: "SERCLOUD"},
		{Type: "bambu", IP: "192.168.1.41", Serial: "SERCLOUDIP"},
		{Type: "bambu", IP: "192.168.1.50", Serial: "SERNEW"},
		{Type: "bambu", IP: "192.168.1.60", Serial: "SERTWICE"},
	}
	saved := []config.Printer{
		{ID: "p-moved", Name: "Workshop X1C", IP: "192.168.1.10", Serial: "sermoved"},
		{ID: "p-same", Name: "Office P1S", IP: " 192.168.1.30 ", Serial: "SERSAME"},
		{ID: "p-cloud", Name: "Cloud A1", Serial: "SERCLOUD", Connection: config.ConnectionCloud},
		{ID: "p-cloud-ip", Name: "Cloud P1P", IP: "192.168.1.9", Serial: "SERCLOUDIP", Connection: config.ConnectionCloud},
		// The same serial saved twice, once at the found address: not moved.
		{ID: "p-twice-old", Name: "Twice old", IP: "192.168.1.61", Serial: "SERTWICE"},
		{ID: "p-twice", Name: "Twice", IP: "192.168.1.60", Serial: "SERTWICE"},
	}
	before := append([]config.Printer(nil), saved...)
	markAdded(found, saved)

	type want struct {
		added, moved bool
		id, name     string
	}
	wants := []want{
		{true, true, "p-moved", "Workshop X1C"},
		{true, false, "", ""},
		{true, false, "", ""},
		{true, false, "", ""},
		{false, false, "", ""},
		{true, false, "", ""},
	}
	for i, w := range wants {
		p := found[i]
		if p.Added != w.added || p.Moved != w.moved || p.SavedID != w.id || p.SavedName != w.name {
			t.Errorf("%s at %s: added=%v moved=%v saved=%q/%q, want added=%v moved=%v saved=%q/%q",
				p.Serial, p.IP, p.Added, p.Moved, p.SavedID, p.SavedName, w.added, w.moved, w.id, w.name)
		}
	}
	for i := range saved {
		if saved[i].IP != before[i].IP || saved[i].ID != before[i].ID || saved[i].Name != before[i].Name {
			t.Errorf("saved printer %d changed: %+v", i, saved[i])
		}
	}

	// The JSON row: the new fields appear only on a moved printer.
	b, err := json.Marshal(found[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"added":true`, `"moved":true`, `"saved_id":"p-moved"`, `"saved_name":"Workshop X1C"`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("moved row %s lacks %s", b, s)
		}
	}
	b, _ = json.Marshal(found[1])
	for _, s := range []string{"moved", "saved_id", "saved_name"} {
		if strings.Contains(string(b), s) {
			t.Errorf("a printer at its saved address must not carry %q: %s", s, b)
		}
	}
	b, _ = json.Marshal(found[2])
	if strings.Contains(string(b), "moved") {
		t.Errorf("a Bambu Cloud printer must never be flagged as moved: %s", b)
	}
}
