package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Config struct {
	APIKey     string    `json:"api_key"`
	WebhookURL string    `json:"webhook_url"`
	Printers   []Printer `json:"printers"`
}

type Printer struct {
	Name         string `json:"name"`
	IP           string `json:"ip,omitempty"`
	Serial       string `json:"serial,omitempty"`
	LANCode      string `json:"lan_code,omitempty"`
	MoonrakerURL string `json:"moonraker_url,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
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

	// Best-effort migration so future updates keep using the stable user config path.
	_ = SaveConfig(&cfg)

	return &cfg, nil
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
	return os.WriteFile(p, data, 0644)
}
