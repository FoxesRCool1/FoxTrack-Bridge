//go:build headless

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

const testFileBody = "G28\nG1 X10 Y10\nM84\n"

type printEnv struct {
	t           *testing.T
	fox         *httptest.Server
	fileHits    atomic.Int32
	fileHandler func(w http.ResponseWriter, r *http.Request) // nil = serve testFileBody
	mu          sync.Mutex
	events      []string // "ack:running", "ack:failed:<msg>", "upload:<name>", ...
	getHeader   http.Header
	pending     []bridgeCommand
	ackFail     map[string]int // status -> times to answer 500 first
	runningHTTP int            // status to answer for a "running" ack (0 = 200)
	runningDrop int            // "running" acks to answer by cutting the connection (-1 = all)
	runningTry  atomic.Int32
	doneHTTP    int // status to answer for a "done" ack (0 = 200)
	uploadGate  chan struct{}
	uploadSeen  chan struct{}
}

// dropConnection ends the request without any HTTP answer, as a timeout or a
// reset would look to the client.
func dropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func (e *printEnv) log(ev string) {
	e.mu.Lock()
	e.events = append(e.events, ev)
	e.mu.Unlock()
}

func (e *printEnv) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func (e *printEnv) count(prefix string) int {
	n := 0
	for _, ev := range e.snapshot() {
		if strings.HasPrefix(ev, prefix) {
			n++
		}
	}
	return n
}

func newPrintEnv(t *testing.T) *printEnv {
	t.Helper()
	e := &printEnv{t: t, ackFail: map[string]int{}}

	// The download link must be on the FoxTrack host Bridge polls, so the same
	// server serves /file.
	e.fox = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			e.fileHits.Add(1)
			if e.fileHandler != nil {
				e.fileHandler(w, r)
				return
			}
			_, _ = w.Write([]byte(testFileBody))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing bearer token")
		}
		if r.Method == "GET" {
			e.mu.Lock()
			e.getHeader = r.Header.Clone()
			cmds := e.pending
			e.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"commands": cmds})
			return
		}
		var ack bridgeCommandResult
		_ = json.NewDecoder(r.Body).Decode(&ack)
		e.mu.Lock()
		if ack.Status == "running" {
			e.runningTry.Add(1)
			if e.runningDrop != 0 {
				if e.runningDrop > 0 {
					e.runningDrop--
				}
				e.mu.Unlock()
				dropConnection(w)
				return
			}
		}
		if ack.Status == "running" && e.runningHTTP != 0 {
			code := e.runningHTTP
			e.mu.Unlock()
			w.WriteHeader(code)
			return
		}
		if ack.Status == "done" && e.doneHTTP != 0 {
			e.events = append(e.events, "ack"+strconv.Itoa(e.doneHTTP)+":done")
			code := e.doneHTTP
			e.mu.Unlock()
			w.WriteHeader(code)
			return
		}
		if e.ackFail[ack.Status] > 0 {
			e.ackFail[ack.Status]--
			e.events = append(e.events, "ack500:"+ack.Status)
			e.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		ev := "ack:" + ack.Status
		if ack.ErrorMessage != "" {
			ev += ":" + ack.ErrorMessage
		}
		e.events = append(e.events, ev)
		e.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(e.fox.Close)

	oldURL := webhook.BridgeCommandsURLV2
	webhook.BridgeCommandsURLV2 = e.fox.URL
	t.Cleanup(func() { webhook.BridgeCommandsURLV2 = oldURL })

	configMutex.Lock()
	oldCfg := configStore
	configStore = &config.Config{
		FoxTrack2APIKey: "test-key",
		Printers: []config.Printer{
			{Name: "Voron", MoonrakerURL: "http://127.0.0.1:7125"},
			{Name: "P1S Shelf", Serial: "01P00A000000001", LANCode: "12345678", IP: "192.168.1.9"},
			{Name: "P1S Cloud", Serial: "01P00A000000002", LANCode: "12345678", Connection: config.ConnectionCloud},
		},
	}
	configMutex.Unlock()
	t.Cleanup(func() { configMutex.Lock(); configStore = oldCfg; configMutex.Unlock() })

	oldJobs, oldDir, oldBackoff, oldBambu, oldKlipper, oldRetry := printJobs, printCacheDir, printAckBackoff, printBambuFile, printKlipperFile, printRunningRetryDelay
	printRunningRetryDelay = time.Millisecond
	printJobs = &printJobTracker{seen: map[string]time.Time{}, busy: map[string]bool{}, inUse: map[string]int{}}
	cache := filepath.Join(t.TempDir(), "print-cache")
	printCacheDir = func() string { return cache }
	printAckBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	printKlipperFile = func(ctx context.Context, name, localPath, remote string) error {
		e.log("upload:" + name + ":" + remote)
		if e.uploadGate != nil {
			e.uploadSeen <- struct{}{}
			<-e.uploadGate
		}
		return nil
	}
	printBambuFile = func(ctx context.Context, p mqttpkg.Printer, localPath string, o mqttpkg.ProjectPrintOptions) error {
		e.log("bambu:" + p.Serial + ":" + o.TaskName)
		return nil
	}
	t.Cleanup(func() {
		printJobs, printCacheDir, printAckBackoff, printBambuFile, printKlipperFile = oldJobs, oldDir, oldBackoff, oldBambu, oldKlipper
		printRunningRetryDelay = oldRetry
	})
	return e
}

