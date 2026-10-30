package lan

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configpkg "foxtrack-bridge/config"
	"foxtrack-bridge/webhook"
)

func octoJSON(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func octoPrinterJSON(text string, flags string, errText string) string {
	return `{"temperature":{"tool0":{"actual":214.9,"target":215.0},"bed":{"actual":59.8,"target":60.0}},
		"state":{"text":"` + text + `","flags":{` + flags + `},"error":"` + errText + `"}}`
}

const octoJobPrinting = `{"job":{"file":{"name":"cube.gcode","display":"Cube v2.gcode","origin":"local"},"estimatedPrintTime":3600},
	"progress":{"completion":42.5,"printTime":1500,"printTimeLeft":1830},"state":"Printing"}`

func TestOctoPrintTelemetry_States(t *testing.T) {
	cases := []struct {
		name, flags, job, want string
		pct, left              int
		file                   string
	}{
		{"printing", `"operational":true,"printing":true,"error":false`, octoJobPrinting, "printing", 42, 30, "Cube v2.gcode"},
		{"paused", `"operational":true,"paused":true`, octoJobPrinting, "paused", 42, 30, "Cube v2.gcode"},
		{"pausing", `"operational":true,"printing":true,"pausing":true`, octoJobPrinting, "paused", 42, 30, "Cube v2.gcode"},
		{"error", `"error":true,"closedOrError":true`, octoJobPrinting, "error", 0, 0, ""},
		{"idle after cancel", `"operational":true,"ready":true`, `{"job":{"file":{"name":"cube.gcode"}},"progress":{"completion":42.5}}`, "idle", 0, 0, ""},
		{"finished", `"operational":true,"ready":true`, `{"job":{"file":{"name":"cube.gcode"}},"progress":{"completion":100.0}}`, "finished", 0, 0, ""},
		{"idle no job", `"operational":true,"ready":true`, `{"job":{"file":{"name":null}},"progress":{"completion":null}}`, "idle", 0, 0, ""},
	}
	for _, tc := range cases {
		got := octoPrintTelemetry(octoJSON(t, octoPrinterJSON("x", tc.flags, "")), octoJSON(t, tc.job))
		if got.Status != tc.want || got.Progress != tc.pct || got.TimeRemaining != tc.left || got.FileName != tc.file {
			t.Errorf("%s: status=%q pct=%d left=%d file=%q", tc.name, got.Status, got.Progress, got.TimeRemaining, got.FileName)
		}
		if got.NozzleTemp != 214.9 || got.BedTarget != 60 {
			t.Errorf("%s: temps %v %v", tc.name, got.NozzleTemp, got.BedTarget)
		}
	}
}

func TestOctoPrintAlarms_ErrorText(t *testing.T) {
	st := nestedMap(octoJSON(t, octoPrinterJSON("Error", `"error":true`, "Printer halted. kill() called!")), "state")
	a := octoPrintAlarms(st)
	if len(a) != 1 || a[0].Severity != webhook.AlarmSerious || a[0].Message == nil || *a[0].Message != "Printer halted. kill() called!" {
		t.Fatalf("alarms = %+v", a)
	}
	if a := octoPrintAlarms(nestedMap(octoJSON(t, octoPrinterJSON("Printing", `"printing":true`, "")), "state")); a != nil {
		t.Fatalf("no error must give no alarm, got %+v", a)
	}
}

func TestFetchOctoPrintTelemetry_OfflineIs409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "octo-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	_, _, err := fetchTelemetry(configpkg.Printer{Name: "Ender", OctoPrintURL: srv.URL, APIKey: "octo-key"})
	if err != errOctoOffline {
		t.Fatalf("err = %v", err)
	}
	_, _, err = fetchTelemetry(configpkg.Printer{Name: "Ender", OctoPrintURL: srv.URL, APIKey: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("refused key err = %v", err)
	}
}

func TestOctoPrintUpload_RequestShape(t *testing.T) {
	var gotPath, gotKey, gotContentType string
	var fields map[string]string
	var fileName, fileBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte(`{"api":"0.1","server":"1.10.0"}`))
			return
		}
		gotPath, gotKey, gotContentType = r.Method+" "+r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart: %v", err)
		}
		fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		if fh := r.MultipartForm.File["file"]; len(fh) == 1 {
			fileName = fh[0].Filename
			f, _ := fh[0].Open()
			b, _ := io.ReadAll(f)
			fileBody = string(b)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	local := writeTemp(t, "G28\nG1 X10\n")
	p := configpkg.Printer{Name: "Ender", OctoPrintURL: srv.URL, APIKey: "octo-key"}
	if err := octoPrintUpload(context.Background(), p, local, "my part.gcode", func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /api/files/local" || gotKey != "octo-key" || !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Fatalf("request = %s key=%s ct=%s", gotPath, gotKey, gotContentType)
	}
	if fields["select"] != "true" || fields["print"] != "true" || fileName != "my_part.gcode" || fileBody != "G28\nG1 X10\n" {
		t.Fatalf("fields=%v file=%q body=%q", fields, fileName, fileBody)
	}
}

func TestResolveOctoCamURL_LoopbackAndRelative(t *testing.T) {
	base := "http://octopi.local"
	cases := map[string]string{
		"http://127.0.0.1:8080/?action=snapshot": "http://octopi.local:8080/?action=snapshot",
		"/webcam/?action=stream":                 "http://octopi.local/webcam/?action=stream",
		"http://192.168.1.9/cam.jpg":             "http://192.168.1.9/cam.jpg",
		"":                                       "",
	}
	for in, want := range cases {
		if got := resolveOctoCamURL(base, in); got != want {
			t.Errorf("resolve(%q) = %q, want %q", in, got, want)
		}
	}
}
