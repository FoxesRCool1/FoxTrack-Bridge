package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"foxtrack-bridge/config"
	"foxtrack-bridge/events"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/mqttexport"
	"foxtrack-bridge/version"
)

// Home Assistant and any other MQTT client: printer state goes to the broker
// the user names, and nowhere else. The settings have their own endpoint, so
// a Settings save on the dashboard never carries them.

var mqttExporter = mqttexport.New(mqttexport.Host{
	Printers: func() []config.Printer {
		configMutex.RLock()
		defer configMutex.RUnlock()
		return append([]config.Printer(nil), configStore.Printers...)
	},
	States:  func() map[string]*mqttpkg.TelemetryData { return liveStates() },
	Control: func(name, command string) error { return runControl(name, command, nil) },
	Version: version.AppVersion,
})

func init() {
	http.HandleFunc("/api/mqtt-export", handleMQTTExport)
	http.HandleFunc("/api/mqtt-export/test", handleMQTTExportTest)
	events.Subscribe(mqttExporter.OnEvent)
	onStartup(func() {
		c, err := mqttexport.Resolve(mqttExportSettings())
		if err != nil || !c.Enabled {
			return
		}
		c = ensureMQTTBridgeID(c)
		mqttExporter.Apply(c)
	})
}

// mqttExportSettings returns a copy of the stored block, or the zero value.
func mqttExportSettings() config.MQTTExport {
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore == nil || configStore.MQTTExport == nil {
		return config.MQTTExport{}
	}
	return *configStore.MQTTExport
}

// ensureMQTTBridgeID gives the block a stable ID the first time it is used and
// saves it. It is what keeps the Home Assistant devices the same after a new
// container or a new host name.
func ensureMQTTBridgeID(c config.MQTTExport) config.MQTTExport {
	if c.BridgeID != "" {
		return c
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	c.BridgeID = hex.EncodeToString(b)
	_ = mutateConfig(func(cfg *config.Config) error {
		if cfg.MQTTExport == nil {
			cfg.MQTTExport = &config.MQTTExport{}
		}
		if cfg.MQTTExport.BridgeID == "" {
			cfg.MQTTExport.BridgeID = c.BridgeID
		}
		c.BridgeID = cfg.MQTTExport.BridgeID
		return nil
	})
	return c
}

type mqttExportView struct {
	Enabled         bool   `json:"enabled"`
	Broker          string `json:"broker"`
	Username        string `json:"username"`
	PasswordSet     bool   `json:"password_set"`
	TopicPrefix     string `json:"topic_prefix"`
	Discovery       bool   `json:"discovery"`
	DiscoveryPrefix string `json:"discovery_prefix"`
	SkipCertCheck   bool   `json:"skip_cert_check"`
	Connected       bool   `json:"connected"`
	Error           string `json:"error"`
}

func mqttExportViewOf(c config.MQTTExport) mqttExportView {
	if c.TopicPrefix == "" {
		c.TopicPrefix = mqttexport.DefaultPrefix
	}
	if c.DiscoveryPrefix == "" {
		c.DiscoveryPrefix = mqttexport.DefaultDiscoveryPrefix
	}
	connected, errText := mqttExporter.Status()
	return mqttExportView{
		Enabled: c.Enabled, Broker: c.Broker, Username: c.Username, PasswordSet: c.Password != "",
		TopicPrefix: c.TopicPrefix, Discovery: c.Discovery, DiscoveryPrefix: c.DiscoveryPrefix,
		SkipCertCheck: c.SkipCertCheck, Connected: connected, Error: errText,
	}
}

// mqttExportRequest is the stored block plus the one flag that clears the
// password. Decoding onto the stored values makes an omitted field a "keep".
type mqttExportRequest struct {
	config.MQTTExport
	ClearPassword bool `json:"clear_password"`
}

// decodeMQTTExport reads a request body over the stored settings. A blank
// password keeps the stored one; clearing it needs clear_password.
func decodeMQTTExport(w http.ResponseWriter, r *http.Request) (mqttExportRequest, bool) {
	stored := mqttExportSettings()
	req := mqttExportRequest{MQTTExport: stored}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "That request was not valid JSON.")
		return req, false
	}
	req.BridgeID = stored.BridgeID
	switch {
	case req.ClearPassword:
		req.Password = ""
	case req.Password == "" || req.Password == stored.Password:
		// A kept password only goes to the broker and user it was saved
		// for: a new address or username without a new password drops it.
		if req.Broker == stored.Broker && req.Username == stored.Username {
			req.Password = stored.Password
		} else {
			req.Password = ""
		}
	}
	return req, true
}

// handleMQTTExport reads or writes the broker settings.
func handleMQTTExport(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	switch r.Method {
	case "OPTIONS":
		return
	case "GET":
		json.NewEncoder(w).Encode(mqttExportViewOf(mqttExportSettings()))
	case "POST":
		if !requireJSON(w, r) {
			return
		}
		req, ok := decodeMQTTExport(w, r)
		if !ok {
			return
		}
		c, err := mqttexport.Resolve(req.MQTTExport)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if c.Enabled {
			c = ensureMQTTBridgeID(c)
		}
		saveErr := mutateConfig(func(cfg *config.Config) error {
			cp := c
			cfg.MQTTExport = &cp
			return nil
		})
		mqttExporter.Apply(c)
		if saveErr != nil {
			writeSaveFailed(w, saveErr)
			return
		}
		json.NewEncoder(w).Encode(mqttExportViewOf(c))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleMQTTExportTest connects once with the settings in the body, which may
// not be saved yet, and says whether the broker accepted them.
func handleMQTTExportTest(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireJSON(w, r) {
		return
	}
	req, ok := decodeMQTTExport(w, r)
	if !ok {
		return
	}
	req.Enabled = true
	c, err := mqttexport.Resolve(req.MQTTExport)
	if err == nil {
		err = mqttexport.Test(c)
	}
	if err != nil {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "Connected. The broker accepted these settings."})
}
