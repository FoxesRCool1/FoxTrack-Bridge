package mqtt

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// Bambu Cloud: ONE MQTT connection for every cloud printer on the account.
//
// Bambu bans an account for 24 hours to 7 days when it sees more than 50
// concurrent MQTT connections or connect/disconnect cycles of about a minute
// (https://forum.bambulab.com/t/bambu-lab-mqtt-limitations/83440). So this
// file never opens more than one connection, backs off exponentially with
// jitter, stops for 30 minutes after repeated failures, treats a rejected
// sign-in as final, and asks a printer for a full state dump at most once a
// minute. Reconnects have a single owner goroutine; nothing else may connect.
//
// Current firmware only honours the light command over the cloud; everything
// else is rejected as unverified (see SendCommandWithArgs).

// CloudSession is the account-level connection to Bambu Cloud.
type CloudSession struct {
	Broker   string // e.g. ssl://us.mqtt.bambulab.com:8883
	Username string // u_<uid>
	Token    string // the account access token, used as the password
}

// CloudStatus is what the dashboard shows for the account connection.
type CloudStatus struct {
	State       string `json:"state"` // one of the CloudState* constants
	Detail      string `json:"detail,omitempty"`
	HoldUntil   int64  `json:"hold_until,omitempty"` // unix: when the next attempt is due
	Failures    int    `json:"failures,omitempty"`
	Printers    int    `json:"printers"`
	ConnectedAt int64  `json:"connected_at,omitempty"`
}

const (
	CloudStateOff        = "off"
	CloudStateConnecting = "connecting"
	CloudStateConnected  = "connected"
	CloudStateBackoff    = "backoff"
	CloudStateHeld       = "held"
	CloudStateAuthFailed = "auth_failed"
)

const (
	cloudBackoffMin  = 5 * time.Second
	cloudBackoffMax  = 5 * time.Minute
	cloudHold        = 30 * time.Minute
	cloudMaxFailures = 10 // failures before a 30-minute hold
	cloudBanFailures = 3  // consecutive timeouts/refusals before assuming a rate limit or ban
	cloudStableAfter = 5 * time.Minute
	cloudPushallGap  = 60 * time.Second
	cloudSilence     = 120 * time.Second
	cloudNudgeGap    = 5 * time.Minute
	cloudKeepAlive   = 30 * time.Second
	cloudRetryGap    = 60 * time.Second
)

var (
	errCloudTimeout = errors.New("connect timed out")
	errCloudLost    = errors.New("connection lost")
)

// OnCloudAuthFailed is called (on its own goroutine) when the broker rejects
// the account token. The server uses it to mark the token expired on disk so
// no restart can retry a dead token.
var OnCloudAuthFailed func()

var (
	cloudMu  sync.Mutex
	cloudRun *cloudRunner

	// cloudIPs holds the LAN IP each cloud printer reports about itself, so the
	// camera (a LAN-only stream) can still be reached.
	cloudIPs   = make(map[string]string)
	cloudIPsMu sync.RWMutex

	// cloudClientID is fixed for the life of the process, so reconnects look
	// like one client to the broker instead of a stream of new ones.
	cloudClientID = newCloudClientID()
)

func newCloudClientID() string {
	host, _ := os.Hostname()
	host = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return -1
	}, host)
	if len(host) > 16 {
		host = host[:16]
	}
	if host == "" {
		host = "bridge"
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		binary.LittleEndian.PutUint32(b, uint32(time.Now().UnixNano()))
	}
	return fmt.Sprintf("foxtrack-%s-%s", host, hex.EncodeToString(b))
}

type cloudRunner struct {
	sess     CloudSession
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	retry    chan struct{}

	mu          sync.Mutex
	onIP        func(serial, ip string)
	printers    map[string]Printer // serial → printer
	client      mqtt.Client        // nil when not connected
	status      CloudStatus
	lastPushall map[string]time.Time
	lastMsg     map[string]time.Time
	lastNudge   map[string]time.Time
	lastRetry   time.Time
}

