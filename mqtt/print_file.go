package mqtt

// Starting a sliced Bambu Studio file (.gcode.3mf) that FoxTrack sent: upload
// it to the printer's storage over FTPS (implicit TLS, port 990), then send
// the MQTT print.project_file command. Needs LAN Only Mode + Developer Mode,
// the same setup pause and stop need. Called by the print_file runner in
// bridge_commands.go (FoxTrack's Print button).
//
// The project_file payload mirrors bambuddy's start_print
// (github.com/maziggy/bambuddy, backend/app/services/bambu_mqtt.py, function
// start_print at line 5890 of main as of 2026-10-06, fetched that day), which
// says it matches a Bambu Studio capture: the field set and values, the flat
// ams_mapping with -1 for the external spool, ams_mapping2 with {255,0} for
// the external spool and {255,255} for an unused filament, use_ams reconciled
// against the mapping, ids below 2^31, bed_leveling spelled with one l,
// vibration_cali off on a P2S, nozzle_offset_cali only on dual nozzle printers.
// Difference on purpose: sequence_id comes from nextSequenceID. md5 is sent
// empty like bambuddy, ha-bambulab and bambulabs_api (firmware takes "" as
// "skip the check"); the 3MF's own plate md5 is only logged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/ftps"
)

// ProjectPrintOptions are what the FoxTrack Print dialog chose.
type ProjectPrintOptions struct {
	// Plate number in the 3MF (Metadata/plate_N.gcode). 0 = find the only one.
	Plate int
	// UseAMS: nil = decide from AMSMapping (no AMS slot chosen: external spool).
	UseAMS *bool
	// One entry per filament of the file (index = slice_info filament id - 1):
	// the AmsSlot.Slot number Bridge reports to FoxTrack, -1 unused, 254 the
	// external spool. nil = no mapping chosen.
	AMSMapping  []int
	BedLeveling bool
	Timelapse   bool
	// Shown on the printer screen (subtask_name): the user's file name without
	// its extension. Empty = "FoxTrack print".
	TaskName string
}

// printerStoredName is the one name every file is stored under on the
// printer's SD card, so sending files over time never fills it up. Safe
// because the printer is idle when we upload (checked before and after) and
// the screen shows TaskName, not this name.
const printerStoredName = "foxtrack-print.gcode.3mf"

var (
	errPrintCloud     = errors.New("Printing a file needs the printer in LAN Only Mode with Developer Mode on. It does not work over Bambu Cloud.")
	errPrintNoClient  = errors.New("Bridge is not connected to this printer right now.")
	errPrintBusy      = errors.New("The printer is busy. Wait until it is idle, then try again.")
	errPrintNoStatus  = errors.New("Bridge has not received the printer's status yet. Wait a moment and try again.")
	errPrintPickSlots = errors.New("Pick the AMS slots in FoxTrack and try again.")
	errPrintMixed     = errors.New("Use AMS slots for every filament, or the external spool for a one-filament file.")
	errPrintNoStart   = errors.New("The printer got the file but did not start. Check the printer screen.")
	errPrintAccess    = errors.New("The printer refused the access code. Check the LAN access code in Bridge.")
	errPrintUpload    = errors.New("Bridge could not send the file to the printer. Check that the printer is on, has an SD card in, LAN Only Mode and Developer Mode are on, and try again.")
)

// Test seams and timings.
var (
	ftpsUpload             = ftps.Upload
	publishProject         = publishProjectFile
	printerClientConnected = clientConnected
	startWait              = 60 * time.Second
	startPoll              = 250 * time.Millisecond
	// A new error code must last this long, with the printer still idle,
	// before it counts as a refusal: some reports carry a passing code one
	// report before gcode_state moves on.
	startErrGrace = 4 * time.Second
	uploadCap     = 15 * time.Minute // per upload attempt
)

