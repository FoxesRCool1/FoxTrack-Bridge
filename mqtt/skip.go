package mqtt

// Skipping single objects on the plate during a print (Bambu half). The
// printer reports its objects in the status report (gcode_file, gcode_state,
// s_obj); Bridge reads the .3mf the print came from to name them and sends
// skip_objects back.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/ftps"
)

type SkipObject struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Skipped bool         `json:"skipped"`
	Current bool         `json:"current"` // never set on Bambu (the printer does not say)
	BBox    []float64    `json:"bbox,omitempty"`
	Polygon [][2]float64 `json:"polygon,omitempty"`
}

type SkipBed struct {
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type SkipList struct {
	Objects []SkipObject `json:"objects"`
	Bed     *SkipBed     `json:"bed,omitempty"`
}

// SkipUnsupported means skipping objects cannot be done for this printer or
// print right now; Reason is plain English for the user.
type SkipUnsupported struct{ Reason string }

func (e *SkipUnsupported) Error() string { return e.Reason }

// Per-printer job memory.
type skipJob struct {
	mu         sync.Mutex // guards the fields below
	fetch      sync.Mutex // held while a file is downloaded
	gcodeFile  string
	doneFile   string // gcode_file of the print that last ended
	idle       bool
	reported   map[string]bool // ids the printer says are skipped
	sent       map[string]bool // ids Bridge sent
	bridgeTask string
	bridge     Printer // the printer Bridge started its print on
	uploads    int     // Bridge uploads started; a pending file removal skips if it moved
	cacheKey   string
	cached     []SkipObject
}

var (
	skipJobsMu sync.Mutex
	skipJobs   = map[string]*skipJob{}
)

func jobFor(name string) *skipJob {
	skipJobsMu.Lock()
	defer skipJobsMu.Unlock()
	j, ok := skipJobs[name]
	if !ok {
		j = &skipJob{reported: map[string]bool{}, sent: map[string]bool{}}
		skipJobs[name] = j
	}
	return j
}

// skipFetchTimeout caps the file download; skipConfirmWait how long to wait
// for the printer's answer to skip_objects.
var (
	skipFetchTimeout = 2 * time.Minute
	skipConfirmWait  = 2 * time.Second
)

var errSkipNoFile = errors.New("Bridge could not find this print's file on the printer. It looks in the printer's cache folder and on its SD card for a .3mf named like the print.")

// noteSkipFields is called for every MQTT report of a printer.
func noteSkipFields(name string, payload []byte) {
	if !bytes.Contains(payload, []byte("gcode_file")) &&
		!bytes.Contains(payload, []byte("s_obj")) &&
		!bytes.Contains(payload, []byte("gcode_state")) {
		return
	}
	var probe struct {
		Print struct {
			GcodeFile  string `json:"gcode_file"`
			GcodeState string `json:"gcode_state"`
			SObj       *[]any `json:"s_obj"`
		} `json:"print"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return
	}
	pr := probe.Print
	j := jobFor(name)
	j.mu.Lock()
	defer j.mu.Unlock()
	switch pr.GcodeState {
	case "IDLE", "FINISH", "FAILED":
		if !j.idle {
			j.doneFile = j.gcodeFile
			if j.bridgeTask != "" {
				removeBridgeFileAfter(j, j.bridge, j.uploads)
			}
			if j.doneFile == "" {
				j.doneFile = pr.GcodeFile
			}
		}
		j.idle = true
		j.gcodeFile = ""
		j.reported = map[string]bool{}
		j.sent = map[string]bool{}
		j.bridgeTask = ""
		j.cacheKey = ""
		j.cached = nil
	default:
		if pr.GcodeState != "" {
			j.idle = false
		}
	}
	// Reports are partial: the next print's gcode_file can arrive before
	// gcode_state leaves FINISH, so keep it even while idle, unless it is
	// still the file of the print that just ended.
	if pr.GcodeFile != "" && (!j.idle || pr.GcodeFile != j.doneFile) {
		j.gcodeFile = pr.GcodeFile
	}
	if j.idle {
		return // a finished print's s_obj must not reach the next print
	}
	if pr.SObj != nil {
		rep := map[string]bool{}
		for _, v := range *pr.SObj {
			if id := skipIDText(v); id != "" {
				rep[id] = true
			}
		}
		j.reported = rep
	}
}

// skipIDText turns a JSON number or numeric string into integer text ("1065").
func skipIDText(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return strconv.FormatInt(i, 10)
		}
	}
	return ""
}

// noteBridgeJob is called when Bridge itself started a print.
func noteBridgeJob(p Printer, taskName string) {
	j := jobFor(p.Name)
	j.mu.Lock()
	j.bridgeTask = taskName
	j.bridge = p
	j.mu.Unlock()
}

// noteBridgeUpload is called when Bridge starts uploading a print file.
func noteBridgeUpload(name string) {
	j := jobFor(name)
	j.mu.Lock()
	j.uploads++
	j.mu.Unlock()
}

// removeBridgeFileAfter removes the file Bridge stored on the printer once its
// print has ended (the printer keeps it otherwise). It waits bridgeFileWait
// first, and skips the delete if the printer is printing or preparing again,
// or if Bridge began a new upload since (a queued next print overwrites the
// same file and may still be uploading). Called with j.mu held.
func removeBridgeFileAfter(j *skipJob, p Printer, uploads int) {
	wait := bridgeFileWait
	go func() {
		time.Sleep(wait)
		j.mu.Lock()
		moved := j.uploads != uploads
		j.mu.Unlock()
		if moved {
			return
		}
		if s := GetPrinterState(p.Name).Status; InPrint(s) || s == "PREPARE" || s == "SLICING" {
			return
		}
		if err := ftpsDelete(p.IP, p.LANCode, printerStoredName); err != nil {
			log.Printf("[%s] could not remove %s from the printer: %v", p.Name, printerStoredName, err)
			return
		}
		log.Printf("[%s] removed %s from the printer", p.Name, printerStoredName)
	}()
}

// ---- Bambu 3mf reading ----

var plateInGcodeFileRe = regexp.MustCompile(`plate_(\d+)\.gcode`)

// plateFromGcodeFile is the N of "plate_N.gcode" found anywhere in s, 0 when absent.
func plateFromGcodeFile(s string) int {
	m := plateInGcodeFileRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

var plateJSONRe = regexp.MustCompile(`^Metadata/plate_(\d+)\.json$`)

type plateBox struct {
	Name string
	BBox []float64
}

// read3MFObjects reads the plate's objects out of a sliced .3mf.
func read3MFObjects(filePath string, wantPlate int) ([]SkipObject, error) {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return nil, errNotA3MF
	}
	defer zr.Close()

	plate := wantPlate
	if plate <= 0 {
		plate = 1
	}
	// When no plate was chosen, the sliced plate is the lowest one with
	// gcode, else the lowest with a plate json, else 1.
	if wantPlate <= 0 {
		best, count := 0, 0
		for _, f := range zr.File {
			if m := plateGcodeRe.FindStringSubmatch(f.Name); m != nil {
				if n, _ := strconv.Atoi(m[1]); n > 0 {
					count++
					if best == 0 || n < best {
						plate = n
						best = n
					}
				}
			}
		}
		if best == 0 {
			for _, f := range zr.File {
				if m := plateJSONRe.FindStringSubmatch(f.Name); m != nil {
					if n, _ := strconv.Atoi(m[1]); n > 0 {
						count++
						if best == 0 || n < best {
							plate = n
							best = n
						}
					}
				}
			}
		}
		// Guessing could list another plate's objects: refuse instead.
		if count > 1 {
			return nil, &SkipUnsupported{"Bridge could not tell which plate of this project is printing, so it cannot list the objects safely."}
		}
	}

	var sliceInfo, plateJSONB []byte
	for _, f := range zr.File {
		switch f.Name {
		case "Metadata/slice_info.config":
			sliceInfo, _ = readEntry(f, maxMetaBytes)
		case fmt.Sprintf("Metadata/plate_%d.json", plate):
			plateJSONB, _ = readEntry(f, maxMetaBytes)
		}
	}

	objs := parseSliceInfoObjects(sliceInfo, plate)
	boxes := parsePlateJSON(plateJSONB)
	if len(objs) == 0 {
		// No object list in slice_info: build it from the plate json.
		for id, box := range boxes {
			objs = append(objs, SkipObject{ID: id, Name: box.Name, BBox: box.BBox})
		}
		sort.Slice(objs, func(i, j int) bool {
			ni, _ := strconv.Atoi(objs[i].ID)
			nj, _ := strconv.Atoi(objs[j].ID)
			return ni < nj
		})
	} else {
		for i := range objs {
			if box, ok := boxes[objs[i].ID]; ok {
				objs[i].BBox = box.BBox
			}
		}
	}
	if len(objs) == 0 {
		return nil, &SkipUnsupported{"This print file has no object list. Skipping objects needs a file sliced in Bambu Studio 1.8 or newer."}
	}
	return objs, nil
}

// parseSliceInfoObjects reads the objects of the plate with the given index
// (the only plate when none has an index) from slice_info.config.
func parseSliceInfoObjects(b []byte, plate int) []SkipObject {
	if len(b) == 0 {
		return nil
	}
	var si struct {
		Plates []struct {
			Metadata []struct {
				Key   string `xml:"key,attr"`
				Value string `xml:"value,attr"`
			} `xml:"metadata"`
			Objects []struct {
				IdentifyID string `xml:"identify_id,attr"`
				Name       string `xml:"name,attr"`
				Skipped    string `xml:"skipped,attr"`
			} `xml:"object"`
		} `xml:"plate"`
	}
	if xml.Unmarshal(b, &si) != nil {
		return nil
	}
	for _, p := range si.Plates {
		idx := 0
		for _, m := range p.Metadata {
			if m.Key == "index" {
				idx, _ = strconv.Atoi(strings.TrimSpace(m.Value))
			}
		}
		if idx != plate && !(idx == 0 && len(si.Plates) == 1) {
			continue
		}
		var objs []SkipObject
		for _, o := range p.Objects {
			// Only numeric ids can be sent to the printer.
			n, err := strconv.Atoi(strings.TrimSpace(o.IdentifyID))
			if err != nil {
				continue
			}
			id := strconv.Itoa(n)
			name := o.Name
			if name == "" {
				name = "Object " + id
			}
			objs = append(objs, SkipObject{
				ID:      id,
				Name:    name,
				Skipped: strings.EqualFold(o.Skipped, "true"),
			})
		}
		return objs
	}
	return nil
}

// parsePlateJSON reads Metadata/plate_N.json: {"bbox_all":[..],"bbox_objects":
// [{"name":"Cube","id":1065,"area":123.4,"bbox":[minx,miny,maxx,maxy]}]}.
func parsePlateJSON(b []byte) map[string]plateBox {
	out := map[string]plateBox{}
	if len(b) == 0 {
		return out
	}
	var pj struct {
		BBoxObjects []struct {
			Name string      `json:"name"`
			ID   json.Number `json:"id"`
			BBox []float64   `json:"bbox"`
		} `json:"bbox_objects"`
	}
	if json.Unmarshal(b, &pj) != nil {
		return out
	}
	for _, o := range pj.BBoxObjects {
		id, err := strconv.ParseInt(o.ID.String(), 10, 64)
		if err != nil {
			continue
		}
		key := strconv.FormatInt(id, 10)
		box := plateBox{Name: o.Name}
		if len(o.BBox) == 4 {
			box.BBox = o.BBox
		}
		out[key] = box
	}
	return out
}

// ---- file picking and payload ----

// pickSkipFile finds the print's .3mf in the printer's file list.
func pickSkipFile(files []PrinterFile, gcodeFile, subtask string, bridgeJob bool) (string, bool) {
	var candidates []string
	if base := path.Base(gcodeFile); strings.HasSuffix(strings.ToLower(base), ".3mf") {
		candidates = append(candidates, base)
	}
	if bridgeJob {
		candidates = append(candidates, printerStoredName)
	}
	if subtask != "" {
		candidates = append(candidates, subtask+".gcode.3mf", subtask+".3mf")
	}
	for _, c := range candidates {
		// Several files can share a name: the newest is the one just printed.
		best := -1
		for i, f := range files {
			if path.Base(f.Name) == c && (best < 0 || f.ModifiedAt > files[best].ModifiedAt) {
				best = i
			}
		}
		if best >= 0 {
			return files[best].Name, true
		}
	}
	return "", false
}

// buildSkipObjects is the skip_objects request payload.
func buildSkipObjects(seq string, ids []int) []byte {
	if ids == nil {
		ids = []int{}
	}
	b, _ := json.Marshal(struct {
		Print struct {
			SequenceID string `json:"sequence_id"`
			Command    string `json:"command"`
			ObjList    []int  `json:"obj_list"`
		} `json:"print"`
	}{Print: struct {
		SequenceID string `json:"sequence_id"`
		Command    string `json:"command"`
		ObjList    []int  `json:"obj_list"`
	}{seq, "skip_objects", ids}})
	return b
}

// ---- flow ----

func skipPreflight(p Printer) error {
	if p.Cloud || IsCloudSerial(p.Serial) {
		return &SkipUnsupported{"Skipping objects needs the printer in LAN Only Mode with Developer Mode on. It does not work over Bambu Cloud."}
	}
	if !printerClientConnected(p.Name) {
		return &SkipUnsupported{errPrintNoClient.Error()}
	}
	switch GetPrinterState(p.Name).Status {
	case "printing", "paused":
		return nil
	}
	return &SkipUnsupported{"Objects can be skipped only while a print is running or paused."}
}

// loadSkipObjects downloads the print's .3mf and reads its objects, caching
// the result per (printer, subtask, gcode file).
func loadSkipObjects(ctx context.Context, p Printer) ([]SkipObject, error) {
	j := jobFor(p.Name)
	subtask := GetPrinterState(p.Name).FileName
	j.mu.Lock()
	key := p.Name + "\x00" + subtask + "\x00" + j.gcodeFile
	gcodeFile := j.gcodeFile
	bridgeJob := subtask != "" && subtask == j.bridgeTask
	if j.cacheKey == key && j.cached != nil {
		cached := append([]SkipObject(nil), j.cached...)
		j.mu.Unlock()
		return cached, nil
	}
	j.mu.Unlock()

	j.fetch.Lock()
	defer j.fetch.Unlock()
	j.mu.Lock()
	if j.cacheKey == key && j.cached != nil {
		cached := append([]SkipObject(nil), j.cached...)
		j.mu.Unlock()
		return cached, nil
	}
	j.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, skipFetchTimeout)
	defer cancel()
	files, err := ListPrinterFiles(ctx, p)
	if err != nil {
		return nil, err
	}
	name, ok := pickSkipFile(files, gcodeFile, subtask, bridgeJob)
	if !ok {
		return nil, errSkipNoFile
	}
	tmp, err := os.CreateTemp("", "foxtrack-skip-*.3mf")
	if err != nil {
		log.Printf("[%s] skip_objects: no temp file: %v", p.Name, err)
		return nil, errors.New("Bridge could not read the print file from the printer. Check that the printer is on and in LAN Only Mode, then try again.")
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	cfg := bambuFTPSConfig(p, "skip_objects")
	remote := "/" + name
	err = ftpsDownload(ctx, cfg, remote, tmp.Name(), maxPlateProbeBytes)
	if useClearData(ctx, serialPrefix(p.Serial), err) {
		cfg.ClearData = true
		err = ftpsDownload(ctx, cfg, remote, tmp.Name(), maxPlateProbeBytes)
	}
	if err != nil {
		switch {
		case errors.Is(err, ftps.ErrTooLarge):
			return nil, errors.New("The print file is bigger than 200 MB, so Bridge did not read it.")
		case errors.Is(err, ftps.ErrLogin):
			return nil, errPrintAccess
		case ctx.Err() != nil:
			return nil, ctx.Err()
		default:
			log.Printf("[%s] skip_objects: download failed: %v", p.Name, err)
			return nil, errors.New("Bridge could not read the print file from the printer. Check that the printer is on and in LAN Only Mode, then try again.")
		}
	}
	objs, err := read3MFObjects(tmp.Name(), plateFromGcodeFile(gcodeFile))
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	j.cacheKey = key
	j.cached = append([]SkipObject(nil), objs...)
	j.mu.Unlock()
	// A cache stored just after the print ended is dropped by the next idle report.
	return objs, nil
}

// skipView is the object list as the user sees it: the file's skipped flag plus
// what the printer reports and what Bridge has sent.
func skipView(objs []SkipObject, j *skipJob) SkipList {
	j.mu.Lock()
	out := make([]SkipObject, len(objs))
	for i, o := range objs {
		out[i] = o
		out[i].Skipped = o.Skipped || j.reported[o.ID] || j.sent[o.ID]
	}
	j.mu.Unlock()
	return SkipList{Objects: out}
}

// ListSkipObjects is the object list for the print that is running or paused.
func ListSkipObjects(ctx context.Context, p Printer) (SkipList, error) {
	if err := skipPreflight(p); err != nil {
		return SkipList{}, err
	}
	objs, err := loadSkipObjects(ctx, p)
	if err != nil {
		return SkipList{}, err
	}
	return skipView(objs, jobFor(p.Name)), nil
}

// SkipObjects tells the printer to skip the named objects.
func SkipObjects(ctx context.Context, p Printer, ids []string) (SkipList, error) {
	if err := skipPreflight(p); err != nil {
		return SkipList{}, err
	}
	seen := map[string]bool{}
	var clean []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	if len(clean) == 0 {
		return SkipList{}, errors.New("Pick at least one object to skip.")
	}
	objs, err := loadSkipObjects(ctx, p)
	if err != nil {
		return SkipList{}, err
	}
	onPlate := map[string]bool{}
	for _, o := range objs {
		onPlate[o.ID] = true
	}
	for _, id := range clean {
		if !onPlate[id] {
			return SkipList{}, &SkipUnsupported{"One of those objects is not on the printer's plate. Refresh the list and try again."}
		}
	}
	j := jobFor(p.Name)
	v := skipView(objs, j)
	var fresh []string
	for _, id := range clean {
		skipped := false
		for _, o := range v.Objects {
			if o.ID == id {
				skipped = o.Skipped
				break
			}
		}
		if !skipped {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) == 0 {
		return SkipList{}, &SkipUnsupported{"Those objects are already skipped."}
	}
	// The download can take a while: the printer may have been stopped meanwhile.
	if err := skipPreflight(p); err != nil {
		return SkipList{}, err
	}
	nums := make([]int, 0, len(fresh))
	for _, id := range fresh {
		n, err := strconv.Atoi(id)
		if err != nil {
			return SkipList{}, &SkipUnsupported{"One of those objects has an ID Bridge cannot send to the printer. Skip it on the printer instead."}
		}
		nums = append(nums, n)
	}
	payload := buildSkipObjects(nextSequenceID(), nums)
	clearCommandRefusal(p.Name, "skip_objects")
	if err := publishRequestFn(p.Name, p.Serial, payload); err != nil {
		log.Printf("[%s] skip_objects: publish failed: %v", p.Name, err)
		return SkipList{}, errPrintNoClient
	}
	j.mu.Lock()
	for _, id := range fresh {
		j.sent[id] = true
	}
	j.mu.Unlock()

	deadline := time.Now().Add(skipConfirmWait)
	for {
		if reason, refused := commandRefusal(p.Name, "skip_objects"); refused {
			j.mu.Lock()
			for _, id := range fresh {
				delete(j.sent, id)
			}
			j.mu.Unlock()
			if reason == "" {
				return SkipList{}, errors.New("The printer refused to skip the objects. Check the printer screen.")
			}
			return SkipList{}, fmt.Errorf("The printer refused to skip the objects (%s). Check the printer screen.", reason)
		}
		j.mu.Lock()
		done := true
		for _, id := range fresh {
			if !j.reported[id] {
				done = false
				break
			}
		}
		j.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			// Do not show objects as skipped that the printer never confirmed.
			j.mu.Lock()
			for _, id := range fresh {
				delete(j.sent, id)
			}
			j.mu.Unlock()
			return SkipList{}, errors.New("The printer did not confirm the skip. Check the plate and try again.")
		}
		select {
		case <-ctx.Done():
			return SkipList{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	log.Printf("[%s] skip_objects sent: %v", p.Name, fresh)
	return skipView(objs, j), nil
}
