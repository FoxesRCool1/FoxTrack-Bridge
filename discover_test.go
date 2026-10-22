package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

func TestParseBambuSSDP_Notify(t *testing.T) {
	data := "NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nLocation: 192.168.1.42\r\n" +
		"NT: urn:bambulab-com:device:3dprinter:1\r\nUSN: 01P00A123456789\r\n" +
		"DevModel.bambu.com: C12\r\nDevName.bambu.com: P1S-Garage\r\nDevConnect.bambu.com: lan\r\n" +
		"DevBind.bambu.com: occupied\r\n\r\n"
	p, ok := parseBambuSSDP([]byte(data), net.ParseIP("10.9.9.9"))
	if !ok {
		t.Fatal("NOTIFY refused")
	}
	want := discoveredPrinter{Type: "bambu", Name: "P1S-Garage", Model: "P1S", IP: "192.168.1.42", Serial: "01P00A123456789", Connect: "lan"}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}
}

func TestRememberBambu(t *testing.T) {
	bambuSeen = map[string]bambuSighting{}
	defer func() { bambuSeen = map[string]bambuSighting{} }()
	a := discoveredPrinter{Type: "bambu", IP: "192.168.1.10", Serial: "A"}
	b := discoveredPrinter{Type: "bambu", IP: "192.168.1.11", Serial: "B"}
	t0 := time.Now()

	rememberBambu([]discoveredPrinter{a}, t0)
	if got := dedupeSort(rememberBambu([]discoveredPrinter{b}, t0.Add(time.Minute))); len(got) != 2 {
		t.Fatalf("a scan that misses a printer seen a minute ago: got %+v, want both", got)
	}
	got := dedupeSort(rememberBambu(nil, t0.Add(bambuSeenFor+30*time.Second)))
	if len(got) != 1 || got[0].Serial != "B" {
		t.Errorf("after %v only the newer sighting stays: got %+v", bambuSeenFor, got)
	}
}

func TestParseBambuSSDP_SearchReplyMixedCaseAndSourceFallback(t *testing.T) {
	data := "HTTP/1.1 200 OK\r\nst: urn:bambulab-com:device:3dprinter:1\r\nusn: 00M00A999\r\n" +
		"DEVMODEL.BAMBU.COM: weird-code\r\ndevconnect.bambu.com: CLOUD\r\n\r\n"
	p, ok := parseBambuSSDP([]byte(data), net.ParseIP("192.168.1.77"))
	if !ok {
		t.Fatal("200 OK reply refused")
	}
	// No Location: the sender's address; no DevName: the IP; unknown model: the raw code.
	if p.IP != "192.168.1.77" || p.Name != "192.168.1.77" || p.Model != "weird-code" || p.Connect != "cloud" || p.Serial != "00M00A999" {
		t.Errorf("got %+v", p)
	}
}

func TestParseBambuSSDP_Refused(t *testing.T) {
	src := net.ParseIP("192.168.1.5")
	for name, data := range map[string]string{
		"no usn":      "NOTIFY * HTTP/1.1\r\nLocation: 192.168.1.42\r\nNT: urn:bambulab-com:device:3dprinter:1\r\n\r\n",
		"not bambu":   "NOTIFY * HTTP/1.1\r\nLocation: 192.168.1.42\r\nNT: upnp:rootdevice\r\nUSN: uuid:abc\r\n\r\n",
		"own msearch": string(bambuMSearch),
		"empty":       "",
	} {
		if p, ok := parseBambuSSDP([]byte(data), src); ok {
			t.Errorf("%s: accepted %+v", name, p)
		}
	}
}