func (e *printEnv) command(id, external, format string, mutate func(args map[string]interface{})) bridgeCommand {
	sum := sha256.Sum256([]byte(testFileBody))
	args := map[string]interface{}{
		"download_url": e.fox.URL + "/file",
		"sha256":       hex.EncodeToString(sum[:]),
		"size_bytes":   float64(len(testFileBody)),
		"format":       format,
		"file_name":    "vase.gcode",
		"plate":        float64(1),
	}
	if format == "gcode_3mf" {
		args["file_name"] = "vase.gcode.3mf"
	}
	if mutate != nil {
		mutate(args)
	}
	return bridgeCommand{ID: id, ExternalID: external, Command: "print_file", Args: args}
}

// poll does what one pass of pollBridgeCommands does for print_file commands,
// then waits for the jobs it started.
func (e *printEnv) poll(cmds ...bridgeCommand) {
	e.t.Helper()
	e.mu.Lock()
	e.pending = cmds
	e.mu.Unlock()
	reply, err := fetchBridgeCommands("test-key")
	if err != nil {
		e.t.Fatalf("fetch: %v", err)
	}
	for _, c := range reply.Commands {
		startPrintFile("test-key", c)
	}
	printJobs.wg.Wait()
}

func TestPrintFile_FetchHeadersAndRunningAckBeforeJob(t *testing.T) {
	e := newPrintEnv(t)
	e.poll(e.command("c1", "Voron", "gcode", nil))

	e.mu.Lock()
	caps, ver := e.getHeader.Get("X-Bridge-Capabilities"), e.getHeader.Get("X-Bridge-Version")
	e.mu.Unlock()
	if caps != "print_file" || ver == "" {
		t.Fatalf("headers: capabilities %q version %q", caps, ver)
	}
	got := strings.Join(e.snapshot(), " | ")
	if got != "ack:running | upload:Voron:vase.gcode | ack:done" {
		t.Fatalf("events = %s", got)
	}
}

