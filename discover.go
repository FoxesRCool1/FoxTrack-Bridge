package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
)

// Printer discovery on the local network, read-only: nothing here saves config,
// sends credentials or POSTs. Bambu printers announce themselves over a
// SSDP-like exchange on UDP 2021; they, Klipper (Moonraker) and PrusaLink
// printers are also found by probing the host's own /24 over TCP.

const (
	bambuSSDPPort    = 2021
	scanDialTimeout  = 400 * time.Millisecond
	scanHTTPTimeout  = 1500 * time.Millisecond
	scanMaxDials     = 128
	scanMaxHosts     = 1024
	bambuResendAfter = 1500 * time.Millisecond
)

// bambuMSearch asks every Bambu printer that hears it to answer with its headers.
var bambuMSearch = []byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1990\r\nMAN: \"ssdp:discover\"\r\nMX: 3\r\nST: urn:bambulab-com:device:3dprinter:1\r\n\r\n")

// bambuModels maps the DevModel.bambu.com code to the name people know.
var bambuModels = map[string]string{
	"BL-P001": "X1 Carbon",
	"BL-P002": "X1",
	"BL-P003": "X1E",
	"C11":     "P1P",
	"C12":     "P1S",
	"C13":     "X1E",
	"N1":      "A1 mini",
	"N2S":     "A1",
	"O1D":     "H2D",
}

func bambuModelName(code string) string {
	if name, ok := bambuModels[strings.ToUpper(code)]; ok {
		return name
	}
	return code
}

type discoveredPrinter struct {
	Type    string `json:"type"` // bambu, klipper or prusalink
	Name    string `json:"name"`
	Model   string `json:"model,omitempty"`
	IP      string `json:"ip"`
	Serial  string `json:"serial,omitempty"`
	Connect string `json:"connect,omitempty"` // Bambu only: lan or cloud
	URL     string `json:"url,omitempty"`     // Klipper and PrusaLink only
	Added   bool   `json:"added"`
}

type discoverResult struct {
	Printers []discoveredPrinter `json:"printers"`
	Notes    []string            `json:"notes"`
}

// discoverMu lets one scan run at a time: two at once would fight over UDP 2021
// and double the load on the network.
var discoverMu sync.Mutex

func handleDiscover(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	secs := 5
	if n, err := strconv.Atoi(r.URL.Query().Get("seconds")); err == nil {
		secs = min(max(n, 2), 10)
	}

	discoverMu.Lock()
	defer discoverMu.Unlock()
	res := runDiscovery(r.Context(), time.Duration(secs)*time.Second)
	markAdded(res.Printers, savedPrinters())
	json.NewEncoder(w).Encode(res)
}

// savedPrinters copies the saved printers under the config lock.
func savedPrinters() []config.Printer {
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore == nil {
		return nil
	}
	return append([]config.Printer(nil), configStore.Printers...)
}

// runDiscovery scans for d: the Bambu listener and the LAN scan run side by side.
func runDiscovery(ctx context.Context, d time.Duration) discoverResult {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	var bambu bambuScan
	var lanFound []discoveredPrinter
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); bambu = scanBambu(ctx) }()
	go func() { defer wg.Done(); lanFound = scanLAN(ctx) }()
	wg.Wait()

	found := bambu.printers
	for _, p := range lanFound {
		if p.Type == "bambu" {
			found = append(found, p)
		}
	}
	bambu.printers = rememberBambu(found, time.Now())
	res := discoverResult{
		Printers: dedupeSort(append(bambu.printers, lanFound...)),
		Notes:    []string{},
	}
	if bambu.noSocket {
		res.Notes = append(res.Notes, "Couldn't listen for Bambu printers on this computer (UDP port 2021).")
	}
	noBambu := !slices.ContainsFunc(res.Printers, func(p discoveredPrinter) bool { return p.Type == "bambu" })
	if !bambu.noSocket && bambu.bindFailed && noBambu {
		res.Notes = append(res.Notes, "Bambu Studio may be holding UDP port 2021 on this computer; if your Bambu printer is missing, close Bambu Studio and scan again.")
	}
	if inDocker() && noBambu {
		res.Notes = append(res.Notes, "Running in Docker: printer discovery needs host networking (network_mode: host).")
	}
	return res
}

// bambuSeen remembers Bambu printers from earlier scans, by serial. P1 printers
// never answer M-SEARCH; they announce about every 10 s and Wi-Fi drops some
// announcements, so a short scan can miss one the last scan saw.
// Guarded by discoverMu.
var bambuSeen = map[string]bambuSighting{}

type bambuSighting struct {
	printer discoveredPrinter
	at      time.Time
}

