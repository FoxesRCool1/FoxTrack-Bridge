package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"foxtrack-bridge/config"
	"foxtrack-bridge/mqttexport"
	"foxtrack-bridge/version"
)

// Settings backup and restore (Settings, General).
//
// The export is a complete copy of the printers and settings, including the
// printer access codes and plug, notification and MQTT passwords, because a
// restore without them would mean entering every code again. Three things are
// left out and are never changed by an import: the dashboard password and API
// tokens (restoring an old password could lock the user out), the linked Bambu
// account (its token is tied to this install), and the assistant's provider key.
//
// An import accepts the export file, or a plain config.json such as one of the
// daily copies in the backups folder.

const backupFormat = "foxtrack-bridge-backup"

type configBackup struct {
	Format     string          `json:"format"`
	Version    int             `json:"format_version"`
	App        string          `json:"app_version"`
	ExportedAt string          `json:"exported_at"`
	Config     json.RawMessage `json:"config"`
}

func init() {
	http.HandleFunc("/api/config/export", handleConfigExport)
	http.HandleFunc("/api/config/import", handleConfigImport)
}

// exportableConfig is cfg without the parts an import never restores.
func exportableConfig(cfg *config.Config) *config.Config {
	out := cfg.Clone()
	out.Security = nil
	out.BambuCloud = nil
	if out.AI != nil {
		ai := *out.AI
		ai.APIKey = ""
		out.AI = &ai
	}
	return out
}

func handleConfigExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	configMutex.RLock()
	var cfg *config.Config
	if configStore != nil {
		cfg = exportableConfig(configStore)
	} else {
		cfg = &config.Config{Printers: []config.Printer{}}
	}
	configMutex.RUnlock()
	inner, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	data, err := json.MarshalIndent(configBackup{
		Format:     backupFormat,
		Version:    1,
		App:        version.AppVersion,
		ExportedAt: now.UTC().Format(time.RFC3339),
		Config:     inner,
	}, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="foxtrack-bridge-backup-%s.json"`, now.Format("2006-01-02")))
	w.Write(data)
}

var errNotABackup = errors.New("This file is not a FoxTrack Bridge backup or config.json.")

// parseConfigBackup reads an export file or a plain config.json. It reports
// whether the file named a printer list at all, so an unrelated JSON file is
// refused instead of being read as "no printers".
func parseConfigBackup(body []byte) (*config.Config, error) {
	var head struct {
		Format string          `json:"format"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, errNotABackup
	}
	inner := body
	if head.Format != "" {
		if head.Format != backupFormat || len(head.Config) == 0 {
			return nil, errNotABackup
		}
		inner = head.Config
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(inner, &raw); err != nil {
		return nil, errNotABackup
	}
	if _, ok := raw["printers"]; !ok {
		return nil, errNotABackup
	}
	var cfg config.Config
	if err := json.Unmarshal(inner, &cfg); err != nil {
		return nil, errNotABackup
	}
	if cfg.Printers == nil {
		cfg.Printers = []config.Printer{}
	}
	return &cfg, nil
}

// resolveConfigImport builds the config an import would store. old is not
// mutated. Like POST /api/config it refuses to replace saved printers with an
// empty list unless confirmClear is set.
func resolveConfigImport(old *config.Config, body []byte, confirmClear bool) (*config.Config, error) {
	cfg, err := parseConfigBackup(body)
	if err != nil {
		return nil, err
	}
	if len(cfg.Printers) == 0 && old != nil && len(old.Printers) > 0 && !confirmClear {
		return nil, errRefuseClearPrinters
	}
	assignMissingIDs(cfg.Printers)
	if err := rejectNewlyIntroducedDuplicateNames(nil, cfg.Printers); err != nil {
		return nil, err
	}
	var oldPrinters []config.Printer
	if old != nil {
		oldPrinters = old.Printers
	}
	if err := rejectNewlyBlankNames(oldPrinters, cfg.Printers); err != nil {
		return nil, err
	}
	for i := range cfg.Printers {
		cfg.Printers[i].Plug = normalizePlug(cfg.Printers[i].Plug)
	}
	if old != nil {
		// An older config.json may predate a FoxTrack key; restoring it must
		// not unlink FoxTrack (same rule as POST /api/config).
		if cfg.APIKey == "" {
			cfg.APIKey = old.APIKey
		}
		if cfg.FoxTrack2APIKey == "" {
			cfg.FoxTrack2APIKey = old.FoxTrack2APIKey
		}
		cfg.Security = old.Security
		cfg.BambuCloud = old.BambuCloud
		cfg.AI = mergeImportedAI(cfg.AI, old.AI)
	} else {
		cfg.Security, cfg.BambuCloud = nil, nil
		if cfg.AI != nil {
			cfg.AI.APIKey = ""
		}
	}
	mergeVirtualPrinter(cfg, old, cfg.VirtualPrinter != nil)
	return cfg, nil
}

