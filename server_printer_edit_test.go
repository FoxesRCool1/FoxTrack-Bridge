//go:build headless

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foxtrack-bridge/config"
)

func strp(s string) *string { return &s }

func editFixture() *config.Config {
	return &config.Config{Printers: []config.Printer{
		{ID: "a", Name: "Monsieur", IP: "192.168.87.22", Serial: "01P00C580801716", LANCode: "81f1aafd"},
		{ID: "b", Name: "Sherlock", IP: "192.168.87.24", Serial: "01P00C580800360", LANCode: "f849c383"},
		{ID: "c", Name: "Cloudy", IP: "10.0.0.5", Serial: "S1", LANCode: "cloudcode", Connection: "cloud"},
		{ID: "k", Name: "Voron", MoonrakerURL: "http://10.0.0.7:7125", APIKey: "mkey", WebcamURL: "http://10.0.0.7/cam"},
	}}
}

func notBusy(string) bool { return false }

func TestApplyPrinterEdit_ChangesBambuIPAndKeepsSecret(t *testing.T) {
	old := editFixture()
	got, idx, err := applyPrinterEdit(old, "a", printerEdit{IP: strp("192.168.1.50"), LANCode: strp("")}, notBusy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0", idx)
	}
	if got.Printers[0].IP != "192.168.1.50" {
		t.Fatalf("IP = %q, want 192.168.1.50", got.Printers[0].IP)
	}
	if got.Printers[0].LANCode != "81f1aafd" {
		t.Fatalf("LANCode = %q, want 81f1aafd", got.Printers[0].LANCode)
	}
	if got.Printers[0].ID != "a" {
		t.Fatalf("ID = %q, want a", got.Printers[0].ID)
	}
	if got.Printers[0].Serial != "01P00C580801716" {
		t.Fatalf("Serial = %q, want 01P00C580801716", got.Printers[0].Serial)
	}
	if old.Printers[0].IP != "192.168.87.22" {
		t.Fatalf("old IP = %q, want 192.168.87.22 (old not mutated)", old.Printers[0].IP)
	}
	if len(got.Printers) != 4 {
		t.Fatalf("len(printers) = %d, want 4", len(got.Printers))
	}
}