const bambuSeenFor = 10 * time.Minute

// rememberBambu records this scan's Bambu printers and returns every one seen
// in the last bambuSeenFor. A TCP find has no name or connect mode, so it keeps
// those an earlier announcement gave.
func rememberBambu(found []discoveredPrinter, now time.Time) []discoveredPrinter {
	for _, p := range found {
		key := strings.ToLower(p.Serial)
		if old, ok := bambuSeen[key]; ok && p.Name == "" {
			p.Name, p.Connect = old.printer.Name, old.printer.Connect
		}
		bambuSeen[key] = bambuSighting{p, now}
	}
	var out []discoveredPrinter
	for serial, s := range bambuSeen {
		if now.Sub(s.at) > bambuSeenFor {
			delete(bambuSeen, serial)
			continue
		}
		out = append(out, s.printer)
	}
	return out
}

// dedupeSort drops repeats by IP (and by serial for Bambu), keeping the first
// seen, then orders by type and IP.
func dedupeSort(in []discoveredPrinter) []discoveredPrinter {
	out := []discoveredPrinter{}
	seen := map[string]bool{}
	for _, p := range in {
		if seen["ip:"+p.IP] || (p.Serial != "" && seen["sn:"+strings.ToLower(p.Serial)]) {
			continue
		}
		seen["ip:"+p.IP] = true
		if p.Serial != "" {
			seen["sn:"+strings.ToLower(p.Serial)] = true
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		a, _ := netip.ParseAddr(out[i].IP)
		b, _ := netip.ParseAddr(out[j].IP)
		return a.Less(b)
	})
	return out
}

// markAdded flags the printers a saved printer already covers, matched by IP,
// by serial, or by the host of its Moonraker or PrusaLink URL.
func markAdded(found []discoveredPrinter, saved []config.Printer) {
	hosts, serials := map[string]bool{}, map[string]bool{}
	for _, p := range saved {
		for _, h := range []string{p.IP, urlHost(p.MoonrakerURL), urlHost(p.PrusaLinkURL)} {
			if h != "" {
				hosts[h] = true
			}
		}
		if p.Serial != "" {
			serials[strings.ToLower(p.Serial)] = true
		}
	}
	for i := range found {
		found[i].Added = hosts[found[i].IP] || (found[i].Serial != "" && serials[strings.ToLower(found[i].Serial)])
	}
}

// urlHost returns the host of a saved URL; a bare "ip:port" counts too.
func urlHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// --- Bambu: SSDP-like discovery on UDP 2021 ---

type bambuScan struct {
	printers   []discoveredPrinter
	bindFailed bool // UDP 2021 was taken (Bambu Studio?), so only replies to our M-SEARCH can arrive
	noSocket   bool // no UDP socket at all
}

// parseBambuSSDP reads one NOTIFY or "HTTP/1.1 200 OK" datagram. src is the
// sender, used when Location is missing. Datagrams that are not from a Bambu
// printer, or carry no USN (our own M-SEARCH echo has none), are refused.
func parseBambuSSDP(data []byte, src net.IP) (discoveredPrinter, bool) {
	h := map[string]string{}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.HasPrefix(strings.ToUpper(lines[0]), "M-SEARCH") {
		return discoveredPrinter{}, false
	}
	for _, line := range lines[1:] {
		if k, v, ok := strings.Cut(line, ":"); ok {
			h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if !strings.Contains(strings.ToLower(h["nt"]+h["st"]), "bambulab") || h["usn"] == "" {
		return discoveredPrinter{}, false
	}
	ip := h["location"]
	if net.ParseIP(ip) == nil {
		if src == nil {
			return discoveredPrinter{}, false
		}
		ip = src.String()
	}
	p := discoveredPrinter{
		Type:    "bambu",
		Name:    h["devname.bambu.com"],
		Model:   bambuModelName(h["devmodel.bambu.com"]),
		IP:      ip,
		Serial:  h["usn"],
		Connect: strings.ToLower(h["devconnect.bambu.com"]),
	}
	if p.Name == "" {
		p.Name = ip
	}
	return p, true
}

// scanBambu listens on UDP 2021 until ctx ends, asking printers to answer.
func scanBambu(ctx context.Context) bambuScan {
	var res bambuScan
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: bambuSSDPPort})
	if err != nil {
		res.bindFailed = true
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{})
		if err != nil {
			res.noSocket = true
			return res
		}
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	targets := bambuTargets()
	send := func() {
		for _, t := range targets {
			conn.WriteToUDP(bambuMSearch, t)
		}
	}
	send()
	go func() {
		t := time.NewTicker(bambuResendAfter)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				send()
			}
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			// A stray read error (Windows reports ICMP port-unreachable here)
			// must not end the scan early; only the deadline or a close does.
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return res
			}
			continue
		}
		if p, ok := parseBambuSSDP(buf[:n], from.IP); ok {
			res.printers = append(res.printers, p)
		}
	}
}

