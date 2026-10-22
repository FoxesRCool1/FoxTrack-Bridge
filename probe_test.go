package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"

	"foxtrack-bridge/config"
)

// runProbe posts body to handleTest and decodes the answer.
func runProbe(t *testing.T, body string) probeResult {
	t.Helper()
	rec := httptest.NewRecorder()
	handleTest(rec, httptest.NewRequest("POST", "/api/test", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res probeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("bad response %q: %v", rec.Body.String(), err)
	}
	return res
}

func wantProbe(t *testing.T, res probeResult, code string, ok bool) {
	t.Helper()
	if res.Code != code || res.OK != ok {
		t.Fatalf("got ok=%v code=%q message=%q, want ok=%v code=%q", res.OK, res.Code, res.Message, ok, code)
	}
	if res.Message == "" {
		t.Fatal("empty message")
	}
}

func TestProbeRejectsBadRequests(t *testing.T) {
	rec := httptest.NewRecorder()
	handleTest(rec, httptest.NewRequest("GET", "/api/test", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	handleTest(rec, httptest.NewRequest("POST", "/api/test", strings.NewReader("{nope")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON status = %d, want 400", rec.Code)
	}
}

func TestProbeMoonraker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/printer/info":
			http.NotFound(w, r)
		case r.Header.Get("X-Api-Key") != "secret":
			http.Error(w, "no", http.StatusUnauthorized)
		default:
			w.Write([]byte(`{"result":{"state":"ready","hostname":"voron"}}`))
		}
	}))
	defer srv.Close()

	res := runProbe(t, `{"moonraker_url":"`+srv.URL+`/","api_key":"secret"}`)
	wantProbe(t, res, probeOK, true)
	if res.Message != "Connected to voron (ready)." || !res.Reachable {
		t.Fatalf("message = %q reachable = %v", res.Message, res.Reachable)
	}
	res = runProbe(t, `{"moonraker_url":"`+srv.URL+`","api_key":"wrong"}`)
	wantProbe(t, res, probeAuth, false)
	if !res.Reachable {
		t.Fatal("a refused key still reaches the printer")
	}
	// The older dashboard sends just the URL and reads reachable.
	if res = runProbe(t, `{"moonraker_url":"`+srv.URL+`"}`); !res.Reachable || res.OK {
		t.Fatalf("keyless: reachable=%v ok=%v, want true/false", res.Reachable, res.OK)
	}

	notMoonraker := httptest.NewServer(http.NotFoundHandler())
	defer notMoonraker.Close()
	wantProbe(t, runProbe(t, `{"moonraker_url":"`+notMoonraker.URL+`"}`), probeNotFound, false)

	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) }))
	defer html.Close()
	wantProbe(t, runProbe(t, `{"moonraker_url":"`+html.URL+`"}`), probeNotFound, false)

	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	res = runProbe(t, `{"moonraker_url":"`+url+`"}`)
	wantProbe(t, res, probeUnreachable, false)
	if !strings.Contains(res.Message, url) || res.Reachable || res.Detail == "" || res.Hint == "" {
		t.Fatalf("unreachable result = %+v", res)
	}

	wantProbe(t, runProbe(t, `{"moonraker_url":"192.168.1.60:7125"}`), probeBadInput, false)
}

func TestProbePrusaLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"api":"2.0.0","hostname":"mk4"}`))
	}))
	defer srv.Close()

	res := runProbe(t, `{"prusalink_url":"`+srv.URL+`","api_key":"secret"}`)
	wantProbe(t, res, probeOK, true)
	if res.Message != "Connected to mk4." {
		t.Fatalf("message = %q", res.Message)
	}
	res = runProbe(t, `{"prusalink_url":"`+srv.URL+`","api_key":"wrong"}`)
	wantProbe(t, res, probeAuth, false)
	if !strings.Contains(res.Hint, "PrusaLink") {
		t.Fatalf("hint = %q", res.Hint)
	}
}

func TestProbeHomeAssistant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/":
			w.Write([]byte(`{"message":"API running."}`))
		case "/api/states/sensor.printer":
			w.Write([]byte(`{"state":"printing"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	body := func(token, entity string) string {
		return `{"home_assistant_url":"` + srv.URL + `","home_assistant_token":"` + token + `","ha_state_entity":"` + entity + `"}`
	}

	wantProbe(t, runProbe(t, body("bad", "sensor.printer")), probeAuth, false)
	res := runProbe(t, body("tok", "sensor.nope"))
	wantProbe(t, res, probeNotFound, false)
	if res.Message != "Home Assistant has no entity sensor.nope." {
		t.Fatalf("message = %q", res.Message)
	}
	res = runProbe(t, body("tok", "sensor.printer"))
	wantProbe(t, res, probeOK, true)
	if res.Message != "Connected. sensor.printer is printing." {
		t.Fatalf("message = %q", res.Message)
	}
	wantProbe(t, runProbe(t, body("", "sensor.printer")), probeBadInput, false)
}