func TestPrintFile_SameCommandTwiceRunsOnce(t *testing.T) {
	e := newPrintEnv(t)
	cmd := e.command("c1", "Voron", "gcode", nil)
	e.poll(cmd)
	e.poll(cmd)
	if e.count("upload:") != 1 || e.count("ack:running") != 1 || e.count("ack:done") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_UnknownPrinterIsLeftAlone(t *testing.T) {
	e := newPrintEnv(t)
	e.poll(e.command("c1", "Someone Elses Printer", "gcode", nil))
	if len(e.snapshot()) != 0 {
		t.Fatalf("expected no ack, got %v", e.snapshot())
	}
}

func TestPrintFile_Running404SkipsTheJob(t *testing.T) {
	e := newPrintEnv(t)
	e.runningHTTP = http.StatusNotFound
	cmd := e.command("c1", "Voron", "gcode", nil)
	e.poll(cmd)
	e.poll(cmd) // seen: no second ack either
	if e.count("upload:") != 0 || len(e.snapshot()) != 0 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_RunningAckFailureRetriesNextPoll(t *testing.T) {
	e := newPrintEnv(t)
	e.runningHTTP = http.StatusInternalServerError
	cmd := e.command("c1", "Voron", "gcode", nil)
	e.poll(cmd)
	if e.count("upload:") != 0 {
		t.Fatalf("job ran without an accepted ack: %v", e.snapshot())
	}
	e.runningHTTP = 0
	e.poll(cmd)
	if e.count("upload:") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_HashMismatchFails(t *testing.T) {
	e := newPrintEnv(t)
	e.poll(e.command("c1", "Voron", "gcode", func(a map[string]interface{}) {
		a["sha256"] = strings.Repeat("ab", 32)
	}))
	want := "ack:failed:" + errPrintMismatch.Error()
	if e.count(want) != 1 || e.count("upload:") != 0 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_SizeMismatchFails(t *testing.T) {
	e := newPrintEnv(t)
	e.poll(e.command("c1", "Voron", "gcode", func(a map[string]interface{}) {
		a["size_bytes"] = float64(len(testFileBody) - 3)
	}))
	if e.count("ack:failed:"+errPrintMismatch.Error()) != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_FormatMustMatchPrinterKind(t *testing.T) {
	cases := []struct{ printer, serialOrName, format, want string }{
		{"klipper", "Voron", "gcode_3mf", "This file is for a Bambu Lab printer, but Voron is a Klipper printer."},
		{"bambu", "01P00A000000001", "gcode", "This file is for a Klipper printer, but P1S Shelf is a Bambu Lab printer."},
		{"cloud", "01P00A000000002", "gcode_3mf", errPrintCloud.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.printer, func(t *testing.T) {
			e := newPrintEnv(t)
			e.poll(e.command("c1", tc.serialOrName, tc.format, nil))
			if e.count("ack:failed:"+tc.want) != 1 || e.fileHits.Load() != 0 {
				t.Fatalf("events = %v (downloads %d)", e.snapshot(), e.fileHits.Load())
			}
		})
	}
}

func TestPrintFile_BadArgsFail(t *testing.T) {
	cases := map[string]func(a map[string]interface{}){
		"plain http elsewhere": func(a map[string]interface{}) { a["download_url"] = "http://example.com/f" },
		"no url":               func(a map[string]interface{}) { delete(a, "download_url") },
		"bad sha":              func(a map[string]interface{}) { a["sha256"] = "xyz" },
		"size as string":       func(a map[string]interface{}) { a["size_bytes"] = "12" },
		"unknown format":       func(a map[string]interface{}) { a["format"] = "stl" },
		"timelapse not bool":   func(a map[string]interface{}) { a["timelapse"] = "yes" },
		"mapping not numbers":  func(a map[string]interface{}) { a["ams_mapping"] = []interface{}{"a"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPrintEnv(t)
			e.poll(e.command("c1", "Voron", "gcode", mutate))
			if e.count("ack:failed:"+errPrintArgs.Error()) != 1 || e.fileHits.Load() != 0 {
				t.Fatalf("events = %v", e.snapshot())
			}
		})
	}
}

func TestPrintFile_CachedFileIsNotDownloadedAgain(t *testing.T) {
	e := newPrintEnv(t)
	e.poll(e.command("c1", "Voron", "gcode", nil))
	e.poll(e.command("c2", "Voron", "gcode", nil))
	if e.fileHits.Load() != 1 || e.count("upload:") != 2 || e.count("ack:done") != 2 {
		t.Fatalf("downloads %d, events = %v", e.fileHits.Load(), e.snapshot())
	}
}

func TestPrintFile_FinalAckIsRetried(t *testing.T) {
	e := newPrintEnv(t)
	e.ackFail["done"] = 1
	e.poll(e.command("c1", "Voron", "gcode", nil))
	if e.count("ack500:done") != 1 || e.count("ack:done") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_SecondJobOnSamePrinterIsRefused(t *testing.T) {
	e := newPrintEnv(t)
	e.uploadGate = make(chan struct{})
	e.uploadSeen = make(chan struct{}, 1)
	e.mu.Lock()
	e.pending = []bridgeCommand{e.command("c1", "Voron", "gcode", nil)}
	e.mu.Unlock()
	startPrintFile("test-key", e.pending[0])
	<-e.uploadSeen // first job is mid-upload

	startPrintFile("test-key", e.command("c2", "Voron", "gcode", nil))
	deadline := time.Now().Add(5 * time.Second)
	for e.count("ack:failed:"+errPrintBusy.Error()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.count("ack:failed:"+errPrintBusy.Error()) != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
	close(e.uploadGate)
	printJobs.wg.Wait()
	if e.count("ack:done") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_BambuGetsOptions(t *testing.T) {
	e := newPrintEnv(t)
	var got mqttpkg.ProjectPrintOptions
	printBambuFile = func(ctx context.Context, p mqttpkg.Printer, localPath string, o mqttpkg.ProjectPrintOptions) error {
		got = o
		if p.Serial != "01P00A000000001" || p.LANCode != "12345678" || p.FoxTrack2APIKey != "test-key" {
			t.Errorf("printer = %+v", p)
		}
		return nil
	}
	e.poll(e.command("c1", "01P00A000000001", "gcode_3mf", func(a map[string]interface{}) {
		a["plate"] = float64(2)
		a["use_ams"] = true
		a["ams_mapping"] = []interface{}{float64(0), float64(-1), float64(254)}
		a["bed_leveling"] = false
		a["timelapse"] = true
	}))
	if got.Plate != 2 || got.UseAMS == nil || !*got.UseAMS || len(got.AMSMapping) != 3 || got.AMSMapping[2] != 254 ||
		got.BedLeveling || !got.Timelapse || got.TaskName != "vase" {
		t.Fatalf("options = %+v", got)
	}
	if e.count("ack:done") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_LongErrorIsCutTo500Chars(t *testing.T) {
	e := newPrintEnv(t)
	long := strings.Repeat("x", 800)
	printKlipperFile = func(context.Context, string, string, string) error { return &plainError{long} }
	e.poll(e.command("c1", "Voron", "gcode", nil))
	for _, ev := range e.snapshot() {
		if strings.HasPrefix(ev, "ack:failed:") && len([]rune(strings.TrimPrefix(ev, "ack:failed:"))) != maxErrorChars {
			t.Fatalf("message not cut to %d chars: %d", maxErrorChars, len(ev))
		}
	}
	if e.count("ack:failed:") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

type plainError struct{ s string }

func (e *plainError) Error() string { return e.s }

func TestPruneCache_KeepsNewestAndTheFileInUse(t *testing.T) {
	dir := t.TempDir()
	var keep string
	for i := 0; i < cacheMaxFiles+3; i++ {
		p := filepath.Join(dir, strings.Repeat("a", 10)+string(rune('a'+i))+".gcode")
		writeFileAt(t, p, "x", time.Now().Add(-time.Duration(100-i)*time.Minute))
		if i == 0 {
			keep = p // oldest, but in use
		}
	}
	pruneCache(dir, keep)
	entries := dirNames(t, dir)
	if len(entries) != cacheMaxFiles {
		t.Fatalf("kept %d files, want %d", len(entries), cacheMaxFiles)
	}
	found := false
	for _, n := range entries {
		if filepath.Join(dir, n) == keep {
			found = true
		}
	}
	if !found {
		t.Fatalf("the file in use was removed")
	}
}

func writeFileAt(t *testing.T, path, body string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// --- review fixes ---------------------------------------------------------

func TestPrintFile_FinalAckBackoffCoversAboutThreeMinutes(t *testing.T) {
	// The production schedule, read before any test env replaces it.
	var total time.Duration
	for _, d := range printAckBackoff {
		total += d
	}
	if total < 170*time.Second || total > 200*time.Second {
		t.Fatalf("final ack waits add up to %s, want about 3 minutes", total)
	}
	for i := 1; i < len(printAckBackoff); i++ {
		if printAckBackoff[i] < printAckBackoff[i-1] {
			t.Fatalf("backoff shrinks at %d: %v", i, printAckBackoff)
		}
	}
}

func TestPrintFile_FinalAckKeepsRetryingUntilItLands(t *testing.T) {
	e := newPrintEnv(t)
	printAckBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	e.ackFail["done"] = 5 // more than the old 3 tries
	e.poll(e.command("c1", "Voron", "gcode", nil))
	if e.count("ack500:done") != 5 || e.count("ack:done") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_FinalAckGivesUpAfterTheLastBackoff(t *testing.T) {
	e := newPrintEnv(t)
	e.ackFail["done"] = 100
	e.poll(e.command("c1", "Voron", "gcode", nil))
	if e.count("ack500:done") != len(printAckBackoff)+1 || e.count("ack:done") != 0 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_FinalAckStopsAtOnceOn404(t *testing.T) {
	e := newPrintEnv(t)
	e.doneHTTP = http.StatusNotFound
	e.poll(e.command("c1", "Voron", "gcode", nil))
	if e.count("ack404:done") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_TwoJobsForTheSameFileDownloadOnce(t *testing.T) {
	e := newPrintEnv(t)
	e.fileHandler = func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // long enough for the second job to arrive
		_, _ = w.Write([]byte(testFileBody))
	}
	a, err := parsePrintFileArgs(e.command("c1", "Voron", "gcode", nil).Args)
	if err != nil {
		t.Fatal(err)
	}
	dir := printCacheDir()
	path := filepath.Join(dir, a.sha256+cacheExt(a.format))
	logf := func(string, ...interface{}) {}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- ensureCachedFile(context.Background(), logf, dir, path, a) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
	}
	if e.fileHits.Load() != 1 {
		t.Fatalf("downloads = %d, want 1", e.fileHits.Load())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != testFileBody {
		t.Fatalf("cache file = %q, %v", got, err)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("cache folder holds %v", names)
	}
}

func TestPrintFile_FailedDownloadNeverShowsTheSignedLink(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"connection cut": func(w http.ResponseWriter, r *http.Request) { dropConnection(w) },
		"server error":   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"cut mid-body": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(testFileBody)))
			_, _ = w.Write([]byte(testFileBody[:5]))
			dropConnection(w)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPrintEnv(t)
			e.fileHandler = h
			e.poll(e.command("c1", "Voron", "gcode", func(a map[string]interface{}) {
				a["download_url"] = e.fox.URL + "/file?token=SECRET123"
			}))
			var msg string
			for _, ev := range e.snapshot() {
				if strings.HasPrefix(ev, "ack:failed:") {
					msg = strings.TrimPrefix(ev, "ack:failed:")
				}
			}
			if msg == "" {
				t.Fatalf("no failed ack: %v", e.snapshot())
			}
			for _, bad := range []string{"token=", "SECRET123", "http", "127.0.0.1"} {
				if strings.Contains(msg, bad) {
					t.Fatalf("failure message %q contains %q", msg, bad)
				}
			}
		})
	}
}

func TestPrintFile_DownloadLinkOnAnotherHostIsRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("another host was contacted")
	}))
	defer other.Close()
	e := newPrintEnv(t)
	e.poll(e.command("c1", "Voron", "gcode", func(a map[string]interface{}) {
		a["download_url"] = other.URL + "/file?token=x"
	}))
	if e.count("ack:failed:"+errPrintHost.Error()) != 1 || e.count("upload:") != 0 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestSameFoxTrackHost(t *testing.T) {
	old := webhook.BridgeCommandsURLV2
	webhook.BridgeCommandsURLV2 = "https://abc.supabase.co/functions/v1/bridge-commands"
	defer func() { webhook.BridgeCommandsURLV2 = old }()
	for url, want := range map[string]bool{
		"https://abc.supabase.co/storage/v1/object/sign/x?token=t": true,
		"https://ABC.supabase.co/x":                                true,
		"http://abc.supabase.co/x":                                 false, // scheme differs
		"https://abc.supabase.co.evil.com/x":                       false,
		"https://evil.com/abc.supabase.co":                         false,
		"https://abc.supabase.co:8443/x":                           false,
		"":                                                         false,
	} {
		if got := sameFoxTrackHost(url); got != want {
			t.Errorf("sameFoxTrackHost(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestPrintFile_RedirectIsNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(testFileBody))
	}))
	defer other.Close()
	e := newPrintEnv(t)
	e.fileHandler = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/file?token=stolen", http.StatusFound)
	}
	e.poll(e.command("c1", "Voron", "gcode", nil))
	if elsewhere.Load() != 0 || e.count("upload:") != 0 {
		t.Fatalf("the redirect was followed: %d hits, events %v", elsewhere.Load(), e.snapshot())
	}
	if e.count("ack:failed:FoxTrack would not give Bridge the file (HTTP 302)") != 1 {
		t.Fatalf("events = %v", e.snapshot())
	}
}

func TestPrintFile_RunningAckNoAnswerRetriesThenRunsAnyway(t *testing.T) {
	e := newPrintEnv(t)
	e.runningDrop = -1
	cmd := e.command("c1", "Voron", "gcode", nil)
	e.poll(cmd)
	if e.runningTry.Load() != 3 || e.count("upload:") != 1 || e.count("ack:done") != 1 {
		t.Fatalf("running tries %d, events = %v", e.runningTry.Load(), e.snapshot())
	}
	e.poll(cmd) // marked seen: not run again
	if e.count("upload:") != 1 {
		t.Fatalf("job ran twice: %v", e.snapshot())
	}
}

func TestPrintFile_RunningAckSecondTryLands(t *testing.T) {
	e := newPrintEnv(t)
	e.runningDrop = 1
	e.poll(e.command("c1", "Voron", "gcode", nil))
	if e.runningTry.Load() != 2 || e.count("ack:running") != 1 || e.count("upload:") != 1 {
		t.Fatalf("running tries %d, events = %v", e.runningTry.Load(), e.snapshot())
	}
}

func TestPrintFile_RunningAck404AfterNoAnswerSkips(t *testing.T) {
	e := newPrintEnv(t)
	e.runningDrop = 1
	e.runningHTTP = http.StatusNotFound
	e.poll(e.command("c1", "Voron", "gcode", nil))
	if e.runningTry.Load() != 2 || e.count("upload:") != 0 || len(e.snapshot()) != 0 {
		t.Fatalf("running tries %d, events = %v", e.runningTry.Load(), e.snapshot())
	}
}

func TestPrintFile_RunningAckHTTPErrorIsNotRetriedQuickly(t *testing.T) {
	e := newPrintEnv(t)
	e.runningHTTP = http.StatusInternalServerError
	e.poll(e.command("c1", "Voron", "gcode", nil))
	if e.runningTry.Load() != 1 || e.count("upload:") != 0 {
		t.Fatalf("running tries %d, events = %v", e.runningTry.Load(), e.snapshot())
	}
}