func newCloudRunner(sess CloudSession, printers []Printer, onIP func(serial, ip string)) *cloudRunner {
	r := &cloudRunner{
		sess:        sess,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		retry:       make(chan struct{}, 1),
		onIP:        onIP,
		printers:    make(map[string]Printer, len(printers)),
		lastPushall: make(map[string]time.Time),
		lastMsg:     make(map[string]time.Time),
		lastNudge:   make(map[string]time.Time),
	}
	for _, p := range printers {
		if p.Serial != "" {
			r.printers[p.Serial] = p
		}
	}
	r.status = CloudStatus{State: CloudStateConnecting, Printers: len(r.printers)}
	return r
}

// SetCloudPrinters reconciles the account connection with the config. A nil
// session or an empty printer list stops the connection. A changed session
// restarts it. A changed printer list only adjusts subscriptions on the live
// connection, so adding or renaming a printer never churns the connection.
func SetCloudPrinters(sess *CloudSession, printers []Printer, onIP func(serial, ip string)) {
	cloudMu.Lock()
	defer cloudMu.Unlock()

	for _, p := range printers {
		if p.Serial != "" {
			setSerial(p.Name, p.Serial)
		}
	}
	if sess == nil || len(printers) == 0 {
		if cloudRun != nil {
			stopCloudRunner(cloudRun)
			cloudRun = nil
			log.Printf("[bambu-cloud] connection stopped")
		}
		for _, p := range printers {
			markDisconnected(p.Name)
		}
		return
	}
	if cloudRun != nil && cloudRun.sess == *sess {
		cloudRun.setPrinters(printers, onIP)
		return
	}
	if cloudRun != nil {
		stopCloudRunner(cloudRun)
	}
	cloudRun = newCloudRunner(*sess, printers, onIP)
	log.Printf("[bambu-cloud] starting one connection for %d printer(s) as %s", len(cloudRun.printers), sess.Username)
	go cloudRun.loop()
}

