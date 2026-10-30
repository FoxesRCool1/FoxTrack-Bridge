package mqtt

import (
	"context"
	"errors"
	"log"
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

// Test seam.
var ftpsList = ftps.List

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
	cfg := ftps.Config{
		Host: p.IP, User: "bblp", Password: p.LANCode,
		Logf: func(format string, args ...any) {
			log.Printf("[%s] list_files: ftps: "+format, append([]any{p.Name}, args...)...)
		},
	}
	var out []PrinterFile
	for _, dir := range bambuFileDirs {
		entries, err := ftpsList(ctx, cfg, dir)
		if err != nil {
			if dir != "/" {
				continue // no cache folder on this printer
			}
			log.Printf("[%s] list_files: %v", p.Name, err)
			if errors.Is(err, ftps.ErrLogin) {
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

// PrintPrinterFile starts a file that is already on the printer (a name from
// ListPrinterFiles): the project_file command with the printer-local path,
// plate 1, the AMS when the printer has one. Refused while printing.
func PrintPrinterFile(ctx context.Context, p Printer, name string) error {
	logf := func(format string, args ...any) {
		log.Printf("[%s] print_printer_file: "+format, append([]any{p.Name}, args...)...)
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" || strings.Contains(name, "..") {
		return errFileName
	}
	if !IsBambuPrintFile(name) {
		return errFileNotCard
	}
	if err := PrintPreflight(p); err != nil {
		logf("refused: %v", err)
		return err
	}
	prefix := serialPrefix(p.Serial)
	st := GetPrinterState(p.Name)
	payload, err := buildProjectFile(projectFileInput{
		SeqID: nextSequenceID(), ID: newJobID(),
		Plate: 1, RemoteName: name,
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
