package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	APIKey     string    `json:"api_key"`
	WebhookURL string    `json:"webhook_url"`
	Printers   []Printer `json:"printers"`
}

type Printer struct {
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Serial  string `json:"serial"`
	LANCode string `json:"lan_code,omitempty"`
	Brand   string `json:"brand,omitempty"`
}

// configPath returns the absolute path to config.json, always stored
// next to the executable regardless of where the user runs it from.
func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join("config", "config.json")
	}
	return filepath.Join(filepath.Dir(exe), "config", "config.json")
}

func LoadConfig() (*Config, error) {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
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