func stopCloudRunner(r *cloudRunner) {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// GetCloudStatus reports the account connection state for the dashboard.
func GetCloudStatus() CloudStatus {
	cloudMu.Lock()
	r := cloudRun
	cloudMu.Unlock()
	if r == nil {
		return CloudStatus{State: CloudStateOff}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// IsCloudSerial reports whether serial belongs to a printer reached over the cloud.
func IsCloudSerial(serial string) bool {
	cloudMu.Lock()
	r := cloudRun
	cloudMu.Unlock()
	if r == nil || serial == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.printers[serial]
	return ok
}

// CloudIP returns the LAN IP a cloud printer last reported, or "".
func CloudIP(serial string) string {
	cloudIPsMu.RLock()
	defer cloudIPsMu.RUnlock()
	return cloudIPs[serial]
}

// ForgetCloudIP drops the learned IP for a removed printer.
func ForgetCloudIP(serial string) {
	cloudIPsMu.Lock()
	delete(cloudIPs, serial)
	cloudIPsMu.Unlock()
}

// RetryCloudNow ends a backoff or hold early. It is the one place a human can
// speed the loop up, and it still refuses more than one retry a minute.
func RetryCloudNow() error {
	cloudMu.Lock()
	r := cloudRun
	cloudMu.Unlock()
	if r == nil {
		return errors.New("the Bambu Cloud connection is off")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.State != CloudStateBackoff && r.status.State != CloudStateHeld {
		return nil
	}
	if !r.lastRetry.IsZero() && time.Since(r.lastRetry) < cloudRetryGap {
		return errors.New("please wait a minute between retries")
	}
	r.lastRetry = time.Now()
	select {
	case r.retry <- struct{}{}:
	default:
	}
	return nil
}

func markDisconnected(name string) {
	UpdatePrinterState(name, TelemetryData{Status: "disconnected", PrinterID: name, Timestamp: time.Now().Unix()})
}

func markConnected(name string) {
	UpdatePrinterState(name, TelemetryData{Status: "connected", PrinterID: name, Timestamp: time.Now().Unix()})
}

func cloudReportTopic(serial string) string  { return "device/" + serial + "/report" }
func cloudRequestTopic(serial string) string { return "device/" + serial + "/request" }

func cloudSerialFromTopic(topic string) string {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 || parts[0] != "device" || parts[2] != "report" {
		return ""
	}
	return parts[1]
}

// cloudCommandAllowed lists what current firmware accepts from a cloud client.
func cloudCommandAllowed(command string) bool {
	switch command {
	case "light", "light_on", "light_off", "toggle_light":
		return true
	}
	return false
}

func (r *cloudRunner) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *cloudRunner) setStatus(state, detail string, holdUntil int64, failures int) {
	r.mu.Lock()
	r.status.State = state
	r.status.Detail = detail
	r.status.HoldUntil = holdUntil
	r.status.Failures = failures
	r.status.Printers = len(r.printers)
	if state == CloudStateConnected {
		r.status.ConnectedAt = time.Now().Unix()
	} else {
		r.status.ConnectedAt = 0
	}
	r.mu.Unlock()
}

// setPrinters applies a new printer list to a running connection.
func (r *cloudRunner) setPrinters(printers []Printer, onIP func(serial, ip string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onIP = onIP
	next := make(map[string]Printer, len(printers))
	for _, p := range printers {
		if p.Serial != "" {
			next[p.Serial] = p
		}
	}
	client := r.client
	for serial, old := range r.printers {
		np, keep := next[serial]
		if keep && np.Name == old.Name {
			continue
		}
		if client != nil {
			if !keep {
				client.Unsubscribe(cloudReportTopic(serial))
			}
			clientMutex.Lock()
			if printerClients[old.Name] == client {
				delete(printerClients, old.Name)
			}
			clientMutex.Unlock()
		}
		if !keep {
			delete(r.lastMsg, serial)
			delete(r.lastPushall, serial)
			delete(r.lastNudge, serial)
		}
	}
	for serial, np := range next {
		old, had := r.printers[serial]
		if had && old.Name == np.Name {
			continue
		}
		if client != nil {
			if !had {
				r.subscribeLocked(client, serial, np.Name)
			}
			clientMutex.Lock()
			printerClients[np.Name] = client
			clientMutex.Unlock()
			markConnected(np.Name)
		}
	}
	r.printers = next
	r.status.Printers = len(next)
}

// subscribeLocked subscribes one serial and asks for a state dump. Caller holds r.mu.
func (r *cloudRunner) subscribeLocked(client mqtt.Client, serial, name string) {
	tok := client.Subscribe(cloudReportTopic(serial), 0, r.handleMessage)
	go func() {
		if tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
			log.Printf("[%s] cloud subscribe failed: %v", name, tok.Error())
			return
		}
		log.Printf("[%s] subscribed via Bambu Cloud", name)
	}()
	r.pushallLocked(client, serial, name)
}

// pushallLocked requests a full state dump at most once a minute per printer.
// Caller holds r.mu.
func (r *cloudRunner) pushallLocked(client mqtt.Client, serial, name string) {
	if last, ok := r.lastPushall[serial]; ok && time.Since(last) < cloudPushallGap {
		return
	}
	r.lastPushall[serial] = time.Now()
	client.Publish(cloudRequestTopic(serial), 0, false, `{"pushing": {"sequence_id": "0", "command": "pushall"}}`)
	log.Printf("[%s] sent pushall (cloud)", name)
}

// cloudRequestPushall is the light command's follow-up for cloud printers: the
// same rate limit applies, so a run of light toggles cannot flood the printer.
func cloudRequestPushall(serial string) {
	cloudMu.Lock()
	r := cloudRun
	cloudMu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.printers[serial]
	if !ok || r.client == nil {
		return
	}
	r.pushallLocked(r.client, serial, p.Name)
}

// loop is the single owner of the connection. It is the only goroutine that
// ever calls connect, so two reconnect loops can never run in parallel.
func (r *cloudRunner) loop() {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[bambu-cloud] connection goroutine panic: %v", rec)
		}
		r.dropClient()
		close(r.done)
	}()

	failures, connFailures := 0, 0
	for {
		r.setStatus(CloudStateConnecting, "", 0, failures)
		start := time.Now()
		err := r.connectAndServe()
		r.dropClient()
		if r.stopped() {
			r.setStatus(CloudStateOff, "", 0, 0)
			return
		}
		if err == nil {
			err = errCloudLost
		}
		if isCloudAuthErr(err) {
			detail := fmt.Sprintf("Bambu Cloud rejected the sign-in (%v). The token has probably expired: link the account again in Settings. The bridge will not retry on its own.", err)
			r.setStatus(CloudStateAuthFailed, detail, 0, failures)
			log.Printf("[bambu-cloud] %s", detail)
			if OnCloudAuthFailed != nil {
				go OnCloudAuthFailed()
			}
			return
		}
		if time.Since(start) >= cloudStableAfter {
			// A session that held for a while is not a failure streak.
			failures, connFailures = 0, 0
		}
		failures++
		if isCloudConnectivityErr(err) {
			connFailures++
		} else {
			connFailures = 0
		}

		var wait time.Duration
		var state, detail string
		switch {
		case connFailures >= cloudBanFailures:
			wait = cloudHold
			state = CloudStateHeld
			detail = fmt.Sprintf("Bambu Cloud did not accept the connection %d times in a row (%v). This can mean a temporary rate limit or ban, which lasts 24 hours to 7 days. Waiting %s before trying again.", connFailures, err, cloudHold)
			failures, connFailures = 0, 0
		case failures >= cloudMaxFailures:
			wait = cloudHold
			state = CloudStateHeld
			detail = fmt.Sprintf("%d failed connection attempts (%v). Waiting %s to stay within Bambu's connection limits.", failures, err, cloudHold)
			failures, connFailures = 0, 0
		default:
			wait = cloudBackoff(failures)
			state = CloudStateBackoff
			detail = fmt.Sprintf("Connection lost (%v). Retrying in %s.", err, wait.Round(time.Second))
		}
		r.setStatus(state, detail, time.Now().Add(wait).Unix(), failures)
		log.Printf("[bambu-cloud] %s", detail)

		select {
		case <-r.stop:
			r.setStatus(CloudStateOff, "", 0, 0)
			return
		case <-time.After(wait):
		case <-r.retry:
			log.Printf("[bambu-cloud] retrying now at the user's request")
		}
	}
}

// cloudBackoff returns the wait before attempt n+1: 5s, 10s, 20s … capped at
// 5 minutes, plus up to 25% jitter so many bridges never retry in lockstep.
func cloudBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := cloudBackoffMax
	if n-1 < 10 {
		d = cloudBackoffMin << uint(n-1)
		if d > cloudBackoffMax {
			d = cloudBackoffMax
		}
	}
	jitter := time.Duration(mrand.Int64N(int64(d/4) + 1))
	return d + jitter
}

func isCloudAuthErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, packets.ErrorRefusedNotAuthorised) || errors.Is(err, packets.ErrorRefusedBadUsernameOrPassword) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not authorized") || strings.Contains(s, "bad user name or password")
}

// isCloudConnectivityErr recognises the broker not answering at all, which is
// what a rate limit or ban looks like from outside.
func isCloudConnectivityErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errCloudTimeout) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection refused") || strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "no route to host") || strings.Contains(s, "connection reset")
}

// connectAndServe runs one session. It returns when the connection drops or
// when stop is closed; on stop it disconnects cleanly and returns nil.
func (r *cloudRunner) connectAndServe() error {
	done := make(chan struct{})
	var once sync.Once

	opts := mqtt.NewClientOptions()
	opts.AddBroker(r.sess.Broker)
	opts.SetUsername(r.sess.Username)
	opts.SetPassword(r.sess.Token)
	opts.SetClientID(cloudClientID)
	opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}) // public CA: verify, unlike the printer's self-signed LAN cert
	opts.SetConnectTimeout(15 * time.Second)
	opts.SetKeepAlive(cloudKeepAlive)
	opts.SetPingTimeout(10 * time.Second)
	opts.SetAutoReconnect(false) // the loop above is the only reconnect path
	opts.SetConnectRetry(false)
	opts.SetCleanSession(true)
	// Ordered delivery (paho's default) keeps each printer's delta merge
	// sequential, as on LAN. Handlers only take short locks and hand slow
	// work (webhooks, snapshots, history) to goroutines, so the reader is
	// never stalled.
	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		log.Printf("[bambu-cloud] connection lost: %v", err)
		once.Do(func() { close(done) })
	})

	client := mqtt.NewClient(opts)
	tok := client.Connect()
	if !tok.WaitTimeout(20 * time.Second) {
		client.Disconnect(0)
		return errCloudTimeout
	}
	if err := tok.Error(); err != nil {
		return err
	}
	if !client.IsConnected() {
		client.Disconnect(0)
		return errCloudTimeout
	}
	log.Printf("[bambu-cloud] MQTT connected to %s", r.sess.Broker)

	r.mu.Lock()
	r.client = client
	for serial, p := range r.printers {
		r.subscribeLocked(client, serial, p.Name)
		clientMutex.Lock()
		printerClients[p.Name] = client
		clientMutex.Unlock()
		markConnected(p.Name)
	}
	r.mu.Unlock()
	r.setStatus(CloudStateConnected, "", 0, 0)

	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-r.stop:
				return
			case <-t.C:
				r.nudgeSilent(client)
			}
		}
	}()

	select {
	case <-done:
		client.Disconnect(250)
		return errCloudLost
	case <-r.stop:
		client.Disconnect(250)
		return nil
	}
}

