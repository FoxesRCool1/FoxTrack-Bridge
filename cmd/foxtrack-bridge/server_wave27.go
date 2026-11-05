package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/plug"
	"foxtrack-bridge/virtualprinter"
	"foxtrack-bridge/webhook"
)

// Bridge 2.5.2: smart plugs and the virtual printer. Both read the config the
// same way the printer connections do and are re-synced from
// syncPrinterConnections, so every config path (Settings save, add, edit,
// delete) keeps them current.

var (
	plugs          = plug.Default
	virtualPrinter = &virtualprinter.Server{}
)

// bridgeCapabilities is what this build announces to FoxTrack's
// bridge-commands endpoint. FoxTrack hands plug_on / plug_off only to a Bridge
// that lists them, and the console commands (set_nozzle_temp, set_bed_temp,
// home) only to one that lists "console".
const bridgeCapabilities = "print_file,set_filament,alarms,plug_on,plug_off,console,files"

// printerLiveState is the latest telemetry for a printer, from whichever
// driver has it. ok is false while the printer is not reporting: "disconnected"
// (link down, a placeholder with no real data) and "connected" (link up, no
// report yet). The plug guards must read that as "unknown", never as "idle".
func printerLiveState(name string) (*mqttpkg.TelemetryData, bool) {
	var s *mqttpkg.TelemetryData
	if printerIsBambu(name) {
		s = mqttpkg.GetPrinterState(name)
	} else {
		s = lanCtrl.GetStates()[name]
	}
	return s, telemetryReporting(s)
}

func telemetryReporting(s *mqttpkg.TelemetryData) bool {
	return s != nil && s.Status != "disconnected" && s.Status != "connected"
}

// wirePlugs installs the plug package's lookups: printer status for the
// auto-off and the plug_off guard, and the plug state and print energy the
// mqtt and lan packages attach to their payloads.
func wirePlugs() {
	plugs.SetStatusSource(func(name string) (bool, float64, bool) {
		s, ok := printerLiveState(name)
		if !ok || s == nil {
			return false, 0, false
		}
		// Busy unless settled: a Bambu print that is starting (PREPARE) must
		// keep its power.
		busy := !mqttpkg.Settled(s.Status)
		// A nozzle reading of exactly 0 is a missing one (a room is never at
		// 0 C): unknown, so the plug stays on. Home Assistant printers often
		// report no nozzle at all; there status alone decides.
		if s.NozzleTemp == 0 && !busy && !printerIsHomeAssistant(name) {
			return false, 0, false
		}
		return busy, s.NozzleTemp, true
	})
	mqttpkg.SetPlugSource(plugs.Current, plugs.PrintEnergy)
}

// isPlugCommand reports whether a command is for the printer's plug rather
// than the printer.
func isPlugCommand(command string) bool {
	return command == "plug_on" || command == "plug_off"
}

// runPlugCommand switches a printer's plug. The off guard (printing, paused
// or hot) lives in the plug package.
func runPlugCommand(name, command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return plugs.Set(ctx, name, command == "plug_on")
}

// normalizePlug drops an empty plug block and fills the defaults, so a
// printer saved from the dashboard with the plug fields blank has no plug.
func normalizePlug(g *config.Plug) *config.Plug {
	if g == nil {
		return nil
	}
	g.Kind = strings.ToLower(strings.TrimSpace(g.Kind))
	g.Address = strings.TrimSpace(g.Address)
	if !g.Configured() {
		return nil
	}
	// Keep only the fields this kind uses, so a key typed under another type
	// is never stored. Passwords keep their spaces.
	keep := map[string]bool{}
	switch g.Kind {
	case config.PlugTuya:
		keep = map[string]bool{"device_id": true, "local_key": true, "version": true}
	case config.PlugKasa, config.PlugTapo:
		keep = map[string]bool{"username": true, "password": true}
	case config.PlugESPHome:
		keep = map[string]bool{"username": true, "password": true, "entity": true, "power_entity": true}
	case config.PlugHA:
		keep = map[string]bool{"token": true, "entity": true, "power_entity": true}
	case config.PlugMeross:
		keep = map[string]bool{"local_key": true, "mac": true}
	case config.PlugMyStrom:
		keep = map[string]bool{"token": true}
	}
	for name, f := range map[string]*string{"device_id": &g.DeviceID, "local_key": &g.LocalKey, "version": &g.Version, "mac": &g.MAC,
		"username": &g.Username, "password": &g.Password, "token": &g.Token, "entity": &g.Entity, "power_entity": &g.PowerEntity} {
		switch {
		case !keep[name]:
			*f = ""
		case name != "password":
			*f = strings.TrimSpace(*f)
		}
	}
	if g.AutoOffDelayMinutes < 0 {
		g.AutoOffDelayMinutes = 0
	}
	return g
}

// keepPlugSecrets refills a plug secret (Tuya local key, TP-Link or ESPHome
// password, Home Assistant token) left blank: the dashboard never gets a saved
// secret back, so blank means "keep it", for the same plug only. A Tuya plug is
// the same by device ID, any other by kind, address and username.
func keepPlugSecrets(g, prev *config.Plug) {
	if g == nil || prev == nil || g.Kind != prev.Kind {
		return
	}
	if g.Kind == config.PlugTuya {
		if g.DeviceID == "" || g.DeviceID != prev.DeviceID {
			return
		}
	} else if g.Address != prev.Address || g.Username != prev.Username {
		return
	}
	for _, f := range [][2]*string{{&g.LocalKey, &prev.LocalKey}, {&g.Password, &prev.Password}, {&g.Token, &prev.Token}} {
		if *f[0] == "" {
			*f[0] = *f[1]
		}
	}
}