func TestApplyPrinterEdit_NewLANCodeReplacesStored(t *testing.T) {
	old := editFixture()
	got, _, err := applyPrinterEdit(old, "a", printerEdit{LANCode: strp(" newcode1 ")}, notBusy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Printers[0].LANCode != "newcode1" {
		t.Fatalf("LANCode = %q, want newcode1", got.Printers[0].LANCode)
	}
}

func TestApplyPrinterEdit_MatchesByNameWhenNoID(t *testing.T) {
	old := editFixture()
	got, idx, err := applyPrinterEdit(old, "Sherlock", printerEdit{IP: strp("192.168.1.60")}, notBusy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1", idx)
	}
	if got.Printers[1].IP != "192.168.1.60" {
		t.Fatalf("IP = %q, want 192.168.1.60", got.Printers[1].IP)
	}
}

func TestApplyPrinterEdit_UnknownTokenIsNotFound(t *testing.T) {
	old := editFixture()
	_, _, err := applyPrinterEdit(old, "nope", printerEdit{IP: strp("1.1.1.1")}, notBusy)
	if !errors.Is(err, errPrinterNotFound) {
		t.Fatalf("err = %v, want errPrinterNotFound", err)
	}
}

func TestApplyPrinterEdit_RenameRecordsPreviousName(t *testing.T) {
	old := editFixture()
	got, _, err := applyPrinterEdit(old, "a", printerEdit{Name: strp("  Hercule ")}, notBusy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Printers[0].Name != "Hercule" {
		t.Fatalf("Name = %q, want Hercule", got.Printers[0].Name)
	}
	want := []string{"Monsieur"}
	if len(got.Printers[0].PreviousNames) != len(want) {
		t.Fatalf("PreviousNames = %v, want %v", got.Printers[0].PreviousNames, want)
	}
	for i := range want {
		if got.Printers[0].PreviousNames[i] != want[i] {
			t.Fatalf("PreviousNames[%d] = %q, want %q", i, got.Printers[0].PreviousNames[i], want[i])
		}
	}
}

func TestApplyPrinterEdit_DuplicateNameRejected(t *testing.T) {
	old := editFixture()
	_, _, err := applyPrinterEdit(old, "a", printerEdit{Name: strp("sherlock")}, notBusy)
	if !errors.Is(err, errDuplicatePrinterName) {
		t.Fatalf("err = %v, want errDuplicatePrinterName", err)
	}
}

func TestApplyPrinterEdit_CaseOnlyRenameAllowed(t *testing.T) {
	old := editFixture()
	got, _, err := applyPrinterEdit(old, "a", printerEdit{Name: strp("MONSIEUR")}, notBusy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Printers[0].Name != "MONSIEUR" {
		t.Fatalf("Name = %q, want MONSIEUR", got.Printers[0].Name)
	}
}

func TestApplyPrinterEdit_BlankNameRejected(t *testing.T) {
	old := editFixture()
	_, _, err := applyPrinterEdit(old, "a", printerEdit{Name: strp("   ")}, notBusy)
	if !errors.Is(err, errBlankPrinterName) {
		t.Fatalf("err = %v, want errBlankPrinterName", err)
	}
}

func TestApplyPrinterEdit_RenameWhilePrintingRejected(t *testing.T) {
	old := editFixture()
	busy := func(name string) bool { return name == "Monsieur" }

	_, _, err := applyPrinterEdit(old, "a", printerEdit{Name: strp("Hercule")}, busy)
	if !errors.Is(err, errRenameWhilePrinting) {
		t.Fatalf("err = %v, want errRenameWhilePrinting", err)
	}

	_, _, err = applyPrinterEdit(old, "a", printerEdit{IP: strp("192.168.1.70")}, busy)
	if err != nil {
		t.Fatalf("err = %v, want nil (IP change allowed during print)", err)
	}

	_, _, err = applyPrinterEdit(old, "a", printerEdit{Name: strp("Monsieur")}, busy)
	if err != nil {
		t.Fatalf("err = %v, want nil (same name is not a rename)", err)
	}
}

func TestApplyPrinterEdit_BlankRequiredFieldsRejected(t *testing.T) {
	cases := []struct {
		name  string
		token string
		edit  printerEdit
	}{
		{"bambu ip", "a", printerEdit{IP: strp(" ")}},
		{"bambu serial", "a", printerEdit{Serial: strp("")}},
		{"moonraker url", "k", printerEdit{MoonrakerURL: strp("")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := editFixture()
			_, _, err := applyPrinterEdit(old, tc.token, tc.edit, notBusy)
			if !errors.Is(err, errInvalidPrinterEdit) {
				t.Fatalf("err = %v, want errInvalidPrinterEdit", err)
			}
		})
	}
}

func TestApplyPrinterEdit_CloudKeepsAccountFields(t *testing.T) {
	old := editFixture()
	got, _, err := applyPrinterEdit(old, "c", printerEdit{Serial: strp("X"), LANCode: strp("other"), IP: strp("")}, notBusy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Printers[2].Serial != "S1" {
		t.Fatalf("Serial = %q, want S1", got.Printers[2].Serial)
	}
	if got.Printers[2].LANCode != "cloudcode" {
		t.Fatalf("LANCode = %q, want cloudcode", got.Printers[2].LANCode)
	}
	if got.Printers[2].IP != "" {
		t.Fatalf("IP = %q, want empty", got.Printers[2].IP)
	}
	if got.Printers[2].Connection != "cloud" {
		t.Fatalf("Connection = %q, want cloud", got.Printers[2].Connection)
	}
}

func TestApplyPrinterEdit_KlipperFields(t *testing.T) {
	old := editFixture()
	got, _, err := applyPrinterEdit(old, "k", printerEdit{
		MoonrakerURL: strp("http://10.0.0.8:7125"),
		APIKey:       strp(""),
		WebcamURL:    strp(""),
		Serial:       strp("X"),
		LANCode:      strp("Y"),
	}, notBusy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Printers[3].MoonrakerURL != "http://10.0.0.8:7125" {
		t.Fatalf("MoonrakerURL = %q, want http://10.0.0.8:7125", got.Printers[3].MoonrakerURL)
	}
	if got.Printers[3].APIKey != "mkey" {
		t.Fatalf("APIKey = %q, want mkey", got.Printers[3].APIKey)
	}
	if got.Printers[3].WebcamURL != "" {
		t.Fatalf("WebcamURL = %q, want empty", got.Printers[3].WebcamURL)
	}
	if got.Printers[3].Serial != "" {
		t.Fatalf("Serial = %q, want empty", got.Printers[3].Serial)
	}
	if got.Printers[3].LANCode != "" {
		t.Fatalf("LANCode = %q, want empty", got.Printers[3].LANCode)
	}
}

func TestApplyPrinterEdit_OmittedFieldsUnchanged(t *testing.T) {
	old := editFixture()
	got, _, err := applyPrinterEdit(old, "b", printerEdit{}, notBusy)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Printers[1].Name != old.Printers[1].Name {
		t.Fatalf("Name = %q, want %q", got.Printers[1].Name, old.Printers[1].Name)
	}
	if got.Printers[1].IP != old.Printers[1].IP {
		t.Fatalf("IP = %q, want %q", got.Printers[1].IP, old.Printers[1].IP)
	}
	if got.Printers[1].Serial != old.Printers[1].Serial {
		t.Fatalf("Serial = %q, want %q", got.Printers[1].Serial, old.Printers[1].Serial)
	}
	if got.Printers[1].LANCode != old.Printers[1].LANCode {
		t.Fatalf("LANCode = %q, want %q", got.Printers[1].LANCode, old.Printers[1].LANCode)
	}
}

func TestAddPreviousName(t *testing.T) {
	cases := []struct {
		name    string
		names   []string
		oldName string
		newName string
		want    []string
	}{
		{"first rename", nil, "A", "B", []string{"A"}},
		{"second rename keeps order", []string{"A"}, "B", "C", []string{"A", "B"}},
		{"rename back drops current name", []string{"A"}, "B", "A", []string{"B"}},
		{"no duplicate of old name", []string{"B", "A"}, "B", "C", []string{"A", "B"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := addPreviousName(tc.names, tc.oldName, tc.newName)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHandlePrinterByName_PUTUnknownPrinterIs404(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	prev := configStore
	configStore = editFixture()
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = prev
		configMutex.Unlock()
	})

	req := httptest.NewRequest("PUT", "/api/printers/nope", strings.NewReader(`{"ip":"1.1.1.1"}`))
	rec := httptest.NewRecorder()
	handlePrinterByName(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "\"error\"") {
		t.Fatalf("body = %q, want to contain \"error\"", rec.Body.String())
	}
}

func TestHandlePrinterByName_PUTDuplicateNameIs409(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	prev := configStore
	configStore = editFixture()
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = prev
		configMutex.Unlock()
	})

	req := httptest.NewRequest("PUT", "/api/printers/a", strings.NewReader(`{"name":"Sherlock"}`))
	rec := httptest.NewRecorder()
	handlePrinterByName(rec, req)

	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	configMutex.RLock()
	name := configStore.Printers[0].Name
	configMutex.RUnlock()
	if name != "Monsieur" {
		t.Fatalf("Name = %q, want Monsieur", name)
	}
}

func TestHandlePrinterByName_PUTInvalidJSONIs400(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	prev := configStore
	configStore = editFixture()
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = prev
		configMutex.Unlock()
	})

	req := httptest.NewRequest("PUT", "/api/printers/a", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	handlePrinterByName(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func fakeCameraListener(t *testing.T, serve func(net.Conn)) string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "printer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		serve(c)
	}()
	return ln.Addr().String()
}

func frameBytes(payload []byte) []byte {
	hdr := make([]byte, 16)
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	return append(hdr, payload...)
}

func TestProxyBambuCamera_NoPictureReturns502(t *testing.T) {
	prev := bambuFirstFrameTimeout
	bambuFirstFrameTimeout = 300 * time.Millisecond
	t.Cleanup(func() { bambuFirstFrameTimeout = prev })

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	addr := fakeCameraListener(t, func(c net.Conn) {
		auth := make([]byte, 80)
		io.ReadFull(c, auth)
		<-done
	})

	rec := httptest.NewRecorder()
	start := time.Now()
	proxyBambuCamera(rec, addr, "code", "P")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sent no picture") {
		t.Fatalf("body = %q, want to contain 'sent no picture'", rec.Body.String())
	}
	if time.Since(start) >= 5*time.Second {
		t.Fatalf("took %v, want < 5s", time.Since(start))
	}
}

func TestProxyBambuCamera_StreamsJPEGAfterAuth(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0x01, 0x02, 0xFF, 0xD9}
	gotAuth := make(chan []byte, 1)

	addr := fakeCameraListener(t, func(c net.Conn) {
		auth := make([]byte, 80)
		if _, err := io.ReadFull(c, auth); err != nil {
			gotAuth <- nil
			return
		}
		gotAuth <- auth
		c.Write(frameBytes([]byte{0x00, 0x01, 0x02, 0x03}))
		c.Write(frameBytes(jpeg))
	})

	rec := httptest.NewRecorder()
	proxyBambuCamera(rec, addr, "code", "P")

	auth := <-gotAuth
	if auth == nil {
		t.Fatalf("auth = nil, want 80-byte auth packet")
	}
	if binary.LittleEndian.Uint32(auth[0:4]) != 0x40 {
		t.Fatalf("magic = %d, want 0x40", binary.LittleEndian.Uint32(auth[0:4]))
	}
	if binary.LittleEndian.Uint32(auth[4:8]) != 0x3000 {
		t.Fatalf("command = %d, want 0x3000", binary.LittleEndian.Uint32(auth[4:8]))
	}
	if !bytes.HasPrefix(auth[16:48], []byte("bblp")) {
		t.Fatalf("username field = %q, want prefix 'bblp'", auth[16:48])
	}
	if !bytes.HasPrefix(auth[48:80], []byte("code")) {
		t.Fatalf("password field = %q, want prefix 'code'", auth[48:80])
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "multipart/x-mixed-replace") {
		t.Fatalf("Content-Type = %q, want prefix multipart/x-mixed-replace", rec.Header().Get("Content-Type"))
	}
	if !bytes.Contains(rec.Body.Bytes(), jpeg) {
		t.Fatalf("body does not contain JPEG frame")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("--bambu")) {
		t.Fatalf("body does not contain boundary --bambu")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte{0x00, 0x01, 0x02, 0x03}) {
		t.Fatalf("body contains non-JPEG metadata frame, want it skipped")
	}
}