// PrintProjectFile uploads localPath to printer p and starts it. The error
// text is shown to the FoxTrack user as is, so it is plain English. It can
// take as long as the upload (up to uploadCap, twice on an A1 that needs the
// clear-text fallback) plus up to 120 s for the printer to confirm it, plus
// startWait (60 s) for the print to begin; ctx cancels it at any point.
func PrintProjectFile(ctx context.Context, p Printer, localPath string, opts ProjectPrintOptions) error {
	logf := func(format string, args ...any) {
		log.Printf("[%s] print_file: "+format, append([]any{p.Name}, args...)...)
	}
	prefix := serialPrefix(p.Serial)

	if err := printPreflight(p); err != nil {
		logf("refused: %v", err)
		return err
	}

	info, err := inspect3MF(localPath, opts.Plate)
	if err != nil {
		logf("3MF check failed: %v", err)
		return err
	}
	logf("file ok: plate %d, %d filaments, plate uses %v, md5 %q", info.Plate, info.FilamentCount, info.PlateFilaments, info.MD5)

	st := GetPrinterState(p.Name)
	flat, ref2, useAMS, err := buildAMSMapping(amsRequest{
		Requested:  opts.AMSMapping,
		Count:      info.FilamentCount,
		Used:       info.PlateFilaments,
		HasAMS:     len(st.AMS) > 0,
		ForceExt:   opts.UseAMS != nil && !*opts.UseAMS,
		Trays:      getTrayRefs(p.Name),
		DualNozzle: dualNozzle(prefix),
	})
	if err != nil {
		logf("AMS mapping refused: %v", err)
		return err
	}
	logf("AMS mapping %v use_ams=%v", flat, useAMS)

	remote := printerStoredName
	logf("uploading as %s to %s", remote, p.IP)

	cfg := ftps.Config{
		Host: p.IP, User: "bblp", Password: p.LANCode,
		Logf: func(format string, args ...any) { logf("ftps: "+format, args...) },
	}
	uploadOnce := func() error {
		c, cancel := context.WithTimeout(ctx, uploadCap)
		defer cancel()
		return ftpsUpload(c, cfg, localPath, remote)
	}
	err = uploadOnce()
	if err != nil && ctx.Err() == nil && (prefix == "030" || prefix == "039") && !errors.Is(err, ftps.ErrLogin) {
		logf("upload failed (%v); A1 family, retrying once with a clear-text data channel", err)
		cfg.ClearData = true
		err = uploadOnce()
	}
	if err != nil {
		logf("upload failed: %v", err)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ftps.ErrLogin):
			return errPrintAccess
		}
		return errPrintUpload
	}
	logf("upload done")

	// The upload can take minutes. The printer may have been started from its
	// screen meanwhile, and a start while busy can cancel a running job.
	if err := printPreflight(p); err != nil {
		logf("refused after upload: %v", err)
		return err
	}

	payload, err := buildProjectFile(projectFileInput{
		SeqID: nextSequenceID(), ID: newJobID(),
		Plate: info.Plate, RemoteName: remote,
		TaskName: firstNonEmpty(opts.TaskName, "FoxTrack print"),
		// ponytail: md5 "" works on P1S/X1C/A1 in three references; send
		// info.MD5 only if a Bambu Studio capture shows the firmware wants it.
		MD5: "", Timelapse: opts.Timelapse, BedLeveling: opts.BedLeveling,
		UseAMS: useAMS, DualNozzle: dualNozzle(prefix), P2S: prefix == "22E",
		AMSMapping: flat, AMSMapping2: ref2,
	})
	if err != nil {
		return fmt.Errorf("build print command: %w", err)
	}

	baseline := lastReportedError(p.Name)
	published := time.Now()
	if err := publishProject(p.Name, p.Serial, payload); err != nil {
		logf("publish failed: %v", err)
		return errPrintNoClient
	}
	logf("project_file sent: %s", payload)

	if err := waitForStart(ctx, p.Name, baseline); err != nil {
		logf("start not confirmed after %s: %v", time.Since(published).Round(time.Second), err)
		return err
	}
	logf("print started after %s", time.Since(published).Round(time.Second))
	return nil
}

// printPreflight is the check done before the upload and again before the
// publish: not cloud, connected, idle.
func printPreflight(p Printer) error {
	if IsCloudSerial(p.Serial) {
		return errPrintCloud
	}
	if !printerClientConnected(p.Name) {
		return errPrintNoClient
	}
	switch GetPrinterState(p.Name).Status {
	case "idle", "finished", "error": // IDLE, FINISH, FAILED
		return nil
	case "", "connected":
		return errPrintNoStatus
	case "disconnected":
		return errPrintNoClient
	}
	return errPrintBusy
}