// mergeImportedAI keeps the stored provider key only when the imported
// settings point at the same provider, so the key never goes to a server it
// was not saved for. A file without an assistant block keeps the current one.
func mergeImportedAI(in, old *config.AI) *config.AI {
	if in == nil {
		return old
	}
	out := *in
	out.APIKey = ""
	trim := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
	if old != nil && old.Preset == in.Preset && trim(old.BaseURL) == trim(in.BaseURL) {
		out.APIKey = old.APIKey
	}
	return &out
}

// saveBeforeImport writes the current config next to config.json so an import
// can always be undone by hand. Returns the file name.
func saveBeforeImport(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", nil
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	name := "config.json.before-import-" + time.Now().Format("20060102-150405.000")
	dir := config.ConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := config.WriteFileAtomic(filepath.Join(dir, name), data, 0600); err != nil {
		return "", err
	}
	pruneBeforeImportFiles(dir, 5)
	return name, nil
}

// pruneBeforeImportFiles keeps the newest keep "before-import" copies in dir
// and removes the rest. Failures are logged, never returned.
func pruneBeforeImportFiles(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("ERROR: could not list %s to prune old imports: %v", dir, err)
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "config.json.before-import-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i := 0; i < len(names)-keep; i++ {
		if err := os.Remove(filepath.Join(dir, names[i])); err != nil {
			log.Printf("ERROR: could not remove old import copy %s: %v", names[i], err)
		}
	}
}

func handleConfigImport(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	confirmClear := r.URL.Query().Get("confirm_clear_printers") == "true"

	configMutex.Lock()
	oldCfg := configStore
	newCfg, err := resolveConfigImport(oldCfg, body, confirmClear)
	if err != nil {
		configMutex.Unlock()
		switch {
		case errors.Is(err, errRefuseClearPrinters), errors.Is(err, errDuplicatePrinterName):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	var prev *config.Config
	if oldCfg != nil {
		prev = oldCfg.Clone()
	}
	backupName, err := saveBeforeImport(prev)
	if err != nil {
		configMutex.Unlock()
		http.Error(w, "Could not save a copy of the current settings first, so nothing was changed ("+err.Error()+").", http.StatusInternalServerError)
		return
	}
	snapshot := newCfg.Clone()
	if saveErr := config.SaveConfig(snapshot); saveErr != nil {
		configMutex.Unlock()
		writeSaveRefused(w, saveErr)
		return
	}
	configStore = newCfg
	configMutex.Unlock()

	syncPrinterConnections(oldCfg, snapshot)
	applyImportedMQTTExport(snapshot)
	if oldCfg == nil || oldCfg.FoxTrack2APIKey != snapshot.FoxTrack2APIKey {
		wakeBridgeCommands()
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"printers": len(snapshot.Printers),
		"backup":   backupName,
	})
}

// applyImportedMQTTExport starts, restarts or stops the Home Assistant export
// for the imported settings, the same way its own Settings save does.
func applyImportedMQTTExport(cfg *config.Config) {
	var c config.MQTTExport
	if cfg.MQTTExport != nil {
		c = *cfg.MQTTExport
	}
	resolved, err := mqttexport.Resolve(c)
	if err != nil || !resolved.Enabled {
		mqttExporter.Apply(config.MQTTExport{})
		return
	}
	mqttExporter.Apply(ensureMQTTBridgeID(resolved))
}
