package main

// print_file: FoxTrack asks Bridge to fetch a sliced file from a signed URL,
// check it, put it on the printer and start it. The poll loop only accepts the
// command (dedupe, match the printer, ack "running"); the job itself runs in a
// goroutine so pause and stop keep working during a long transfer. Every error
// returned by the job is shown to the shop owner as is, so each one is plain
// English.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
	"foxtrack-bridge/webhook"
)

const (
	maxPrintFileBytes = 60 << 20
	cacheMaxFiles     = 20
	cacheMaxBytes     = 1 << 30
	seenCommandTTL    = time.Hour
	maxErrorChars     = 500
)

// printJobTimeout: FoxTrack fails a running command after 15 minutes for a
// small file; 12 leaves room for the result ack to land first. A big file gets
// more (printJobTimeoutFor).
var printJobTimeout = 12 * time.Minute

// printJobTimeoutFor mirrors print_file_transfer_limit in FoxTrack (migration
// 20261031000129; change both): FoxTrack allows 15 minutes, or 10 minutes plus
// the file at 20 KB/s when that is longer (a slow printer Wi-Fi measured
// 27 KB/s). Bridge stops 3 minutes before FoxTrack does.
func printJobTimeoutFor(args map[string]interface{}) time.Duration {
	size, ok := intArg(args["size_bytes"])
	if !ok || size <= 0 || size > maxPrintFileBytes {
		return printJobTimeout
	}
	extra := 10*time.Minute + time.Duration(size)*time.Second/20480 - 15*time.Minute
	if extra < 0 {
		extra = 0
	}
	return printJobTimeout + extra
}

var (
	errPrintArgs        = errors.New("FoxTrack sent an incomplete print command. Update FoxTrack Bridge, then try again.")
	errPrintBusy        = errors.New("Bridge is already sending a file to this printer.")
	errPrintCloud       = errors.New("Printing a file needs the printer in LAN Only Mode with Developer Mode on. It does not work over Bambu Cloud.")
	errPrintMismatch    = errors.New("The file got damaged on the way to this computer. Try again.")
	errPrintTooLong     = errors.New("Sending the file took too long. Check the printer's network connection, then try again.")
	errPrintHost        = errors.New("Bridge could not check the file link. Try again, and contact FoxTrack support if it keeps happening.")
	errPrintSave        = errors.New("Bridge could not save the file on this computer. Free up some disk space, then try again.")
	errPrintDownload    = errors.New("Bridge could not download the file from FoxTrack. Check this computer's internet connection, then try again.")
	errPrintInterrupted = errors.New("The download from FoxTrack was interrupted. Try again.")
)

