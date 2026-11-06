package config

import "encoding/json"

// The blocks below are the Bridge's standalone features. Each one is optional
// and additive (absent means off, as before), and each is written only through
// its own /api endpoint: POST /api/config never touches them (see
// KeepFeatureBlocks), so a Settings save can never wipe one.

// Security guards the dashboard and API. With no PasswordHash the Bridge is
// open to the local network, as it always was.
type Security struct {
	// PasswordHash is "pbkdf2-sha256$<iterations>$<salt hex>$<hash hex>".
	PasswordHash string `json:"password_hash,omitempty"`
	// SessionKey signs login cookies. A new key signs everyone out.
	SessionKey string     `json:"session_key,omitempty"`
	Tokens     []APIToken `json:"tokens,omitempty"`
	// Shares are read-only printer pages (/share/{token}). Optional and
	// additive. Only the SHA-256 of each link's token is stored.
	Shares []ShareLink `json:"shares,omitempty"`
}

// ShareLink lets anyone with the link see one printer's progress,
// temperatures and, when Camera is set, a camera still. No controls.
type ShareLink struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PrinterID string `json:"printer_id"`
	Camera    bool   `json:"camera,omitempty"`
	Hash      string `json:"hash"`
	Hint      string `json:"hint,omitempty"` // last 4 characters, for the list
	CreatedAt int64  `json:"created_at"`
}

// APIToken lets a script, Home Assistant or Prometheus call the API while a
// password is set. Only the SHA-256 of the token is stored.
type APIToken struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	Hint      string `json:"hint,omitempty"` // last 4 characters, for the list
	CreatedAt int64  `json:"created_at"`
}

// Notifications sends print events to chat and push services.
type Notifications struct {
	Channels []NotifyChannel `json:"channels,omitempty"`
	// QuietStart and QuietEnd ("22:00", "07:00") hold back everything but
	// errors in between. Empty means no quiet hours.
	QuietStart string `json:"quiet_start,omitempty"`
	QuietEnd   string `json:"quiet_end,omitempty"`
	// AlmostDoneMinutes is how many minutes before the end a channel that
	// listens to print_almost_done gets told. 0 means the default of 10.
	AlmostDoneMinutes int `json:"almost_done_minutes,omitempty"`
}

// NotifyChannel is one destination. Which fields matter depends on Kind:
//
//	discord, slack: URL is the incoming webhook
//	telegram:       Token is the bot token, Target the chat ID
//	ntfy:           URL is the topic URL, Token an optional access token
//	pushover:       Token is the app token, Target the user key
//	gotify:         URL is the server, Token the app token
//	email:          SMTP, Target the recipients (comma-separated)
//	webhook:        URL receives a JSON POST
type NotifyChannel struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Kind     string   `json:"kind"`
	Enabled  bool     `json:"enabled,omitempty"`
	URL      string   `json:"url,omitempty"`
	Token    string   `json:"token,omitempty"` // secret
	Target   string   `json:"target,omitempty"`
	SMTP     *SMTP    `json:"smtp,omitempty"`
	Events   []string `json:"events,omitempty"`   // empty = the default set
	Printers []string `json:"printers,omitempty"` // printer IDs, empty = all
	Snapshot bool     `json:"snapshot,omitempty"` // attach a camera picture where the service allows it
}

// SMTP is an outgoing mail server.
type SMTP struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"` // secret
	From     string `json:"from,omitempty"`
	// Security is "starttls" (default), "tls" or "none".
	Security string `json:"security,omitempty"`
}

// Spoolman reports filament used per print to a Spoolman server.
type Spoolman struct {
	Enabled bool   `json:"enabled,omitempty"`
	URL     string `json:"url,omitempty"`
	// Slots maps "<printer ID>/<slot>" to a Spoolman spool ID.
	Slots map[string]int `json:"slots,omitempty"`
}

// Costs prices each print's filament and electricity.
type Costs struct {
	Currency      string             `json:"currency,omitempty"`        // shown as typed, e.g. "$" or "EUR"
	FilamentPerKg float64            `json:"filament_per_kg,omitempty"` // used when a material has no price
	MaterialPerKg map[string]float64 `json:"material_per_kg,omitempty"` // "PLA": 20
	EnergyPerKWh  float64            `json:"energy_per_kwh,omitempty"`
}