// bambuTargets is the limited broadcast address plus each interface's directed
// broadcast: on a multi-homed computer the limited one leaves by one interface only.
func bambuTargets() []*net.UDPAddr {
	targets := []*net.UDPAddr{{IP: net.IPv4bcast, Port: bambuSSDPPort}}
	for _, n := range localIPv4Nets() {
		ones, bits := n.Mask.Size()
		if bits != 32 || ones >= 31 || !isPrivateIP(n.IP) {
			continue
		}
		b := make(net.IP, 4)
		for i := range b {
			b[i] = n.IP[i] | ^n.Mask[i]
		}
		targets = append(targets, &net.UDPAddr{IP: b, Port: bambuSSDPPort})
	}
	return targets
}

// --- Klipper and PrusaLink: TCP scan of the host's /24 ---

// scanLAN probes every host in scanTargets for Moonraker and PrusaLink.
func scanLAN(ctx context.Context) []discoveredPrinter {
	hosts := scanTargets(localIPv4Nets(), scanMaxHosts)
	client := &http.Client{
		Timeout:       scanHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	sem := make(chan struct{}, scanMaxDials)
	open := func(ip string, port int) bool {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return false
		}
		d := net.Dialer{Timeout: scanDialTimeout}
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		c.Close()
		return true
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var found []discoveredPrinter
	for _, ip := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, ok := discoverHost(ctx, client, ip, func(port int) bool { return open(ip, port) }); ok {
				mu.Lock()
				found = append(found, p)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return found
}

// scanTargets lists the hosts to probe: the /24 around each private interface
// address, minus this computer's own addresses, at most limit in total.
// ponytail: only the host's /24 is scanned even on a wider mask; widen if users ask.
func scanTargets(nets []*net.IPNet, limit int) []string {
	own := map[string]bool{}
	for _, n := range nets {
		own[n.IP.String()] = true
	}
	// Scan home and office ranges first so the cap and the deadline cut the rest.
	nets = append([]*net.IPNet(nil), nets...)
	sort.SliceStable(nets, func(i, j int) bool { return addrRank(nets[i].IP) < addrRank(nets[j].IP) })
	var out []string
	seen := map[string]bool{}
	for _, n := range nets {
		ip := n.IP.To4()
		if ip == nil || !isPrivateIP(ip) {
			continue
		}
		for i := 1; i <= 254; i++ {
			h := net.IPv4(ip[0], ip[1], ip[2], byte(i)).String()
			if own[h] || seen[h] {
				continue
			}
			if len(out) >= limit {
				return out
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// discoverHost checks one host's open ports for a Bambu, Moonraker or PrusaLink printer.
func discoverHost(ctx context.Context, c *http.Client, ip string, open func(port int) bool) (discoveredPrinter, bool) {
	if open(8883) {
		if serial, ok := discoverBambuTLS(ctx, ip); ok {
			return discoveredPrinter{Type: "bambu", Model: bambuSerialModel(serial), IP: ip, Serial: serial}, true
		}
	}
	if open(7125) {
		base := "http://" + ip + ":7125"
		if host, ok := discoverKlipper(ctx, c, base); ok {
			return klipperPrinter(ip, base, host), true
		}
	}
	if open(80) {
		base := "http://" + ip
		if host, ok := discoverKlipper(ctx, c, base); ok {
			return klipperPrinter(ip, base, host), true
		}
		if name, ok := discoverPrusaLink(ctx, c, base); ok {
			return discoveredPrinter{Type: "prusalink", Name: name, IP: ip, URL: base}, true
		}
	}
	return discoveredPrinter{}, false
}

// discoverBambuTLS finds a Bambu printer whose announcements Wi-Fi keeps
// dropping: its MQTT certificate carries the serial. Only the TLS handshake
// runs; nothing is sent after it.
func discoverBambuTLS(ctx context.Context, ip string) (string, bool) {
	// No timeout of its own: a printer's handshake can take over 1.5 s on Wi-Fi,
	// so only the scan's deadline ends it.
	d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} // signed by BBL CA; read, not trusted
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "8883"))
	if err != nil {
		return "", false
	}
	defer c.Close()
	certs := c.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", false
	}
	return bambuCertSerial(certs[0])
}

// bambuCertSerial returns the serial a Bambu printer's certificate carries as its CN.
func bambuCertSerial(cert *x509.Certificate) (string, bool) {
	bbl := slices.ContainsFunc(cert.Issuer.Organization, func(o string) bool { return strings.HasPrefix(o, "BBL") })
	if !bbl || len(cert.Subject.CommonName) < 8 {
		return "", false
	}
	return cert.Subject.CommonName, true
}

// bambuSerialModel names the model from the serial's first three characters.
func bambuSerialModel(serial string) string {
	return map[string]string{"00M": "X1 Carbon", "01S": "P1P", "01P": "P1S", "030": "A1 mini", "039": "A1"}[serial[:3]]
}

func klipperPrinter(ip, base, hostname string) discoveredPrinter {
	if hostname == "" {
		hostname = ip
	}
	return discoveredPrinter{Type: "klipper", Name: hostname, IP: ip, URL: base}
}

// discoverGet does one credential-free GET and returns the status, headers and the first 64 KB.
func discoverGet(ctx context.Context, c *http.Client, u string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header, body, nil
}

// discoverKlipper reports whether base serves Moonraker's /printer/info, and the
// hostname it gives.
func discoverKlipper(ctx context.Context, c *http.Client, base string) (string, bool) {
	_, _, body, err := discoverGet(ctx, c, base+"/printer/info")
	if err != nil {
		return "", false
	}
	var v struct {
		Result *struct {
			Hostname string `json:"hostname"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &v) != nil || v.Result == nil {
		return "", false
	}
	return v.Result.Hostname, true
}

// discoverPrusaLink reports whether base is a PrusaLink printer: /api/version
// answers 200, or 401 when it wants a key, and says Prusa somewhere.
func discoverPrusaLink(ctx context.Context, c *http.Client, base string) (string, bool) {
	status, hdr, body, err := discoverGet(ctx, c, base+"/api/version")
	if err != nil || (status != http.StatusOK && status != http.StatusUnauthorized) {
		return "", false
	}
	said := strings.ToLower(string(body) + hdr.Get("WWW-Authenticate") + hdr.Get("Server"))
	if !strings.Contains(said, "prusa") {
		return "", false
	}
	var v struct {
		Hostname string `json:"hostname"`
	}
	json.Unmarshal(body, &v)
	if v.Hostname == "" {
		return "PrusaLink printer", true
	}
	return v.Hostname, true
}

// --- This computer's network addresses ---

// cgnat is 100.64.0.0/10, the range Tailscale hands out.
var cgnat = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// addrRank orders addresses by how likely they are the LAN a phone or printer is
// on: 192.168, then 10, then the rest (172.16/12 is mostly container bridges).
func addrRank(ip net.IP) int {
	switch ip4 := ip.To4(); {
	case ip4 != nil && ip4[0] == 192:
		return 0
	case ip4 != nil && ip4[0] == 10:
		return 1
	}
	return 2
}

// virtualIface reports container and VM bridges: no printer or phone is behind them.
func virtualIface(name string) bool {
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "podman", "lxc", "lxd", "vmnet", "vboxnet"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// localIPv4Nets lists the IPv4 address and mask of every up, non-loopback,
// non-virtual interface.
func localIPv4Nets() []*net.IPNet {
	var out []*net.IPNet
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || virtualIface(iface.Name) {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				out = append(out, &net.IPNet{IP: n.IP.To4(), Mask: n.Mask})
			}
		}
	}
	return out
}

// lanURLs lists the dashboard's http://<ip>:<port> on each private (or Tailscale)
// address of this computer, likeliest LAN first, never nil.
func lanURLs(port int) []string {
	var ips []net.IP
	seen := map[string]bool{}
	for _, n := range localIPv4Nets() {
		if (!isPrivateIP(n.IP) && !cgnat.Contains(n.IP)) || seen[n.IP.String()] {
			continue
		}
		seen[n.IP.String()] = true
		ips = append(ips, n.IP)
	}
	sort.SliceStable(ips, func(i, j int) bool {
		if addrRank(ips[i]) != addrRank(ips[j]) {
			return addrRank(ips[i]) < addrRank(ips[j])
		}
		return ips[i].String() < ips[j].String()
	})
	urls := []string{}
	for _, ip := range ips {
		urls = append(urls, "http://"+net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	}
	return urls
}

func inDocker() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func handleNetwork(w http.ResponseWriter, r *http.Request) {
	jsonHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(struct {
		Port     int      `json:"port"`
		URLs     []string `json:"urls"`
		InDocker bool     `json:"in_docker"`
	}{serverPort, lanURLs(serverPort), inDocker()})
}
