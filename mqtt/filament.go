package mqtt

// Filament slots: the external spool (vt_tray) in the report, and
// set_filament, which tells the printer what is loaded in a slot (what Bambu
// Studio and Handy do when you edit a tray). Needs LAN Only Mode + Developer
// Mode, like pause and stop.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"foxtrack-bridge/webhook"
)

// ExternalSlot is the slot number of the external spool, the same number
// FoxTrack uses in print_file's ams_mapping and in the printer's tray_now.
const ExternalSlot = 254

// parseVtTray reads the external spool's keys from a report. It is parsed on
// its own, apart from BambuReport, so an odd vt_tray never costs the rest of
// the report. nil when the report has no vt_tray.
func parseVtTray(payload []byte) (map[string]any, bool) {
	if !bytes.Contains(payload, []byte(`"vt_tray"`)) && !bytes.Contains(payload, []byte(`"vir_slot"`)) {
		return nil, false
	}
	var probe struct {
		Print struct {
			VtTray  map[string]any   `json:"vt_tray"`
			VirSlot []map[string]any `json:"vir_slot"` // newer firmware; H2D has two (not shown yet)
		} `json:"print"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return nil, false
	}
	v := probe.Print.VtTray
	if v == nil && len(probe.Print.VirSlot) == 1 {
		v = probe.Print.VirSlot[0]
	}
	return v, v != nil
}

// mergeExternalSpool lays the reported vt_tray keys over the last known
// external spool, so a partial report changes only what it carries (FoxTrack
// replaces its stored status with every relay post). The loaded flag follows
// tray_now when a report has it.
func mergeExternalSpool(prev *AmsSlot, reported map[string]any, trayNow string) *AmsSlot {
	if prev == nil && reported == nil {
		return nil
	}
	s := &AmsSlot{Slot: ExternalSlot}
	if prev != nil {
		*s = *prev
	}
	if m, ok := reported["tray_type"].(string); ok {
		s.Material = m
	}
	if c, ok := reported["tray_color"].(string); ok && len(c) >= 6 {
		s.Color = c[:6]
	}
	if s.Material == "" { // an empty spool reports a black, see-through color
		s.Color = ""
	}
	if r, ok := reported["remain"].(float64); ok {
		s.Remaining = 0
		if r > 0 && r <= 100 {
			s.Remaining = int(r)
		}
	}
	if trayNow != "" {
		s.Active = trayNow == strconv.Itoa(ExternalSlot)
	}
	return s
}

func relayExternalSpool(s *AmsSlot) *webhook.RelayAmsSlot {
	if s == nil {
		return nil
	}
	return &webhook.RelayAmsSlot{Slot: s.Slot, Color: s.Color, Material: s.Material, Remaining: s.Remaining, Active: s.Active}
}

// filamentChanged reports a changed color or material in any slot, so an edit
// reaches FoxTrack at once instead of with the next heartbeat. Remaining is
// left out: it moves during a print and can wait.
func filamentChanged(prev, curr *TelemetryData) bool {
	if len(prev.AMS) != len(curr.AMS) || (prev.ExternalSpool == nil) != (curr.ExternalSpool == nil) {
		return true
	}
	for i := range prev.AMS {
		a, b := prev.AMS[i], curr.AMS[i]
		if a.Slot != b.Slot || a.Color != b.Color || a.Material != b.Material {
			return true
		}
	}
	if prev.ExternalSpool != nil {
		a, b := prev.ExternalSpool, curr.ExternalSpool
		return a.Color != b.Color || a.Material != b.Material
	}
	return false
}

// ---- set_filament ----

type filamentPreset struct {
	ID       string // tray_info_idx: Bambu's generic filament id
	Min, Max int    // nozzle range
}

// filamentPresets: the materials set_filament takes. Ids from ha-bambulab
// pybambu/filaments.json, ranges from Bambu Studio's generic profiles
// (fdm_filament_*.json), both read 2026-10-07. ponytail: the PLA-CF, PETG-CF
// and PVA ranges are estimates; the printer only uses them as a hint.
var filamentPresets = map[string]filamentPreset{
	"PLA":     {"GFL99", 190, 240},
	"PLA-CF":  {"GFL98", 190, 250},
	"PETG":    {"GFG99", 220, 270},
	"PETG-CF": {"GFG98", 230, 270},
	"ABS":     {"GFB99", 240, 280},
	"ASA":     {"GFB98", 240, 280},
	"PC":      {"GFC99", 260, 290},
	"PA":      {"GFN99", 260, 300},
	"PA-CF":   {"GFN98", 270, 300},
	"TPU":     {"GFU99", 200, 250},
	"PVA":     {"GFS99", 190, 240},
	"HIPS":    {"GFS98", 220, 270},
}

// FilamentMaterials lists the materials set_filament takes, sorted.
func FilamentMaterials() []string {
	out := make([]string, 0, len(filamentPresets))
	for m := range filamentPresets {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Test seams and timings.
var (
	requestPushall     = pushallByName
	filamentVerifyWait = 6 * time.Second
	filamentVerifyPoll = 200 * time.Millisecond
)

// setFilamentFromArgs reads FoxTrack's args: slot (a number from the relay
// ams array, or 254 for the external spool), material, color ("RRGGBB").
func setFilamentFromArgs(name, serial string, args map[string]interface{}) error {
	f, ok := args["slot"].(float64)
	if !ok || f != float64(int(f)) {
		return errors.New("FoxTrack sent no filament slot. Update FoxTrack Bridge, then try again.")
	}
	return SetFilament(name, serial, int(f), getStringArg(args, "material"), getStringArg(args, "color"))
}

// SetFilament tells the printer which filament is in slot: an AMS slot (the
// slot number Bridge reports) or ExternalSlot. It waits until a report shows
// the change, because a printer can answer "success" without storing it.
// The error text is plain English, shown in FoxTrack as is.
func SetFilament(name, serial string, slot int, material, color string) error {
	material = strings.ToUpper(strings.TrimSpace(material))
	preset, ok := filamentPresets[material]
	if !ok {
		return fmt.Errorf("Bridge does not know that material. Use one of: %s.", strings.Join(FilamentMaterials(), ", "))
	}
	color = strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(color), "#"))
	if b, err := hex.DecodeString(color); err != nil || len(b) != 3 {
		return errors.New("The color must be 6 hex digits, like FF8800.")
	}

	var amsID, trayID, slotID int
	switch {
	case slot == ExternalSlot && dualNozzle(serialPrefix(serial)):
		return errors.New("Setting the external spool is not supported on this printer model yet.")
	case slot == ExternalSlot:
		// Bambu Studio's ids for the one external spool (a P1S refuses tray_id 0).
		amsID, trayID, slotID = 255, 254, 0
	default:
		t, ok := findTray(getTrayRefs(name), slot)
		switch {
		case !ok:
			return fmt.Errorf("AMS slot %d is not on the printer right now. Refresh and try again.", slot+1)
		case t.Unit >= 128:
			return errors.New("Setting an AMS HT slot is not supported yet.")
		}
		amsID, trayID, slotID = t.Unit, t.Tray, t.Tray
	}

	payload := buildFilamentSetting(nextSequenceID(), amsID, trayID, slotID, material, color, preset)
	clearCommandRefusal(name, "ams_filament_setting")
	if err := publishRequestFn(name, serial, payload); err != nil {
		log.Printf("[%s] set_filament: publish failed: %v", name, err)
		return errPrintNoClient
	}
	log.Printf("[%s] set_filament: sent %s", name, payload)
	time.AfterFunc(400*time.Millisecond, func() { requestPushall(name) }) // the printer does not report the change by itself

	deadline := time.Now().Add(filamentVerifyWait)
	for {
		if reason, refused := commandRefusal(name, "ams_filament_setting"); refused {
			if reason == "" {
				reason = "no reason given"
			}
			return fmt.Errorf("The printer refused the change (%s). Check that Developer Mode is on, then try again.", truncateRunes(reason, 200))
		}
		st := GetPrinterState(name)
		if filamentShows(st, slot, material, color) {
			log.Printf("[%s] set_filament: slot %d is now %s #%s", name, slot, material, color)
			return nil
		}
		if st.Status == "disconnected" {
			return errors.New("The printer went offline. Try again when it is back.")
		}
		if time.Now().After(deadline) {
			return errors.New("The printer did not take the change. Check that Developer Mode is on. A Bambu Lab spool with a tag sets its own filament and cannot be changed.")
		}
		time.Sleep(filamentVerifyPoll)
	}
}

// buildFilamentSetting is the pure payload builder: Bambu Studio's field set
// (DeviceManager.cpp, command_ams_filament_settings) as bambuddy sends it
// (ams_set_filament_setting), without setting_id, which bambuddy found
// mislinks the profile. The color must be uppercase: P1S firmware reads a
// lowercase hex letter as 0.
func buildFilamentSetting(seq string, amsID, trayID, slotID int, material, color string, p filamentPreset) []byte {
	b, _ := json.Marshal(map[string]any{"print": map[string]any{
		"sequence_id": seq, "command": "ams_filament_setting",
		"ams_id": amsID, "tray_id": trayID, "slot_id": slotID,
		"tray_info_idx": p.ID, "tray_type": material, "tray_sub_brands": "",
		"tray_color": color + "FF", "nozzle_temp_min": p.Min, "nozzle_temp_max": p.Max,
	}})
	return b
}

func filamentShows(st *TelemetryData, slot int, material, color string) bool {
	s := st.ExternalSpool
	if slot != ExternalSlot {
		s = nil
		for i := range st.AMS {
			if st.AMS[i].Slot == slot {
				s = &st.AMS[i]
			}
		}
	}
	return s != nil && strings.EqualFold(s.Material, material) && strings.EqualFold(s.Color, color)
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func pushallByName(name string) {
	clientMutex.RLock()
	c, ok := printerClients[name]
	clientMutex.RUnlock()
	if serial := getSerial(name); ok && serial != "" && c.IsConnected() {
		sendPushall(c, name, "device/"+serial+"/request")
	}
}