// waitForStart returns nil as soon as the printer leaves IDLE/FINISH/FAILED
// into a busy state (PREPARE, SLICING, RUNNING, PAUSE), even if an error code
// also showed up on the way: a printer that is starting normally often reports
// one. A new error code only counts while the state is still IDLE/FINISH or is
// FAILED, and only once it has stayed for startErrGrace. errPrintNoStart after
// startWait.
func waitForStart(ctx context.Context, name, baselineErr string) error {
	deadline := time.NewTimer(startWait)
	defer deadline.Stop()
	tick := time.NewTicker(startPoll)
	defer tick.Stop()
	var seenCode string
	var seenAt time.Time
	for {
		switch GetPrinterState(name).Status {
		case "printing", "paused", "PREPARE", "SLICING":
			return nil
		case "idle", "finished", "error":
			// A print error left over from an earlier failure is not news:
			// only a different code counts.
			switch code := lastReportedError(name); {
			case code == "" || code == baselineErr:
				seenCode = ""
			case code != seenCode:
				seenCode, seenAt = code, time.Now()
			case time.Since(seenAt) >= startErrGrace:
				return fmt.Errorf("The printer refused the print (error %s). Check the printer screen.", code)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errPrintNoStart
		case <-tick.C:
		}
	}
}

// ---- printer file names, ids, models ----

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var (
	jobIDMu   sync.Mutex
	lastJobID int64
)

// newJobID returns a decimal id for project_id, subtask_id and task_id:
// non-zero, below 2^31 (P1S firmware clamps larger ones and treats a task_id
// of 0 as a continuation), and unique within this process.
func newJobID() string {
	const limit = 2147483647
	jobIDMu.Lock()
	defer jobIDMu.Unlock()
	id := time.Now().UnixMilli() % limit
	if id <= lastJobID {
		id = lastJobID + 1
	}
	if id <= 0 || id >= limit {
		id = 1
	}
	lastJobID = id
	return strconv.FormatInt(id, 10)
}

var bambuModels = map[string]string{
	"00M": "Bambu Lab X1 Carbon",
	"00W": "Bambu Lab X1",
	"03W": "Bambu Lab X1E",
	"01S": "Bambu Lab P1P",
	"01P": "Bambu Lab P1S",
	"030": "Bambu Lab A1 mini",
	"039": "Bambu Lab A1",
	"094": "Bambu Lab H2D",
	"239": "Bambu Lab H2D Pro",
	"093": "Bambu Lab H2S",
	"22E": "Bambu Lab P2S",
	"31B": "Bambu Lab H2C",
	"20P": "Bambu Lab X2D",
	"26A": "Bambu Lab A2L",
}

// ModelFromSerial names a Bambu Lab printer from its serial number prefix,
// as Bambu Studio does ("Bambu Lab P1S"), or "" when the prefix is unknown.
// The table is the sn_prefix of Bambu Studio's resources/printers/*.json.
func ModelFromSerial(serial string) string {
	return bambuModels[serialPrefix(serial)]
}

func serialPrefix(serial string) string {
	if len(serial) < 3 {
		return ""
	}
	return strings.ToUpper(serial[:3])
}

// dualNozzle: H2D, H2D Pro, H2C, X2D. (H2S shares the H2 firmware but has one
// nozzle.)
func dualNozzle(prefix string) bool {
	switch prefix {
	case "094", "239", "31B", "20P":
		return true
	}
	return false
}

// ---- project_file payload ----

type amsRef struct {
	AmsID  int `json:"ams_id"`
	SlotID int `json:"slot_id"`
}

type projectFileInput struct {
	SeqID, ID, RemoteName, TaskName, MD5 string
	Plate                                int
	Timelapse, BedLeveling, UseAMS       bool
	DualNozzle, P2S                      bool
	AMSMapping                           []int
	AMSMapping2                          []amsRef
}

type projectFileCmd struct {
	SequenceID            string   `json:"sequence_id"`
	Command               string   `json:"command"`
	Param                 string   `json:"param"`
	URL                   string   `json:"url"`
	File                  string   `json:"file"`
	MD5                   string   `json:"md5"`
	BedType               string   `json:"bed_type"`
	Timelapse             bool     `json:"timelapse"`
	BedLeveling           bool     `json:"bed_leveling"`
	AutoBedLeveling       int      `json:"auto_bed_leveling"`
	FlowCali              bool     `json:"flow_cali"`
	VibrationCali         bool     `json:"vibration_cali"`
	LayerInspect          bool     `json:"layer_inspect"`
	UseAMS                bool     `json:"use_ams"`
	ExtrudeCaliFlag       int      `json:"extrude_cali_flag"`
	ExtrudeCaliManualMode int      `json:"extrude_cali_manual_mode"`
	NozzleOffsetCali      int      `json:"nozzle_offset_cali"`
	SubtaskName           string   `json:"subtask_name"`
	ProfileID             string   `json:"profile_id"`
	ProjectID             string   `json:"project_id"`
	SubtaskID             string   `json:"subtask_id"`
	TaskID                string   `json:"task_id"`
	AMSMapping            []int    `json:"ams_mapping,omitempty"`
	AMSMapping2           []amsRef `json:"ams_mapping2,omitempty"`
}

// printerFileURL is the "url" field. Senders disagree: bambuddy sends
// ftp://<name> for every model (used here); ha-bambulab sends
// file:///sdcard/<name> on everything but H2. If a real printer wants the
// other form, change only this function.
func printerFileURL(name string) string { return "ftp://" + name }

// buildProjectFile is the pure payload builder.
func buildProjectFile(in projectFileInput) ([]byte, error) {
	// Calibration: like Bambu Studio's defaults, "auto" (2): the printer runs
	// it only when it was not done recently. An explicit Bed levelling tick
	// forces it (bed_leveling true, 1); an unticked box is "auto", not "off",
	// so a first layer is never ruined by a skipped level.
	bedLevelInt, bedLevel := 2, false
	if in.BedLeveling {
		bedLevelInt, bedLevel = 1, true
	}
	nozzleCali := 0
	if in.DualNozzle {
		nozzleCali = 2
	}
	cmd := projectFileCmd{
		SequenceID: in.SeqID, Command: "project_file",
		Param: fmt.Sprintf("Metadata/plate_%d.gcode", in.Plate),
		URL:   printerFileURL(in.RemoteName), File: in.RemoteName,
		MD5: strings.ToLower(in.MD5), BedType: "auto", Timelapse: in.Timelapse,
		BedLeveling: bedLevel, AutoBedLeveling: bedLevelInt,
		FlowCali: false, VibrationCali: !in.P2S, LayerInspect: false, UseAMS: in.UseAMS,
		ExtrudeCaliFlag: 2, ExtrudeCaliManualMode: 0, NozzleOffsetCali: nozzleCali,
		SubtaskName: in.TaskName, ProfileID: "0",
		ProjectID: in.ID, SubtaskID: in.ID, TaskID: in.ID,
		AMSMapping: in.AMSMapping, AMSMapping2: in.AMSMapping2,
	}
	return json.Marshal(map[string]any{"print": cmd})
}

// ---- AMS mapping ----

// trayRef ties Bridge's positional slot number (AmsSlot.Slot) to the printer's
// real AMS unit id and tray id.
type trayRef struct{ Slot, Unit, Tray int }

// newTrayRef parses the raw ids of a report; when one does not parse the
// position stands in for it.
func newTrayRef(slot, amsIdx, trayIdx int, unitID, trayID string) trayRef {
	unit, err := strconv.Atoi(unitID)
	if err != nil {
		unit = amsIdx
	}
	tray, err := strconv.Atoi(trayID)
	if err != nil {
		tray = trayIdx
	}
	return trayRef{Slot: slot, Unit: unit, Tray: tray}
}

var (
	trayRefsMu    sync.RWMutex
	trayRefTable  = map[string][]trayRef{}
	printErrMu    sync.Mutex
	printErrTable = map[string]string{}
)

func setTrayRefs(name string, refs []trayRef) {
	trayRefsMu.Lock()
	defer trayRefsMu.Unlock()
	if refs == nil {
		delete(trayRefTable, name)
		return
	}
	trayRefTable[name] = refs
}

func getTrayRefs(name string) []trayRef {
	trayRefsMu.RLock()
	defer trayRefsMu.RUnlock()
	return trayRefTable[name]
}

func forgetPrintFileState(name string) {
	setTrayRefs(name, nil)
	printErrMu.Lock()
	delete(printErrTable, name)
	printErrMu.Unlock()
}

// noteReportedError remembers the last non-zero print error a printer
// reported (mc_print_error_code, or print_error in the raw payload) and clears
// it when a report says 0. Reports carrying neither field change nothing.
func noteReportedError(name string, payload []byte, mcCode string) {
	code, seen := "", false
	if mcCode != "" {
		seen = true
		if mcCode != "0" {
			code = mcCode
		}
	}
	if strings.Contains(string(payload), `"print_error"`) {
		var probe struct {
			Print struct {
				PrintError *flexInt `json:"print_error"`
			} `json:"print"`
		}
		if json.Unmarshal(payload, &probe) == nil && probe.Print.PrintError != nil {
			seen = true
			if v := int(*probe.Print.PrintError); v != 0 && code == "" {
				code = strconv.Itoa(v)
			}
		}
	}
	if !seen {
		return
	}
	printErrMu.Lock()
	defer printErrMu.Unlock()
	if code == "" {
		delete(printErrTable, name)
	} else {
		printErrTable[name] = code
	}
}

func lastReportedError(name string) string {
	printErrMu.Lock()
	defer printErrMu.Unlock()
	return printErrTable[name]
}

type amsRequest struct {
	Requested  []int     // opts.AMSMapping: Bridge slot numbers, -1 unused, 254 external
	Count      int       // project filament count
	Used       []int     // 0-based filament positions the plate uses; nil = all
	HasAMS     bool      // the printer reports an AMS
	ForceExt   bool      // the dialog asked for no AMS
	Trays      []trayRef // the printer's latest report
	DualNozzle bool
}

// buildAMSMapping turns FoxTrack's choice into the flat ams_mapping, the
// ams_mapping2 list and use_ams for the project_file command.
func buildAMSMapping(r amsRequest) (flat []int, ref2 []amsRef, useAMS bool, err error) {
	n := max(r.Count, len(r.Requested))
	for _, u := range r.Used {
		n = max(n, u+1)
	}
	if n == 0 {
		n = 1
	}
	used := make([]bool, n)
	if r.Used == nil {
		for i := range used {
			used[i] = true
		}
	}
	for _, u := range r.Used {
		if u >= 0 {
			used[u] = true
		}
	}

	req := r.Requested
	if req == nil {
		// Nothing chosen: only a printer without AMS (or an explicit "no AMS")
		// can print from the external spool.
		if r.HasAMS && !r.ForceExt {
			return nil, nil, false, errPrintPickSlots
		}
		req = make([]int, n)
		for i := range req {
			req[i] = 254
		}
	}

	flat = make([]int, n)
	ref2 = make([]amsRef, n)
	var anyAMS, anyExt bool
	for i := 0; i < n; i++ {
		v := -1
		if i < len(req) {
			v = req[i]
		}
		switch {
		case !used[i]:
			flat[i], ref2[i] = -1, amsRef{255, 255}
		case v < 0:
			return nil, nil, false, errPrintPickSlots
		case v >= 254:
			// Studio writes -1 flat for a virtual tray and describes it in
			// ams_mapping2: 255 (main) on single nozzle printers; dual nozzle
			// printers keep 254 (deputy / left nozzle).
			ams := 255
			if r.DualNozzle && v == 254 {
				ams = 254
			}
			flat[i], ref2[i] = -1, amsRef{ams, 0}
			anyExt = true
		default:
			t, ok := findTray(r.Trays, v)
			if !ok {
				return nil, nil, false, fmt.Errorf("AMS slot %d is not on the printer right now. Refresh FoxTrack and pick the slots again.", v+1)
			}
			if t.Unit >= 128 { // AMS HT: the unit id is the tray id
				flat[i], ref2[i] = t.Unit, amsRef{t.Unit, 0}
			} else {
				flat[i], ref2[i] = t.Unit*4+t.Tray, amsRef{t.Unit, t.Tray}
			}
			anyAMS = true
		}
	}
	if anyAMS && anyExt {
		return nil, nil, false, errPrintMixed
	}
	return flat, ref2, anyAMS, nil
}

func findTray(trays []trayRef, slot int) (trayRef, bool) {
	for _, t := range trays {
		if t.Slot == slot {
			return t, true
		}
	}
	return trayRef{}, false
}

// ---- MQTT publish ----

func clientConnected(name string) bool {
	clientMutex.RLock()
	c, ok := printerClients[name]
	clientMutex.RUnlock()
	return ok && c.IsConnected()
}

// publishProjectFile publishes on the printer's request topic, QoS 0 like the
// other commands (the printer's broker does not PUBACK QoS 1 there).
func publishProjectFile(name, serial string, payload []byte) error {
	clientMutex.RLock()
	c, ok := printerClients[name]
	clientMutex.RUnlock()
	if !ok || !c.IsConnected() {
		return errPrintNoClient
	}
	tok := c.Publish("device/"+serial+"/request", 0, false, payload)
	if !tok.WaitTimeout(5 * time.Second) {
		return errors.New("publish timed out")
	}
	return tok.Error()
}