func TestBambuModelName(t *testing.T) {
	for code, want := range map[string]string{
		"BL-P001": "X1 Carbon", "BL-P002": "X1", "C11": "P1P", "C12": "P1S", "C13": "X1E",
		"N1": "A1 mini", "N2S": "A1", "O1D": "H2D", "c12": "P1S", "ZZ9": "ZZ9", "": "",
	} {
		if got := bambuModelName(code); got != want {
			t.Errorf("bambuModelName(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestMarkAdded(t *testing.T) {
	found := []discoveredPrinter{
		{Type: "bambu", IP: "192.168.1.1", Serial: "SER1"},
		{Type: "bambu", IP: "192.168.1.2", Serial: "SER2"},
		{Type: "klipper", IP: "192.168.1.3", URL: "http://192.168.1.3:7125"},
		{Type: "prusalink", IP: "192.168.1.4", URL: "http://192.168.1.4"},
		{Type: "klipper", IP: "192.168.1.5", URL: "http://192.168.1.5:7125"},
		{Type: "klipper", IP: "192.168.1.6", URL: "http://192.168.1.6:7125"},
	}
	saved := []config.Printer{
		{Name: "a", IP: "192.168.1.1"},
		{Name: "b", Serial: "ser2", IP: "10.0.0.2"}, // serial matches, case-insensitive
		{Name: "c", MoonrakerURL: "http://192.168.1.3:7125/"},
		{Name: "d", PrusaLinkURL: "192.168.1.4"}, // bare host
		{Name: "e", MoonrakerURL: "http://192.168.1.50:7125"},
	}
	markAdded(found, saved)
	want := []bool{true, true, true, true, false, false}
	for i, p := range found {
		if p.Added != want[i] {
			t.Errorf("%s %s: added = %v, want %v", p.Type, p.IP, p.Added, want[i])
		}
	}
}

func TestDedupeSort(t *testing.T) {
	got := dedupeSort([]discoveredPrinter{
		{Type: "prusalink", IP: "192.168.1.70"},
		{Type: "klipper", IP: "192.168.1.100"},
		{Type: "klipper", IP: "192.168.1.60"},
		{Type: "bambu", IP: "192.168.1.42", Serial: "S1", Name: "first"},
		{Type: "bambu", IP: "192.168.1.42", Serial: "S1", Name: "same ip"},
		{Type: "bambu", IP: "192.168.1.43", Serial: "s1", Name: "same serial"},
		{Type: "klipper", IP: "192.168.1.60"},
	})
	var keys []string
	for _, p := range got {
		keys = append(keys, p.Type+" "+p.IP)
	}
	want := "bambu 192.168.1.42,klipper 192.168.1.60,klipper 192.168.1.100,prusalink 192.168.1.70"
	if strings.Join(keys, ",") != want {
		t.Errorf("got %s, want %s", strings.Join(keys, ","), want)
	}
	if got[0].Name != "first" {
		t.Errorf("kept %q, want the first seen", got[0].Name)
	}
	if empty := dedupeSort(nil); empty == nil {
		t.Error("dedupeSort(nil) = nil, want an empty slice")
	}
}

func ipnet(ip string, ones int) *net.IPNet {
	return &net.IPNet{IP: net.ParseIP(ip).To4(), Mask: net.CIDRMask(ones, 32)}
}

func TestScanTargets(t *testing.T) {
	got := scanTargets([]*net.IPNet{ipnet("192.168.1.10", 24)}, 1024)
	if len(got) != 253 || got[0] != "192.168.1.1" || got[len(got)-1] != "192.168.1.254" {
		t.Fatalf("got %d hosts %v..%v, want .1-.254 minus the host", len(got), got[0], got[len(got)-1])
	}
	for _, h := range got {
		if h == "192.168.1.10" {
			t.Fatal("scanned the host's own address")
		}
	}

	// A /16 still scans only the host's /24; a second address on the same /24 is
	// not scanned twice and is skipped as our own; public and /32 CGNAT addresses are ignored.
	got = scanTargets([]*net.IPNet{ipnet("10.1.2.3", 16), ipnet("10.1.2.4", 24), ipnet("8.8.8.8", 24), ipnet("100.64.1.5", 32)}, 1024)
	if len(got) != 252 {
		t.Errorf("got %d hosts, want 252", len(got))
	}
	for _, h := range got {
		if !strings.HasPrefix(h, "10.1.2.") {
			t.Fatalf("scanned %s outside the host's /24", h)
		}
	}

	if got := scanTargets([]*net.IPNet{ipnet("192.168.1.10", 24), ipnet("172.16.5.9", 24)}, 300); len(got) != 300 {
		t.Errorf("cap: got %d hosts, want 300", len(got))
	}

	// Under the cap, a docker bridge listed first must not push out the home LAN.
	got = scanTargets([]*net.IPNet{ipnet("172.17.0.1", 16), ipnet("10.0.0.5", 24), ipnet("192.168.1.10", 24)}, 100)
	if len(got) != 100 || !strings.HasPrefix(got[0], "192.168.1.") || !strings.HasPrefix(got[99], "192.168.1.") {
		t.Errorf("cap order: got %v ... %v", got[0], got[len(got)-1])
	}
}

func testClient() *http.Client { return &http.Client{Timeout: 2 * time.Second} }

func TestDiscoverKlipper(t *testing.T) {
	moonraker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/printer/info" || r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			http.Error(w, "unexpected", 400)
			return
		}
		w.Write([]byte(`{"result":{"state":"ready","hostname":"voron"}}`))
	}))
	defer moonraker.Close()
	if host, ok := discoverKlipper(context.Background(), testClient(), moonraker.URL); !ok || host != "voron" {
		t.Errorf("moonraker: got %q, %v", host, ok)
	}

	for name, h := range map[string]http.HandlerFunc{
		"404":       func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"html":      func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>hi</html>")) },
		"no result": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"error":{"code":401}}`)) },
	} {
		srv := httptest.NewServer(h)
		if _, ok := discoverKlipper(context.Background(), testClient(), srv.URL); ok {
			t.Errorf("%s: classified as klipper", name)
		}
		srv.Close()
	}
}

func TestDiscoverPrusaLink(t *testing.T) {
	for name, tc := range map[string]struct {
		h        http.HandlerFunc
		wantName string
		wantOK   bool
	}{
		"200 with hostname": {func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"api":"2.0.0","server":"2.1.2","text":"PrusaLink","hostname":"prusa-mk4"}`))
		}, "prusa-mk4", true},
		"200 without hostname": {func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"text":"prusalink"}`))
		}, "PrusaLink printer", true},
		"401 in header": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Digest realm="PrusaLink"`)
			w.WriteHeader(401)
		}, "PrusaLink printer", true},
		"401 in body": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			w.Write([]byte("Unauthorized - Prusa"))
		}, "PrusaLink printer", true},
		"401 plain": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			w.Write([]byte("go away"))
		}, "", false},
		"octoprint": {func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"server":"1.2.1","api":"0.1","text":"OctoPrint 1.2.1"}`))
		}, "", false},
		"404": {func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "", false},
	} {
		srv := httptest.NewServer(tc.h)
		name2, ok := discoverPrusaLink(context.Background(), testClient(), srv.URL)
		srv.Close()
		if ok != tc.wantOK || name2 != tc.wantName {
			t.Errorf("%s: got %q, %v; want %q, %v", name, name2, ok, tc.wantName, tc.wantOK)
		}
	}
}

func TestDiscoverHost_NothingOpen(t *testing.T) {
	if p, ok := discoverHost(context.Background(), testClient(), "192.0.2.1", func(int) bool { return false }); ok {
		t.Errorf("classified a closed host: %+v", p)
	}
}

func TestHandleDiscover_RejectsNonGET(t *testing.T) {
	rec := httptest.NewRecorder()
	handleDiscover(rec, httptest.NewRequest("POST", "/api/discover", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}

func TestHandleNetwork(t *testing.T) {
	old := serverPort
	serverPort = 8080
	defer func() { serverPort = old }()

	rec := httptest.NewRecorder()
	handleNetwork(rec, httptest.NewRequest("GET", "/api/network", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["urls"]) == "null" || raw["urls"] == nil {
		t.Errorf("urls = %s, want an array", raw["urls"])
	}
	if string(raw["port"]) != "8080" {
		t.Errorf("port = %s, want 8080", raw["port"])
	}
	if _, ok := raw["in_docker"]; !ok {
		t.Error("in_docker missing")
	}

	rec = httptest.NewRecorder()
	handleNetwork(rec, httptest.NewRequest("POST", "/api/network", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}

func TestLanURLs_NonNilAndPrefixed(t *testing.T) {
	urls := lanURLs(9999)
	if urls == nil {
		t.Fatal("lanURLs returned nil")
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "http://") || !strings.HasSuffix(u, ":9999") {
			t.Errorf("bad url %q", u)
		}
	}
}
