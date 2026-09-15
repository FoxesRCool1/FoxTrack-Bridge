package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	APIKey          string    `json:"api_key"`
	FoxTrack2APIKey string    `json:"foxtrack2_api_key,omitempty"`
	Printers        []Printer `json:"printers"`
	AutoUpdate      bool      `json:"auto_update,omitempty"`
	// BambuCloud is the linked Bambu Lab account, if any. Optional and
	// additive: a config without it keeps loading unchanged.
	BambuCloud *BambuCloud `json:"bambu_cloud,omitempty"`
}

// BambuCloud holds one signed-in Bambu Lab account. AccessToken is a secret
// and must never leave the server (see redactConfig in server.go). Times are
// unix seconds. Bambu issues tokens for about 90 days and offers no refresh,
// so ExpiresAt is when the user has to sign in again.
type BambuCloud struct {
	Region       string `json:"region,omitempty"` // "global" (default) or "cn"
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	MQTTUsername string `json:"mqtt_username,omitempty"` // u_<uid>
	IssuedAt     int64  `json:"token_issued_at,omitempty"`
	ExpiresAt    int64  `json:"token_expires_at,omitempty"`
}

// Linked reports whether an account token is stored.
func (b *BambuCloud) Linked() bool {
	return b != nil && b.AccessToken != ""
}

// Expired reports whether the stored token is past its expiry.
func (b *BambuCloud) Expired(now int64) bool {
	return b != nil && b.ExpiresAt > 0 && now >= b.ExpiresAt
}

type Printer struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"name"`
	IP           string `json:"ip,omitempty"`
	Serial       string `json:"serial,omitempty"`
	LANCode      string `json:"lan_code,omitempty"`
	MoonrakerURL string `json:"moonraker_url,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	WebcamURL    string `json:"webcam_url,omitempty"`
	// PreviousNames accumulates every name this printer has been renamed from,
	// so history lookups can still find prints made under an old name. Always
	// empty until the edit-printer feature starts appending to it on rename.
	PreviousNames []string `json:"previous_names,omitempty"`
	// CameraHidden hides this printer's camera on the dashboard only. It does not
	// affect snapshot sends. Zero value is false, so existing configs stay visible.
	CameraHidden  bool     `json:"camera_hidden,omitempty"`
	// Connection selects how a Bambu printer is reached: "" or "lan" is the
	// printer's own MQTT broker (LAN mode), "cloud" is Bambu Cloud. The zero
	// value keeps every existing config on LAN. Klipper printers ignore it.
	Connection string `json:"connection,omitempty"`
}

// ConnectionCloud marks a Bambu printer reached through Bambu Cloud.
const ConnectionCloud = "cloud"

// IsCloud reports whether the printer is reached through Bambu Cloud.
func (p Printer) IsCloud() bool {
	return p.Connection == ConnectionCloud
}

// NewPrinterID returns a fresh, server-generated printer ID: 16 random bytes,
// hex-encoded. Assigned once on creation (or backfilled on first load for a
// pre-existing printer) and never changed afterward.
func NewPrinterID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read failing means the OS entropy source is broken —
		// extremely rare, but fall back to a timestamp-derived ID rather than
		// leaving the printer with no identity at all.
		return "fallback-" + hex.EncodeToString([]byte(time.Now().String()))[:32]
	}
	return hex.EncodeToString(b)
}

// legacyConfigPath is the old location used by previous builds.
func legacyConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join("config", "config.json")
	}
	return filepath.Join(filepath.Dir(exe), "config", "config.json")
}

// configPath returns the preferred persistent user config location.
func configPath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return legacyConfigPath()
	}
	return filepath.Join(base, "FoxTrack-Bridge", "config.json")
}

func LoadConfig() (*Config, error) {
	preferredPath := configPath()
	data, err := os.ReadFile(preferredPath)
	if errors.Is(err, os.ErrNotExist) {
		legacyPath := legacyConfigPath()
		legacyData, legacyErr := os.ReadFile(legacyPath)
		if legacyErr != nil {
			return nil, err
		}
		data = legacyData
		err = nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	backfilled := false
	for i := range cfg.Printers {
		if cfg.Printers[i].ID == "" {
			cfg.Printers[i].ID = NewPrinterID()
			backfilled = true
		}
	}

	// Best-effort migration so future updates keep using the stable user config path.
	if err := SaveConfig(&cfg); err != nil && backfilled {
		log.Printf("WARNING: failed to persist backfilled printer IDs (%v) — IDs will be regenerated on next restart until this config can be saved; printer identity will not be stable across restarts", err)
	}

	return &cfg, nil
}

// ConfigDir returns the directory where FoxTrack Bridge stores its data files.
func ConfigDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		exe, _ := os.Executable()
		return filepath.Join(filepath.Dir(exe), "config")
	}
	return filepath.Join(base, "FoxTrack-Bridge")
}

func SaveConfig(cfg *Config) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(p, data, 0600)
}

// WriteFileAtomic writes data to a temp file in the target directory and renames
// it over path, so a crash mid-write can never leave a truncated or partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// BackupCorrupt moves a config file that failed to load aside as
// config.json.corrupt-<timestamp> so a fresh config can be saved without
// destroying the original. It checks the preferred path first, then the
// legacy path, and returns the backup location.
func BackupCorrupt() (string, error) {
	p := configPath()
	if _, err := os.Stat(p); err != nil {
		legacy := legacyConfigPath()
		if _, lerr := os.Stat(legacy); lerr != nil {
			return "", err
		}
		p = legacy
	}
	backup := p + ".corrupt-" + time.Now().Format("20060102-150405")
	if err := os.Rename(p, backup); err != nil {
		return "", err
	}
	return backup, nil
}