// Maintenance holds reminders that come due after some hours of printing.
type Maintenance struct {
	Tasks []MaintenanceTask `json:"tasks,omitempty"`
}

// MaintenanceTask is one reminder for one printer. Hours count the printer's
// prints in local history that ended after LastDoneAt.
type MaintenanceTask struct {
	ID            string  `json:"id"`
	PrinterID     string  `json:"printer_id"`
	Name          string  `json:"name"`
	IntervalHours float64 `json:"interval_hours"`
	LastDoneAt    int64   `json:"last_done_at"` // Unix seconds
	Note          string  `json:"note,omitempty"`
}

// MQTTExport publishes printer state to the user's own MQTT broker, with
// Home Assistant discovery so printers appear as devices on their own.
type MQTTExport struct {
	Enabled         bool   `json:"enabled,omitempty"`
	Broker          string `json:"broker,omitempty"` // "tcp://host:1883" or "ssl://host:8883"
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`     // secret
	TopicPrefix     string `json:"topic_prefix,omitempty"` // default "foxtrack_bridge"
	Discovery       bool   `json:"discovery,omitempty"`
	DiscoveryPrefix string `json:"discovery_prefix,omitempty"` // default "homeassistant"
	// SkipCertCheck accepts any certificate on an ssl:// broker (a self-signed one).
	SkipCertCheck bool `json:"skip_cert_check,omitempty"`
	// BridgeID names this Bridge in Home Assistant. Generated on first use and
	// kept, so the devices survive a new container or a new host name.
	BridgeID string `json:"bridge_id,omitempty"`
}

// Metrics serves Prometheus metrics at /metrics.
type Metrics struct {
	Enabled bool `json:"enabled,omitempty"`
}

// Timelapse records a picture every IntervalSec while a printer prints and
// turns them into a video (or an animated picture without ffmpeg) at the end.
type Timelapse struct {
	Enabled     bool     `json:"enabled,omitempty"`
	IntervalSec int      `json:"interval_sec,omitempty"` // default 30
	KeepDays    int      `json:"keep_days,omitempty"`    // default 30, 0 keeps them
	Printers    []string `json:"printers,omitempty"`     // printer IDs, empty = all with a camera
}

// KeepFeatureBlocks copies the standalone feature blocks from old into
// newCfg. POST /api/config never writes them: each has its own endpoint, and
// the dashboard's config view leaves them out, so a body that carries one is
// stale or hand-made.
func KeepFeatureBlocks(newCfg, old *Config) {
	if old == nil {
		newCfg.Security, newCfg.Notifications, newCfg.Spoolman, newCfg.Costs = nil, nil, nil, nil
		newCfg.Maintenance, newCfg.MQTTExport, newCfg.Metrics, newCfg.Timelapse = nil, nil, nil, nil
		return
	}
	newCfg.Security = old.Security
	newCfg.Notifications = old.Notifications
	newCfg.Spoolman = old.Spoolman
	newCfg.Costs = old.Costs
	newCfg.Maintenance = old.Maintenance
	newCfg.MQTTExport = old.MQTTExport
	newCfg.Metrics = old.Metrics
	newCfg.Timelapse = old.Timelapse
}

// cloneFeatureBlocks deep-copies the feature blocks into out. They hold
// slices and maps, so a JSON round trip is the copy least likely to miss one.
func cloneFeatureBlocks(out, c *Config) {
	out.Security = deepCopy(c.Security)
	out.Notifications = deepCopy(c.Notifications)
	out.Spoolman = deepCopy(c.Spoolman)
	out.Costs = deepCopy(c.Costs)
	out.Maintenance = deepCopy(c.Maintenance)
	out.MQTTExport = deepCopy(c.MQTTExport)
	out.Metrics = deepCopy(c.Metrics)
	out.Timelapse = deepCopy(c.Timelapse)
}

func deepCopy[T any](p *T) *T {
	if p == nil {
		return nil
	}
	data, err := json.Marshal(p)
	if err != nil {
		cp := *p
		return &cp
	}
	out := new(T)
	if err := json.Unmarshal(data, out); err != nil {
		cp := *p
		return &cp
	}
	return out
}