func TestProbeFillsBlankSecretFromSavedPrinter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "saved-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"result":{"state":"ready","hostname":"voron"}}`))
	}))
	defer srv.Close()
	prev := configStore
	configStore = &config.Config{Printers: []config.Printer{{ID: "abc", Name: "voron", MoonrakerURL: srv.URL, APIKey: "saved-key"}}}
	defer func() { configStore = prev }()

	wantProbe(t, runProbe(t, `{"id":"abc","moonraker_url":"`+srv.URL+`","api_key":""}`), probeOK, true)
	// A typed key wins over the saved one; an unknown id fills nothing.
	wantProbe(t, runProbe(t, `{"id":"abc","moonraker_url":"`+srv.URL+`","api_key":"typed"}`), probeAuth, false)
	wantProbe(t, runProbe(t, `{"id":"nope","moonraker_url":"`+srv.URL+`","api_key":""}`), probeAuth, false)
	if got := configStore.Printers[0].APIKey; got != "saved-key" {
		t.Fatalf("a test changed the saved key to %q", got)
	}
}

func TestProbeBambuInputAndCloud(t *testing.T) {
	for _, body := range []string{
		`{"serial":"S","lan_code":"1"}`,
		`{"ip":"127.0.0.1","lan_code":"1"}`,
		`{"ip":"127.0.0.1","serial":"S","lan_code":""}`,
	} {
		res := runProbe(t, body)
		wantProbe(t, res, probeBadInput, false)
		if res.Hint == "" {
			t.Fatalf("%s: no hint", body)
		}
	}
	res := runProbe(t, `{"name":"x","serial":"S","connection":"cloud"}`)
	wantProbe(t, res, probeUnsupported, true)
	if res.Message != "Bambu Cloud printers connect through your Bambu account." {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestProbeBambuUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	setBambuPort(t, port)
	res := runProbe(t, `{"ip":"127.0.0.1","serial":"S","lan_code":"1"}`)
	wantProbe(t, res, probeUnreachable, false)
	if !strings.Contains(res.Message, "127.0.0.1") {
		t.Fatalf("message = %q", res.Message)
	}
}

func setBambuPort(t *testing.T, port string) {
	t.Helper()
	prev := bambuProbePort
	bambuProbePort = port
	t.Cleanup(func() { bambuProbePort = prev })
}

// fakeBambuBroker is a TLS listener with a throwaway certificate. handle runs
// for each accepted connection, after the client's CONNECT packet is read.
func fakeBambuBroker(t *testing.T, handle func(net.Conn)) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake"}, NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := packets.ReadPacket(conn); err == nil {
					handle(conn)
				}
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	setBambuPort(t, port)
}

func connack(conn net.Conn, code byte) {
	p := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
	p.ReturnCode = code
	p.Write(conn)
}

// serveSession answers CONNECT with CONNACK 0, then the client's packets: a
// SUBSCRIBE gets a SUBACK (or, if dropOnSubscribe, a closed connection) and a
// PUBLISH gets a report back.
func serveSession(conn net.Conn, dropOnSubscribe bool) {
	connack(conn, 0)
	for {
		pk, err := packets.ReadPacket(conn)
		if err != nil {
			return
		}
		switch p := pk.(type) {
		case *packets.SubscribePacket:
			if dropOnSubscribe {
				return
			}
			ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
			ack.MessageID = p.MessageID
			ack.ReturnCodes = []byte{0}
			ack.Write(conn)
		case *packets.PublishPacket:
			rep := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
			rep.TopicName = "device/SERIAL1/report"
			rep.Payload = []byte(`{"print":{}}`)
			rep.Write(conn)
		case *packets.DisconnectPacket:
			return
		}
	}
}

const bambuBody = `{"name":"p1s","ip":"127.0.0.1","serial":"SERIAL1","lan_code":"12345678"}`

func TestProbeBambuAccessCodeRejected(t *testing.T) {
	fakeBambuBroker(t, func(conn net.Conn) { connack(conn, 5) })
	res := runProbe(t, bambuBody)
	wantProbe(t, res, probeAuth, false)
	if res.Message != "The printer rejected the access code." || !res.Reachable {
		t.Fatalf("result = %+v", res)
	}
}

func TestProbeBambuAnswers(t *testing.T) {
	fakeBambuBroker(t, func(conn net.Conn) { serveSession(conn, false) })
	res := runProbe(t, bambuBody)
	wantProbe(t, res, probeOK, true)
	if res.Message != "Connected to SERIAL1. The printer answered." {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestProbeBambuWrongSerial(t *testing.T) {
	fakeBambuBroker(t, func(conn net.Conn) { serveSession(conn, true) })
	res := runProbe(t, bambuBody)
	wantProbe(t, res, probeSerial, false)
	if !strings.Contains(res.Message, "SERIAL1") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestProbeBambuReachOnlyForOlderDashboard(t *testing.T) {
	fakeBambuBroker(t, func(conn net.Conn) { connack(conn, 5) })
	res := runProbe(t, `{"ip":"127.0.0.1"}`)
	wantProbe(t, res, probeOK, true)
	if !res.Reachable {
		t.Fatal("reachable = false")
	}
}