// Seams for tests.
var (
	// The signed link must be fetched as given: a redirect could carry the
	// token to another host, so none is followed (a 3xx then fails the job).
	printFileHTTPClient = &http.Client{
		Timeout:       10 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// Waits between the tries of the final done/failed ack: about 3 minutes in
	// all, so a FoxTrack outage of a minute or two does not lose the result.
	printAckBackoff = []time.Duration{
		2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second,
		30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	printCacheDir    = func() string { return filepath.Join(config.ConfigDir(), "print-cache") }
	printBambuFile   = mqttpkg.PrintProjectFile
	printKlipperFile = func(ctx context.Context, name, localPath, remoteName string) error {
		return lanCtrl.UploadAndPrint(ctx, name, localPath, remoteName)
	}
	// printIdleCheck runs before the download, so a busy printer never costs
	// a 50 MB fetch. The senders check again before they upload.
	printIdleCheck = func(p config.Printer, isBambu bool, mq mqttpkg.Printer) error {
		if isBambu {
			return mqttpkg.PrintPreflight(mq)
		}
		return lanCtrl.PrintReady(p.Name)
	}
)

// printJobTracker is Bridge's memory of print_file work: command ids seen
// (FoxTrack re-sends a pending command on every poll), printers with a job in
// flight, and cache files a running job is using.
type printJobTracker struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	busy  map[string]bool
	inUse map[string]int
	wg    sync.WaitGroup // tests wait on it

	locksMu   sync.Mutex
	fileLocks map[string]*sync.Mutex
}

var printJobs = &printJobTracker{seen: map[string]time.Time{}, busy: map[string]bool{}, inUse: map[string]int{}}

func (t *printJobTracker) pruneSeen(now time.Time) {
	for id, at := range t.seen {
		if now.Sub(at) > seenCommandTTL {
			delete(t.seen, id)
		}
	}
}

func (t *printJobTracker) hasSeen(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneSeen(time.Now())
	_, ok := t.seen[id]
	return ok
}

func (t *printJobTracker) markSeen(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneSeen(time.Now())
	t.seen[id] = time.Now()
}

func (t *printJobTracker) lockPrinter(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.busy[name] {
		return false
	}
	t.busy[name] = true
	return true
}

func (t *printJobTracker) unlockPrinter(name string) {
	t.mu.Lock()
	delete(t.busy, name)
	t.mu.Unlock()
}

func (t *printJobTracker) useFile(path string, delta int) {
	t.mu.Lock()
	t.inUse[path] += delta
	if t.inUse[path] <= 0 {
		delete(t.inUse, path)
	}
	t.mu.Unlock()
}

// fileLock returns the mutex for one cache path. Two jobs for the same file
// take turns: the second finds the first one's verified copy instead of
// downloading and renaming over it. The map holds one small mutex per distinct
// file seen since start-up (ponytail: never pruned, a few bytes each).
func (t *printJobTracker) fileLock(path string) *sync.Mutex {
	t.locksMu.Lock()
	defer t.locksMu.Unlock()
	if t.fileLocks == nil {
		t.fileLocks = map[string]*sync.Mutex{}
	}
	m := t.fileLocks[path]
	if m == nil {
		m = &sync.Mutex{}
		t.fileLocks[path] = m
	}
	return m
}

func (t *printJobTracker) fileInUse(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inUse[path] > 0
}

// findBridgePrinter matches external_id the way executeBridgeCommand does:
// Bambu by serial, Klipper by name.
func findBridgePrinter(externalID string) (p config.Printer, isBambu bool, mq mqttpkg.Printer, ok bool) {
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore == nil {
		return
	}
	for _, cp := range configStore.Printers {
		bambu := isBambuPrinterConfig(cp)
		if (bambu && cp.Serial == externalID) || (!bambu && cp.Name == externalID) {
			return cp, bambu, mqttPrinter(cp, configStore), true
		}
	}
	return
}

// startPrintFile runs on the poll loop. It must stay quick: dedupe, match,
// ack "running", start the goroutine.
func startPrintFile(apiKey string, cmd bridgeCommand) {
	if printJobs.hasSeen(cmd.ID) {
		return
	}
	p, isBambu, mq, ok := findBridgePrinter(cmd.ExternalID)
	if !ok {
		return // maybe another Bridge's printer: leave it pending
	}
	// One try: this runs on the poll loop, and every retry would hold up
	// pause and stop for the other printers. An HTTP error is retried by the
	// next poll.
	err := ackBridgeCommand(apiKey, bridgeCommandResult{CommandID: cmd.ID, Status: "running"})
	var statusErr *httpStatusError
	switch {
	case err == nil:
	case errors.As(err, &statusErr) && statusErr.code == http.StatusNotFound:
		printJobs.markSeen(cmd.ID)
		log.Printf("[bridge-commands] print_file %s %s: skipped, FoxTrack says another Bridge took it or it expired", cmd.ID, p.Name)
		return
	case errors.As(err, &statusErr):
		log.Printf("[bridge-commands] print_file %s %s: could not accept: %v", cmd.ID, p.Name, err)
		return // not marked seen: the next poll tries again
	default:
		// No answer at all. FoxTrack may have applied the ack,
		// and then never hands the command out again, so the job would be lost
		// (FoxTrack fails it as timed out after 15 minutes or more). Run it: the final
		// done/failed ack is accepted from pending or running. The price: a
		// workspace whose printer has no owning Bridge recorded could see
		// another Bridge take the same command and run it twice.
		log.Printf("[bridge-commands] print_file %s %s: no answer to the running ack (%v), running the job anyway", cmd.ID, p.Name, err)
	}
	printJobs.markSeen(cmd.ID)
	log.Printf("[bridge-commands] print_file %s %s: accepted", cmd.ID, p.Name)

	printJobs.wg.Add(1)
	go func() {
		defer printJobs.wg.Done()
		finishPrintFile(apiKey, cmd, p, isBambu, mq)
	}()
}

// finishPrintFile runs the job and reports the result to FoxTrack.
func finishPrintFile(apiKey string, cmd bridgeCommand, p config.Printer, isBambu bool, mq mqttpkg.Printer) {
	logf := func(format string, a ...interface{}) {
		log.Printf("[bridge-commands] print_file %s %s: %s", cmd.ID, p.Name, fmt.Sprintf(format, a...))
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				logf("panic: %v", r)
				err = errors.New("Bridge hit an unexpected error while sending the file. Try again.")
			}
		}()
		if !printJobs.lockPrinter(p.Name) {
			err = errPrintBusy
			return
		}
		defer printJobs.unlockPrinter(p.Name)
		ctx, cancel := context.WithTimeout(context.Background(), printJobTimeoutFor(cmd.Args))
		defer cancel()
		err = runPrintJob(ctx, logf, cmd.Args, p, isBambu, mq)
		if errors.Is(err, context.DeadlineExceeded) {
			logf("timed out: %v", err)
			err = errPrintTooLong
		}
	}()

	result := bridgeCommandResult{CommandID: cmd.ID, Status: "done"}
	if err != nil {
		result.Status = "failed"
		result.ErrorMessage = truncateChars(err.Error(), maxErrorChars)
		logf("failed: %v", err)
	} else {
		logf("done, the printer has started the file")
	}
	for attempt := 0; ; attempt++ {
		ackErr := ackBridgeCommand(apiKey, result)
		if ackErr == nil {
			return
		}
		logf("could not report %s to FoxTrack (try %d of %d): %v", result.Status, attempt+1, len(printAckBackoff)+1, ackErr)
		var statusErr *httpStatusError
		if attempt >= len(printAckBackoff) || (errors.As(ackErr, &statusErr) && statusErr.code == http.StatusNotFound) {
			return
		}
		time.Sleep(printAckBackoff[attempt])
	}
}

func truncateChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

// --- args -----------------------------------------------------------------

type printFileArgs struct {
	downloadURL string
	sha256      string
	size        int64
	format      string // "gcode" | "gcode_3mf"
	fileName    string
	plate       int
	useAMS      *bool
	amsMapping  []int
	bedLeveling bool
	timelapse   bool
}

func intArg(v interface{}) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1e9 {
		return 0, false
	}
	return int(f), true
}

func parsePrintFileArgs(args map[string]interface{}) (printFileArgs, error) {
	// plate 0 = the only plate in the file (a file with several is refused).
	a := printFileArgs{bedLeveling: true}
	str := func(k string) string { s, _ := args[k].(string); return strings.TrimSpace(s) }
	optBool := func(k string, dst *bool) bool {
		v, present := args[k]
		if !present || v == nil {
			return true
		}
		b, ok := v.(bool)
		if ok {
			*dst = b
		}
		return ok
	}

	a.downloadURL = str("download_url")
	a.sha256 = strings.ToLower(str("sha256"))
	a.format = str("format")
	if i := strings.LastIndexAny(str("file_name"), `/\`); i >= 0 {
		a.fileName = str("file_name")[i+1:]
	} else {
		a.fileName = str("file_name")
	}
	if a.downloadURL == "" || a.fileName == "" || !webhook.URLAllowed(a.downloadURL) {
		return a, errPrintArgs
	}
	if b, err := hex.DecodeString(a.sha256); err != nil || len(b) != sha256.Size {
		return a, errPrintArgs
	}
	if a.format != "gcode" && a.format != "gcode_3mf" {
		return a, errPrintArgs
	}
	size, ok := intArg(args["size_bytes"])
	if !ok || size <= 0 {
		return a, errPrintArgs
	}
	if size > maxPrintFileBytes {
		return a, errors.New("That file is larger than the 60 MB Bridge can send. Start it from the printer or its own web page instead.")
	}
	a.size = int64(size)
	if v, present := args["plate"]; present && v != nil {
		if a.plate, ok = intArg(v); !ok || a.plate < 1 {
			return a, errPrintArgs
		}
	}
	var useAMS bool
	if v, present := args["use_ams"]; present && v != nil {
		if !optBool("use_ams", &useAMS) {
			return a, errPrintArgs
		}
		a.useAMS = &useAMS
	}
	if v, present := args["ams_mapping"]; present && v != nil {
		list, isList := v.([]interface{})
		if !isList || len(list) > 32 {
			return a, errPrintArgs
		}
		for _, e := range list {
			n, ok := intArg(e)
			if !ok || n < -1 || n > 254 {
				return a, errPrintArgs
			}
			a.amsMapping = append(a.amsMapping, n)
		}
	}
	if !optBool("bed_leveling", &a.bedLeveling) || !optBool("timelapse", &a.timelapse) {
		return a, errPrintArgs
	}
	return a, nil
}

// --- the job --------------------------------------------------------------

func runPrintJob(ctx context.Context, logf func(string, ...interface{}), args map[string]interface{}, p config.Printer, isBambu bool, mq mqttpkg.Printer) error {
	a, err := parsePrintFileArgs(args)
	if err != nil {
		return err
	}
	if !sameFoxTrackHost(a.downloadURL) {
		logf("refused: the download link is not on the FoxTrack host Bridge talks to")
		return errPrintHost
	}
	switch {
	case a.format == "gcode_3mf" && !isBambu:
		return fmt.Errorf("This file is for a Bambu Lab printer, but %s is a Klipper printer.", p.Name)
	case a.format == "gcode" && isBambu:
		return fmt.Errorf("This file is for a Klipper printer, but %s is a Bambu Lab printer.", p.Name)
	case isBambu && p.IsCloud():
		return errPrintCloud
	}
	if err := printIdleCheck(p, isBambu, mq); err != nil {
		logf("refused before the download: %v", err)
		return err
	}

	dir := printCacheDir()
	path := filepath.Join(dir, a.sha256+cacheExt(a.format))
	printJobs.useFile(path, 1)
	defer func() {
		pruneCache(dir, path)
		printJobs.useFile(path, -1)
	}()
	if err := ensureCachedFile(ctx, logf, dir, path, a); err != nil {
		return err
	}

	logf("sending %s (%d bytes) to the printer", a.fileName, a.size)
	if isBambu {
		return printBambuFile(ctx, mq, path, mqttpkg.ProjectPrintOptions{
			Plate:       a.plate,
			UseAMS:      a.useAMS,
			AMSMapping:  a.amsMapping,
			BedLeveling: a.bedLeveling,
			Timelapse:   a.timelapse,
			TaskName:    taskName(a.fileName),
		})
	}
	return printKlipperFile(ctx, p.Name, path, a.fileName)
}

// sameFoxTrackHost reports whether the download link has the scheme and host
// of the FoxTrack address Bridge polls for commands (so the
// FOXTRACK_SUPABASE_URL override moves both).
func sameFoxTrackHost(downloadURL string) bool {
	d, err1 := url.Parse(downloadURL)
	b, err2 := url.Parse(webhook.BridgeCommandsURLV2)
	return err1 == nil && err2 == nil && d.Host != "" &&
		strings.EqualFold(d.Scheme, b.Scheme) && strings.EqualFold(d.Host, b.Host)
}

func cacheExt(format string) string {
	if format == "gcode_3mf" {
		return ".gcode.3mf"
	}
	return ".gcode"
}

func taskName(fileName string) string {
	lower := strings.ToLower(fileName)
	for _, ext := range []string{".gcode.3mf", ".3mf", ".gcode"} {
		if strings.HasSuffix(lower, ext) {
			return fileName[:len(fileName)-len(ext)]
		}
	}
	return fileName
}

// ensureCachedFile leaves a verified copy of the file at path: reused when the
// cache already holds it, downloaded otherwise.
func ensureCachedFile(ctx context.Context, logf func(string, ...interface{}), dir, path string, a printFileArgs) error {
	// One job at a time per file. A second job for the same file waits here
	// (at most as long as the first one's download) and then reuses its copy.
	lock := printJobs.fileLock(path)
	lock.Lock()
	defer lock.Unlock()

	if st, err := os.Stat(path); err == nil && st.Size() == a.size {
		if sum, err := hashFile(path); err == nil && sum == a.sha256 {
			now := time.Now()
			_ = os.Chtimes(path, now, now) // newest in the prune order
			logf("using the cached copy, no download needed")
			return nil
		}
	}
	_ = os.Remove(path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		logf("create the print cache folder: %v", err)
		return errPrintSave
	}
	logf("downloading %d bytes from FoxTrack", a.size)
	req, err := http.NewRequestWithContext(ctx, "GET", a.downloadURL, nil)
	if err != nil {
		return errPrintArgs
	}
	resp, err := printFileHTTPClient.Do(req)
	if err != nil {
		// The error text of a failed request holds the whole signed link,
		// token included, and this job's error is shown to every member in
		// FoxTrack. Log only the cause, never the link.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		logf("download failed: %v", err)
		return errPrintDownload
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logf("download refused: HTTP %d", resp.StatusCode)
		return downloadStatusError(resp.StatusCode)
	}

	tmp, err := createTempFile(dir, "dl-*.tmp")
	if err != nil {
		logf("create the download file: %v", err)
		return errPrintSave
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	h := sha256.New()
	w := &recordErrWriter{w: tmp}
	n, copyErr := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, a.size+1))
	// A failed write, Sync or Close is this computer's (disk full), not the network's.
	localErr := w.err
	if syncErr := tmp.Sync(); localErr == nil {
		localErr = syncErr
	}
	if closeErr := tmp.Close(); localErr == nil {
		localErr = closeErr
	}
	if localErr != nil {
		logf("write the download file: %v", localErr)
		return errPrintSave
	}
	if copyErr != nil {
		logf("download interrupted: %v", copyErr)
		return errPrintInterrupted
	}
	if n != a.size || hex.EncodeToString(h.Sum(nil)) != a.sha256 {
		logf("downloaded file does not match: %d bytes, expected %d", n, a.size)
		return errPrintMismatch
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, path); err != nil {
		logf("move the download into the cache: %v", err)
		return errPrintSave
	}
	logf("download verified")
	return nil
}

// downloadStatusError words a refused download for FoxTrack's user. The signed
// link is good for a short time, so most refusals mean it ran out.
func downloadStatusError(status int) error {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return errors.New("The download link from FoxTrack expired. Press Print again in FoxTrack.")
	case http.StatusNotFound:
		return errors.New("FoxTrack no longer has this file. Upload it again in FoxTrack.")
	}
	return errors.New("FoxTrack could not send the file right now. Try again in a minute.")
}

// tempFile is what the download needs of the cache's temp file.
type tempFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

// createTempFile is a test seam.
var createTempFile = func(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) }

// recordErrWriter remembers the first error of the writer it wraps, so a
// failed write can be told apart from a failed read when io.Copy returns it.
type recordErrWriter struct {
	w   io.Writer
	err error
}

func (r *recordErrWriter) Write(p []byte) (int, error) {
	n, err := r.w.Write(p)
	if err != nil && r.err == nil {
		r.err = err
	}
	return n, err
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pruneCache keeps the newest cacheMaxFiles files and cacheMaxBytes bytes,
// oldest removed first. keep and any file another job is using stay.
func pruneCache(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type cached struct {
		path string
		size int64
		mod  time.Time
	}
	var files []cached
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".tmp") {
			if time.Since(info.ModTime()) > time.Hour && !printJobs.fileInUse(path) {
				_ = os.Remove(path) // a crashed download
			}
			continue
		}
		files = append(files, cached{path, info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	count := len(files)
	for _, f := range files {
		if count <= cacheMaxFiles && total <= cacheMaxBytes {
			return
		}
		if f.path == keep || printJobs.fileInUse(f.path) {
			continue
		}
		if os.Remove(f.path) == nil {
			count--
			total -= f.size
		}
	}
}
