package lan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

func newUploadController(t *testing.T, url string, status string) *Controller {
	t.Helper()
	c := NewController()
	c.printers["voron"] = configpkg.Printer{Name: "voron", MoonrakerURL: url, APIKey: "moon-key"}
	if status != "" {
		c.states["voron"] = &mqttpkg.TelemetryData{Status: status}
	}
	return c
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.gcode")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUploadAndPrint_SendsFieldsAndFile(t *testing.T) {
	const body = "G28\nG1 X10\n"
	sum := sha256.Sum256([]byte(body))
	var got map[string]string
	var fileName, fileBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/server/info" {
			if r.Header.Get("X-Api-Key") != "moon-key" {
				t.Errorf("preflight without api key")
			}
			_, _ = w.Write([]byte(`{"result":{}}`))
			return
		}
		if r.URL.Path != "/server/files/upload" || r.Method != "POST" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "moon-key" {
			t.Errorf("missing api key")
		}
		if r.ContentLength <= 0 {
			t.Errorf("want a Content-Length, got %d", r.ContentLength)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		got = map[string]string{}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(part)
			if part.FormName() == "file" {
				fileName, fileBody = part.FileName(), string(b)
			} else {
				got[part.FormName()] = string(b)
			}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"item":{"path":"x.gcode"},"print_started":true,"print_queued":false,"action":"create_file"}`))
	}))
	defer srv.Close()

	c := newUploadController(t, srv.URL, "idle")
	if err := c.UploadAndPrint(context.Background(), "voron", writeTemp(t, body), "My Vase (1).gcode"); err != nil {
		t.Fatalf("UploadAndPrint: %v", err)
	}
	if got["root"] != "gcodes" || got["print"] != "true" || got["checksum"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("fields = %v", got)
	}
	if fileName != "My_Vase_1.gcode" || fileBody != body {
		t.Fatalf("file part = %q %q", fileName, fileBody)
	}
}

func TestUploadAndPrint_Errors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		reply  string
		want   string
	}{
		{"not started", 201, `{"print_started":false,"print_queued":false}`, "did not start it"},
		{"queued is not started", 201, `{"print_started":false,"print_queued":true}`, errQueued.Error()},
		{"bare started", 201, `{"item":{"path":"a.gcode"},"print_started":true,"print_queued":false,"action":"create_file"}`, ""},
		{"wrapped started", 201, `{"result":{"item":{"path":"a.gcode"},"print_started":true,"print_queued":false}}`, ""},
		{"wrapped queued", 201, `{"result":{"print_started":false,"print_queued":true}}`, errQueued.Error()},
		{"wrapped not started", 201, `{"result":{"print_started":false,"print_queued":false}}`, "did not start it"},
		{"not json", 201, `ok`, "did not start it"},
		{"file in use", 403, `{"error":{"message":"File currently in use"}}`, "printing on the printer right now"},
		{"file loaded", 403, `{"error":{"message":"File is loaded, upload not permitted"}}`, "printing on the printer right now"},
		{"bad key", 401, `{}`, errMoonKey.Error()},
		{"forbidden", 403, `{"error":{"message":"nope"}}`, errMoonKey.Error()},
		{"other", 500, `{}`, errRefused.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/server/info" { // the key preflight passes here
					_, _ = w.Write([]byte(`{}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer srv.Close()
			err := newUploadController(t, srv.URL, "idle").UploadAndPrint(context.Background(), "voron", writeTemp(t, "G28"), "a.gcode")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Moonraker answers a bad key with 401 before it reads the body, which the
// upload would see as a broken pipe: the preflight turns it into the plain
// message and no file is sent.
func TestUploadAndPrint_PreflightRefusedKeySendsNoFile(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		var uploads, infos atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/server/info" {
				infos.Add(1)
				w.WriteHeader(status)
				return
			}
			uploads.Add(1)
		}))
		err := newUploadController(t, srv.URL, "idle").UploadAndPrint(context.Background(), "voron", writeTemp(t, "G28"), "a.gcode")
		srv.Close()
		if !errors.Is(err, errMoonKey) || infos.Load() != 1 || uploads.Load() != 0 {
			t.Fatalf("status %d: err = %v, infos %d, uploads %d", status, err, infos.Load(), uploads.Load())
		}
	}
}

func TestUploadAndPrint_PreflightOtherFailureStillUploads(t *testing.T) {
	var uploads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/server/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		uploads.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"print_started":true}`))
	}))
	defer srv.Close()
	if err := newUploadController(t, srv.URL, "idle").UploadAndPrint(context.Background(), "voron", writeTemp(t, "G28"), "a.gcode"); err != nil || uploads.Load() != 1 {
		t.Fatalf("err = %v, uploads %d", err, uploads.Load())
	}
}

func TestUploadAndPrint_BusyPrinterSendsNothing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	for _, st := range []string{"printing", "paused"} {
		err := newUploadController(t, srv.URL, st).UploadAndPrint(context.Background(), "voron", writeTemp(t, "G28"), "a.gcode")
		if err == nil || !strings.Contains(err.Error(), "The printer is busy") {
			t.Fatalf("%s: err = %v", st, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("expected no request, got %d", hits.Load())
	}
}

func TestSanitizeRemoteName(t *testing.T) {
	cases := map[string]string{
		"vase.gcode":                        "vase.gcode",
		"My Vase (v2).gcode":                "My_Vase_v2.gcode",
		"../../etc/passwd":                  "etcpasswd.gcode",
		"héllo wörld.GCODE":                 "hllo_wrld.GCODE",
		"":                                  "print.gcode",
		"部品.gcode":                          "print.gcode",
		"Ünïcödé.gcode":                     "ncd.gcode",
		"vase.gcode.3mf":                    "vase.gcode.3mf.gcode",
		strings.Repeat("a", 300) + ".gcode": strings.Repeat("a", 114) + ".gcode",
	}
	for in, want := range cases {
		if got := SanitizeRemoteName(in); got != want {
			t.Errorf("SanitizeRemoteName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A dead connection is reported in plain words, without the printer address.
func TestUploadAndPrint_UnreachableHidesTheAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/server/info" {
			return
		}
		dropConn(w)
	}))
	defer srv.Close()
	err := newUploadController(t, srv.URL, "idle").UploadAndPrint(context.Background(), "voron", writeTemp(t, "G28"), "a.gcode")
	if !errors.Is(err, errUnreachable) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("err = %v", err)
	}
}

func dropConn(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		conn.Close()
	}
}

func TestPrintReady(t *testing.T) {
	c := newUploadController(t, "http://127.0.0.1:1", "printing")
	if err := c.PrintReady("voron"); !errors.Is(err, errPrinterBusy) {
		t.Fatalf("printing: %v", err)
	}
	if err := newUploadController(t, "http://127.0.0.1:1", "idle").PrintReady("voron"); err != nil {
		t.Fatalf("idle: %v", err)
	}
	if err := c.PrintReady("nope"); err == nil {
		t.Fatal("unknown printer accepted")
	}
}

func TestSendKlipperCommand_StartMissingFileIsPlain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newUploadController(t, srv.URL, "")
	err := c.sendKlipperCommand(c.printers["voron"], nil, "start", map[string]interface{}{"file_name": "gone.gcode"})
	if err == nil || strings.Contains(err.Error(), "HTTP") || !strings.Contains(err.Error(), "no file with that name") {
		t.Fatalf("err = %v", err)
	}
}
