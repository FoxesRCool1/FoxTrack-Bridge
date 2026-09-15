package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"foxtrack-bridge/cloud"
	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// Bambu Cloud account handlers (/api/cloud/…) and the glue that hands the
// account plus its printers to the MQTT package. Every outbound call goes
// through the cloud package, which paces it; nothing here loops or polls.

// cloudSessionFromConfig returns the MQTT session for the linked account, or
// nil when there is no usable token (unlinked or expired).
func cloudSessionFromConfig(cfg *config.Config) *mqttpkg.CloudSession {
	bc := cfg.BambuCloud
	if !bc.Linked() || bc.MQTTUsername == "" || bc.Expired(time.Now().Unix()) {
		return nil
	}
	return &mqttpkg.CloudSession{
		Broker:   cloud.Broker(bc.Region),
		Username: bc.MQTTUsername,
		Token:    bc.AccessToken,
	}
}

// syncCloud reconciles the account connection with cfg.
func syncCloud(cfg *config.Config) {
	sess := cloudSessionFromConfig(cfg)
	var printers []mqttpkg.Printer
	for _, p := range cfg.Printers {
		if p.IsCloud() {
			printers = append(printers, mqttPrinter(p, cfg))
		}
	}
	mqttpkg.SetCloudPrinters(sess, printers, onCloudIP)
}

// onCloudIP stores the LAN IP a cloud printer reported so the camera keeps
// working across restarts. Called off the MQTT goroutine; the mutex is never
// held across the save.
func onCloudIP(serial, ip string) {
	configMutex.Lock()
	changed := false
	for i := range configStore.Printers {
		p := &configStore.Printers[i]
		if p.IsCloud() && p.Serial == serial && p.IP != ip {
			p.IP = ip
			changed = true
		}
	}
	cfg := configStore
	configMutex.Unlock()
	if !changed {
		return
	}
	if err := config.SaveConfig(cfg); err != nil {
		log.Printf("Warning: failed to save config: %v", err)
	}
}

// cloudAccount returns a copy of the stored account block (nil when unlinked).
func cloudAccount() *config.BambuCloud {
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore == nil || configStore.BambuCloud == nil {
		return nil
	}
	bc := *configStore.BambuCloud
	return &bc
}

// storeCloudSession saves a fresh sign-in and (re)starts the connection.
func storeCloudSession(sess *cloud.Session) {
	configMutex.Lock()
	configStore.BambuCloud = &config.BambuCloud{
		Region:       sess.Region,
		Email:        sess.Email,
		AccessToken:  sess.Token,
		MQTTUsername: sess.Username,
		IssuedAt:     sess.IssuedAt,
		ExpiresAt:    sess.ExpiresAt,
	}
	cfg := configStore
	configMutex.Unlock()
	if err := config.SaveConfig(cfg); err != nil {
		log.Printf("Warning: failed to save config: %v", err)
	}
	log.Printf("[bambu-cloud] linked %s (%s); token valid until %s", sess.Email, sess.Region, time.Unix(sess.ExpiresAt, 0).Format("2006-01-02"))
	syncCloud(cfg)
}

// expireCloudToken marks the stored token dead after Bambu rejected it, so
// nothing retries with it and the dashboard asks for a new sign-in.
func expireCloudToken() {
	configMutex.Lock()
	if configStore.BambuCloud == nil {
		configMutex.Unlock()
		return
	}
	configStore.BambuCloud.ExpiresAt = time.Now().Unix()
	cfg := configStore
	configMutex.Unlock()
	if err := config.SaveConfig(cfg); err != nil {
		log.Printf("Warning: failed to save config: %v", err)
	}
	syncCloud(cfg)
}

type cloudStatusResponse struct {
	Linked         bool                `json:"linked"`
	Email          string              `json:"email,omitempty"`
	Region         string              `json:"region,omitempty"`
	TokenExpiresAt int64               `json:"token_expires_at,omitempty"`
	TokenExpired   bool                `json:"token_expired,omitempty"`
	SignInHoldTo   int64               `json:"signin_hold_until,omitempty"`
	CloudPrinters  int                 `json:"cloud_printers"`
	MQTT           mqttpkg.CloudStatus `json:"mqtt"`
}

type cloudDeviceResponse struct {
	cloud.Device
	Added   bool   `json:"added"`
	AddedAs string `json:"added_as,omitempty"`
}

func handleCloud(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/cloud/"), "/")
	switch action {
	case "status":
		handleCloudStatus(w, r)
	case "devices":
		handleCloudDevices(w, r)
	case "login":
		handleCloudLogin(w, r)
	case "verify":
		handleCloudVerify(w, r)
	case "token":
		handleCloudToken(w, r)
	case "unlink":
		handleCloudUnlink(w, r)
	case "retry":
		handleCloudRetry(w, r)
	default:
		http.Error(w, "unknown cloud action", http.StatusNotFound)
	}
}

func handleCloudStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := cloudStatusResponse{MQTT: mqttpkg.GetCloudStatus()}
	configMutex.RLock()
	if configStore != nil {
		if bc := configStore.BambuCloud; bc.Linked() {
			resp.Linked = true
			resp.Email = bc.Email
			resp.Region = bc.Region
			resp.TokenExpiresAt = bc.ExpiresAt
			resp.TokenExpired = bc.Expired(time.Now().Unix())
		}
		for _, p := range configStore.Printers {
			if p.IsCloud() {
				resp.CloudPrinters++
			}
		}
	}
	configMutex.RUnlock()
	if h := cloud.HoldUntil(); !h.IsZero() {
		resp.SignInHoldTo = h.Unix()
	}
	json.NewEncoder(w).Encode(resp)
}

// cloudErrorStatus maps a cloud package error to an HTTP status: pacing
// refusals are 429 so the dashboard can say "wait", auth is 401, the rest 502.
func cloudErrorStatus(err error) int {
	switch {
	case errors.Is(err, cloud.ErrRateLimited), errors.Is(err, cloud.ErrCloudflare):
		return http.StatusTooManyRequests
	case errors.Is(err, cloud.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, cloud.ErrWrongCode), errors.Is(err, cloud.ErrCodeExpired):
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

func writeCloudError(w http.ResponseWriter, err error) {
	w.WriteHeader(cloudErrorStatus(err))
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func handleCloudLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	sess, err := cloud.NewClient(req.Region).Login(ctx, req.Email, req.Password)
	if errors.Is(err, cloud.ErrCodeRequired) {
		json.NewEncoder(w).Encode(map[string]string{"status": "code_required"})
		return
	}
	if err != nil {
		log.Printf("[bambu-cloud] sign-in failed: %v", err)
		writeCloudError(w, err)
		return
	}
	storeCloudSession(sess)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleCloudVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Email  string `json:"email"`
		Code   string `json:"code"`
		Region string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	sess, err := cloud.NewClient(req.Region).LoginWithCode(ctx, req.Email, req.Code)
	if err != nil {
		log.Printf("[bambu-cloud] code sign-in failed: %v", err)
		writeCloudError(w, err)
		return
	}
	storeCloudSession(sess)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleCloudToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Email       string `json:"email"`
		AccessToken string `json:"access_token"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	sess, err := cloud.NewClient(req.Region).SessionFromToken(ctx, req.Email, req.AccessToken)
	if err != nil {
		log.Printf("[bambu-cloud] token link failed: %v", err)
		writeCloudError(w, err)
		return
	}
	storeCloudSession(sess)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleCloudUnlink(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	configMutex.Lock()
	configStore.BambuCloud = nil
	cfg := configStore
	configMutex.Unlock()
	if err := config.SaveConfig(cfg); err != nil {
		log.Printf("Warning: failed to save config: %v", err)
	}
	syncCloud(cfg)
	log.Printf("[bambu-cloud] account unlinked")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleCloudRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := mqttpkg.RetryCloudNow(); err != nil {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// cloudDevices lists the account's printers, from the cloud package's cache
// unless the caller asks for a refresh (still at most once a minute).
func cloudDevices(ctx context.Context, force bool) ([]cloud.Device, error) {
	bc := cloudAccount()
	if !bc.Linked() {
		return nil, errors.New("link your Bambu account in Settings first")
	}
	if bc.Expired(time.Now().Unix()) {
		return nil, errors.New("the Bambu sign-in has expired; link the account again in Settings")
	}
	devs, err := cloud.NewClient(bc.Region).Devices(ctx, bc.AccessToken, force)
	if errors.Is(err, cloud.ErrUnauthorized) {
		expireCloudToken()
	}
	return devs, err
}

func handleCloudDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	devs, err := cloudDevices(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeCloudError(w, err)
		return
	}
	addedAs := map[string]string{}
	configMutex.RLock()
	for _, p := range configStore.Printers {
		if p.Serial != "" {
			addedAs[p.Serial] = p.Name
		}
	}
	configMutex.RUnlock()
	out := make([]cloudDeviceResponse, 0, len(devs))
	for _, d := range devs {
		name, added := addedAs[d.Serial]
		out = append(out, cloudDeviceResponse{Device: d, Added: added, AddedAs: name})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"devices": out})
}

// resolveCloudPrinter fills a new cloud printer from the account's device
// list: the serial must belong to the account, and the LAN access code the
// cloud reports is what the camera needs. Returns a user-facing error.
func resolveCloudPrinter(ctx context.Context, p *config.Printer) error {
	p.Serial = strings.TrimSpace(p.Serial)
	if p.Serial == "" {
		return errors.New("pick a printer from your Bambu account")
	}
	devs, err := cloudDevices(ctx, false)
	if err != nil {
		return err
	}
	for _, d := range devs {
		if d.Serial != p.Serial {
			continue
		}
		p.Connection = config.ConnectionCloud
		p.LANCode = d.AccessCode
		p.MoonrakerURL = ""
		p.APIKey = ""
		p.WebcamURL = ""
		p.IP = strings.TrimSpace(p.IP) // optional: only used for the camera until the printer reports its own
		if strings.TrimSpace(p.Name) == "" {
			p.Name = d.Name
		}
		return nil
	}
	return errors.New("that printer is not on the linked Bambu account")
}