// nudgeSilent asks a printer that has gone quiet to resume pushing, at most
// once every five minutes per printer. Printers push on their own; this only
// recovers from a push stream that stopped.
func (r *cloudRunner) nudgeSilent(client mqtt.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != client {
		return
	}
	now := time.Now()
	baseline := time.Unix(r.status.ConnectedAt, 0)
	for serial, p := range r.printers {
		last, ok := r.lastMsg[serial]
		if !ok {
			last = baseline
		}
		if now.Sub(last) < cloudSilence {
			continue
		}
		if n, ok := r.lastNudge[serial]; ok && now.Sub(n) < cloudNudgeGap {
			continue
		}
		r.lastNudge[serial] = now
		client.Publish(cloudRequestTopic(serial), 0, false, `{"pushing": {"sequence_id": "0", "command": "start"}}`)
		log.Printf("[%s] no cloud report for %s — asked the printer to resume pushing", p.Name, now.Sub(last).Round(time.Second))
	}
}

// dropClient forgets the connection: commands fail fast and every cloud
// printer shows as disconnected until the next session is up.
func (r *cloudRunner) dropClient() {
	r.mu.Lock()
	client := r.client
	r.client = nil
	names := make([]string, 0, len(r.printers))
	for _, p := range r.printers {
		names = append(names, p.Name)
	}
	r.mu.Unlock()
	if client == nil {
		return
	}
	clientMutex.Lock()
	for _, name := range names {
		if printerClients[name] == client {
			delete(printerClients, name)
		}
	}
	clientMutex.Unlock()
	for _, name := range names {
		markDisconnected(name)
	}
}

// handleMessage routes a report to the printer that owns the topic and feeds
// it through the same parser as a LAN report.
func (r *cloudRunner) handleMessage(c mqtt.Client, msg mqtt.Message) {
	serial := cloudSerialFromTopic(msg.Topic())
	if serial == "" {
		return
	}
	r.mu.Lock()
	p, ok := r.printers[serial]
	if ok {
		r.lastMsg[serial] = time.Now()
	}
	onIP := r.onIP
	r.mu.Unlock()
	if !ok {
		return
	}
	if ip := cloudIPFromReport(msg.Payload()); ip != "" && learnCloudIP(serial, ip) {
		log.Printf("[%s] printer reports LAN IP %s (used for the camera)", p.Name, ip)
		if onIP != nil {
			go onIP(serial, ip)
		}
	}
	if p.IP == "" {
		p.IP = CloudIP(serial)
	}
	makeHandler(p)(c, msg)
}

func learnCloudIP(serial, ip string) bool {
	cloudIPsMu.Lock()
	defer cloudIPsMu.Unlock()
	if cloudIPs[serial] == ip {
		return false
	}
	cloudIPs[serial] = ip
	return true
}

// cloudIPFromReport extracts the printer's own LAN address from a report.
// Bambu sends it as a little-endian uint32 under print.net.info[].ip; only a
// private address is trusted.
func cloudIPFromReport(payload []byte) string {
	var rep struct {
		Print struct {
			Net struct {
				Info []struct {
					IP uint32 `json:"ip"`
				} `json:"info"`
			} `json:"net"`
		} `json:"print"`
	}
	if err := json.Unmarshal(payload, &rep); err != nil {
		return ""
	}
	for _, info := range rep.Print.Net.Info {
		if info.IP == 0 {
			continue
		}
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, info.IP)
		ip := net.IP(b)
		if ip.IsPrivate() {
			return ip.String()
		}
	}
	return ""
}
