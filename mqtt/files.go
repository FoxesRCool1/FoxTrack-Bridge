package mqtt

import (
	"context"
	"errors"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"foxtrack-bridge/ftps"
)

// Files on the printer (Bridge 2.8, FoxTrack migration 20261031000291):
// FoxTrack sends list_files and gets the list back in the command's result;
// print_printer_file {name} starts one of them. The shape here is what every
// driver returns and what FoxTrack reads.

// PrinterFile is one file on the printer as FoxTrack shows it.
type PrinterFile struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"size_bytes"`
	ModifiedAt string `json:"modified_at"` // RFC 3339, UTC
}

// MaxListedFiles caps a listing: FoxTrack shows the newest ones.
const MaxListedFiles = 200

// NewestFirst sorts by modified time, newest first, then by name, and keeps
// at most max entries.
func NewestFirst(files []PrinterFile, max int) []PrinterFile {
	out := append([]PrinterFile(nil), files...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ModifiedAt != out[j].ModifiedAt {
			return out[i].ModifiedAt > out[j].ModifiedAt
		}
		return out[i].Name < out[j].Name
	})
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// IsBambuPrintFile reports whether a name is a file a Bambu printer prints
// from its card: .3mf or .gcode.3mf.
func IsBambuPrintFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".3mf")
}

var (
	errListNoClient = errors.New("Bridge could not reach the printer's file storage. Check that the printer is on and in LAN Only Mode.")
	errFileName     = errors.New("Pick a file from the printer's list.")
	errFileNotCard  = errors.New("The printer prints .3mf and .gcode.3mf files from its card.")
)

// Test seams.
var (
	ftpsList     = ftps.List
	ftpsDownload = ftps.Download
)

// maxPlateProbeBytes caps the download that finds a file's sliced plate.
const maxPlateProbeBytes = 200 << 20

func bambuFTPSConfig(p Printer, op string) ftps.Config {
	return ftps.Config{
		Host: p.IP, User: "bblp", Password: p.LANCode,
		Logf: func(format string, args ...any) {
			log.Printf("[%s] %s: ftps: "+format, append([]any{p.Name, op}, args...)...)
		},
	}
}

// slicedPlate downloads a .3mf from the printer and returns the plate it was
// sliced for. Any failure is logged and gives plate 1: the file name is all
// the printer needs, the plate only picks Metadata/plate_N.gcode.
func slicedPlate(ctx context.Context, p Printer, name string, logf func(string, ...any)) int {
	tmp, err := os.CreateTemp("", "foxtrack-plate-*.3mf")
	if err != nil {
		logf("plate: no temp file (%v), using plate 1", err)
		return 1
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	cfg := bambuFTPSConfig(p, "print_printer_file")
	remote := "/" + name
	err = ftpsDownload(ctx, cfg, remote, tmp.Name(), maxPlateProbeBytes)
	if useClearData(ctx, serialPrefix(p.Serial), err) {
		cfg.ClearData = true
		err = ftpsDownload(ctx, cfg, remote, tmp.Name(), maxPlateProbeBytes)
	}
	if err != nil {
		logf("plate: download failed (%v), using plate 1", err)
		return 1
	}
	plate, err := firstSlicedPlate(tmp.Name())
	if err != nil {
		logf("plate: %v, using plate 1", err)
		return 1
	}
	logf("plate %d is the sliced one", plate)
	return plate
}

// bambuFileDirs are the folders a Bambu printer keeps print files in: the
// card root and the cache Bambu Studio sends to.
var bambuFileDirs = []string{"/", "/cache"}

// ListPrinterFiles lists the .3mf files on a Bambu printer over FTPS, newest
// first, at most MaxListedFiles. A file in /cache is named "cache/<name>" so
// PrintPrinterFile can address it.
func ListPrinterFiles(ctx context.Context, p Printer) ([]PrinterFile, error) {
	if p.Cloud || IsCloudSerial(p.Serial) {
		return nil, errPrintCloud
	}
	cfg := bambuFTPSConfig(p, "list_files")
	serial := serialPrefix(p.Serial)
	list := func(dir string) ([]ftps.Entry, error) {
		entries, err := ftpsList(ctx, cfg, dir)
		if useClearData(ctx, serial, err) {
			// A1 / A1 mini: the TLS data channel can hang, as for an upload.
			cfg.ClearData = true
			entries, err = ftpsList(ctx, cfg, dir)
		}
		return entries, err
	}
	var out []PrinterFile
	for _, dir := range bambuFileDirs {
		entries, err := list(dir)
		if err != nil {
			if dir != "/" && errors.Is(err, ftps.ErrNotFound) {
				continue // no cache folder on this printer
			}
			log.Printf("[%s] list_files: %v", p.Name, err)
			switch {
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case errors.Is(err, ftps.ErrLogin):
				return nil, errPrintAccess
			}
			return nil, errListNoClient
		}
		prefix := strings.TrimPrefix(strings.TrimSuffix(dir, "/")+"/", "/")
		for _, e := range entries {
			if !IsBambuPrintFile(e.Name) {
				continue
			}
			out = append(out, PrinterFile{
				Name:       prefix + e.Name,
				SizeBytes:  e.Size,
				ModifiedAt: e.Modified.UTC().Format(time.RFC3339),
			})
		}
	}
	return NewestFirst(out, MaxListedFiles), nil
}

// badFileName reports a path that leaves the printer's file folders: a ".."
// segment (not just two dots inside a name like "v1..2.3mf") or a backslash.
func badFileName(name string) bool {
	if strings.Contains(name, `\`) {
		return true
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// PrintPrinterFile starts a file that is already on the printer (a name from
// ListPrinterFiles): the project_file command with the printer-local path,
// the given plate (0 = the sliced plate, read from the file after downloading
// it from the printer; plate 1 when that fails), the AMS when the printer has
// one. Refused while printing.
func PrintPrinterFile(ctx context.Context, p Printer, name string, plate int) error {
	logf := func(format string, args ...any) {
		log.Printf("[%s] print_printer_file: "+format, append([]any{p.Name}, args...)...)
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" || badFileName(name) {
		return errFileName
	}
	if !IsBambuPrintFile(name) {
		return errFileNotCard
	}
	if err := PrintPreflight(p); err != nil {
		logf("refused: %v", err)
		return err
	}
	if plate < 1 {
		plate = slicedPlate(ctx, p, name, logf)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The download can take a while: the printer may have been started meanwhile.
		if err := PrintPreflight(p); err != nil {
			logf("refused after download: %v", err)
			return err
		}
	}
	prefix := serialPrefix(p.Serial)
	st := GetPrinterState(p.Name)
	payload, err := buildProjectFile(projectFileInput{
		SeqID: nextSequenceID(), ID: newJobID(),
		Plate: plate, RemoteName: name,
		TaskName: strings.TrimSuffix(strings.TrimSuffix(path.Base(name), ".3mf"), ".gcode"),
		MD5:      "", UseAMS: len(st.AMS) > 0,
		DualNozzle: dualNozzle(prefix), P2S: prefix == "22E",
	})
	if err != nil {
		logf("build print command: %v", err)
		return errors.New("Bridge could not prepare the print command for the printer. Try again.")
	}
	baseline := lastReportedError(p.Name)
	clearCommandRefusal(p.Name, "project_file")
	published := time.Now()
	if err := publishRequestFn(p.Name, p.Serial, payload); err != nil {
		logf("publish failed: %v", err)
		return errPrintNoClient
	}
	logf("project_file sent for %s", name)
	if err := waitForStart(ctx, p.Name, baseline); err != nil {
		logf("start not confirmed after %s: %v", time.Since(published).Round(time.Second), err)
		return err
	}
	logf("print started after %s", time.Since(published).Round(time.Second))
	return nil
}
