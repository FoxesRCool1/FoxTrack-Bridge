package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

var (
	configStore *config.Config
	configMutex sync.RWMutex
)

// StartServer loads config, connects printers, and starts the HTTP server.
// It blocks until the server fails.
func StartServer() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Printf("No config found (%v) — starting fresh", err)
		cfg = &config.Config{Printers: []config.Printer{}}
	}

	configMutex.Lock()
	configStore = cfg
	configMutex.Unlock()

	for _, p := range cfg.Printers {
		mqttpkg.ConnectPrinter(mqttPrinter(p, cfg))
	}

	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/logo.png", handleLogo)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/printers", handlePrinters)
	http.HandleFunc("/api/status", handleStatus)

	fmt.Println("FoxTrack Bridge running at http://localhost:8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Printf("Server error: %v", err)
	}
}

func mqttPrinter(p config.Printer, cfg *config.Config) mqttpkg.Printer {
	return mqttpkg.Printer{
		Name:       p.Name,
		IP:         p.IP,
		Serial:     p.Serial,
		LANCode:    p.LANCode,
		WebhookURL: cfg.WebhookURL,
		APIKey:     cfg.APIKey,
	}
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "ui.html")
}

func handleLogo(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "logo.png")
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	cors(w)
	json.NewEncoder(w).Encode(mqttpkg.GetPrintersState())
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	cors(w)
	switch r.Method {
	case "GET":
		configMutex.RLock()
		defer configMutex.RUnlock()
		json.NewEncoder(w).Encode(configStore)

	case "POST":
		var newCfg config.Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		configMutex.Lock()
		configStore = &newCfg
		configMutex.Unlock()
		if err := config.SaveConfig(&newCfg); err != nil {
			log.Printf("Warning: failed to save config: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handlePrinters(w http.ResponseWriter, r *http.Request) {
	cors(w)
	switch r.Method {
	case "GET":
		configMutex.RLock()
		defer configMutex.RUnlock()
		json.NewEncoder(w).Encode(configStore.Printers)

	case "POST":
		var p config.Printer
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		configMutex.Lock()
		configStore.Printers = append(configStore.Printers, p)
		cfg := configStore
		configMutex.Unlock()

		if err := config.SaveConfig(cfg); err != nil {
			log.Printf("Warning: failed to save config: %v", err)
		}
		mqttpkg.ConnectPrinter(mqttPrinter(p, cfg))
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