// plugHasSecret reports whether a saved plug holds a secret the dashboard
// does not get back.
func plugHasSecret(g *config.Plug) bool {
	return g != nil && (g.LocalKey != "" || g.Password != "" || g.Token != "")
}

// handlePlugTest answers POST /api/plug/test {kind, address}: one read of
// the plug, so the dashboard can say whether the address works before the
// printer is saved.
func handlePlugTest(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var g config.Plug
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&g); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if normalizePlug(&g) == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "message": "Choose the plug type and enter its address."})
		return
	}
	if g.LocalKey == "" || g.Password == "" || g.Token == "" {
		// Checking a saved plug: use its saved secrets, only for the same
		// plug at its saved address.
		configMutex.RLock()
		for _, p := range configStore.Printers {
			if p.Plug != nil && p.Plug.Address == g.Address {
				keepPlugSecrets(&g, p.Plug)
			}
		}
		configMutex.RUnlock()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	reading, err := plug.Test(ctx, g)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "message": err.Error()})
		return
	}
	state := "off"
	if reading.On {
		state = "on"
	}
	msg := "Plug found, " + state + "."
	if reading.Watts != nil {
		msg = "Plug found, " + state + ", " + formatWatts(*reading.Watts) + " W now."
	} else {
		msg += " It has no energy meter, so FoxTrack will not see watts or kWh."
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": msg, "on": reading.On, "watts": reading.Watts, "has_meter": reading.Watts != nil})
}

// formatWatts renders a wattage for a user-facing message: one decimal place
// at most, so 100 -> "100", 12.34 -> "12.3", 0 -> "0".
func formatWatts(w float64) string {
	return strconv.FormatFloat(math.Round(w*10)/10, 'f', -1, 64)
}

// syncVirtualPrinter starts or stops the virtual printer to match cfg. The
// forwarder is nil while no Bridge token is saved, which makes every upload
// answer "connect the Bridge to FoxTrack first".
func syncVirtualPrinter(cfg *config.Config) {
	vp := cfg.VirtualPrinter
	if vp == nil {
		virtualPrinter.Apply(false, 0, "", nil)
		return
	}
	var forward virtualprinter.Forwarder
	if token := strings.TrimSpace(cfg.FoxTrack2APIKey); token != "" {
		forward = func(ctx context.Context, name string, body io.Reader, size int64) (int, []byte, error) {
			return webhook.SendUpload(ctx, token, webhook.UploadURLV2, name, body, size)
		}
	}
	virtualPrinter.Apply(vp.Enabled, vp.PortOrDefault(), vp.APIKey, forward)
}

// mergeVirtualPrinter keeps the stored virtual printer settings when a
// Settings save leaves them out, keeps the key when the dashboard sends none
// (it only ever sends enabled and port), and mints a key the first time the
// feature is turned on.
func mergeVirtualPrinter(newCfg, old *config.Config, present bool) {
	if !present {
		if old != nil {
			newCfg.VirtualPrinter = old.VirtualPrinter
		}
		return
	}
	if newCfg.VirtualPrinter == nil {
		return
	}
	if newCfg.VirtualPrinter.APIKey == "" && old != nil && old.VirtualPrinter != nil {
		newCfg.VirtualPrinter.APIKey = old.VirtualPrinter.APIKey
	}
	if newCfg.VirtualPrinter.Enabled && newCfg.VirtualPrinter.APIKey == "" {
		newCfg.VirtualPrinter.APIKey = config.NewVirtualPrinterKey()
	}
}

// handleVirtualPrinter answers GET /api/virtual-printer: the settings plus
// the addresses to type into the slicer and whether the port is open.
func handleVirtualPrinter(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	configMutex.RLock()
	vp := configStore.VirtualPrinter
	connected := strings.TrimSpace(configStore.FoxTrack2APIKey) != ""
	configMutex.RUnlock()
	out := map[string]interface{}{
		"enabled":   vp != nil && vp.Enabled,
		"port":      vp.PortOrDefault(),
		"api_key":   "",
		"urls":      lanURLs(vp.PortOrDefault()),
		"running":   virtualPrinter.Running(),
		"connected": connected,
	}
	if vp != nil {
		out["api_key"] = vp.APIKey
	}
	json.NewEncoder(w).Encode(out)
}

// errPlugNotSet is the plug_off / plug_on answer for a printer without a plug,
// worded for the FoxTrack user.
var errPlugNotSet = errors.New("This printer has no smart plug set in the Bridge. Add one under the printer's settings in the Bridge dashboard.")

// runPlugCommandForFoxTrack is runPlugCommand with the FoxTrack wording for a
// missing plug.
func runPlugCommandForFoxTrack(name, command string) error {
	err := runPlugCommand(name, command)
	if errors.Is(err, plug.ErrNoPlug) {
		return errPlugNotSet
	}
	return err
}

// printerIsHomeAssistant reports a saved printer read through Home Assistant.
func printerIsHomeAssistant(name string) bool {
	configMutex.RLock()
	defer configMutex.RUnlock()
	for _, p := range configStore.Printers {
		if p.Name == name {
			return p.Kind() == config.KindHomeAssistant
		}
	}
	return false
}
