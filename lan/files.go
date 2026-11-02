package lan

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// Files on the printer (Bridge 2.5.2): Klipper lists Moonraker's gcodes root,
// OctoPrint its local storage. PrusaLink and Home Assistant are not listed.

var (
	errNoFileList = errors.New("This printer does not list its files through the Bridge.")
	errNoSuchFile = errors.New("The printer has no file with that name. Refresh the list and try again.")
)

// ListFiles lists the print files on the HTTP printer called name, newest
// first, at most mqttpkg.MaxListedFiles.
func (c *Controller) ListFiles(name string) ([]mqttpkg.PrinterFile, error) {
	c.mu.RLock()
	p, ok := c.printers[name]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("printer %q not found", name)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: insecureTransport()}
	headers := moonrakerAuthHeaders(p)
	switch p.Kind() {
	case configpkg.KindOctoPrint:
		m, err := sendJSONRequest(client, "GET", octoPrintURL(p, "/api/files/local?recursive=true"), headers, nil)
		if err != nil {
			return nil, err
		}
		return mqttpkg.NewestFirst(parseOctoPrintFiles(m), mqttpkg.MaxListedFiles), nil
	case configpkg.KindPrusaLink, configpkg.KindHomeAssistant:
		return nil, errNoFileList
	}
	u := moonrakerURL(p, "/server/files/list?root=gcodes")
	if u == "" {
		return nil, errNoAddress
	}
	m, err := sendJSONRequest(client, "GET", u, headers, nil)
	if err != nil {
		return nil, err
	}
	return mqttpkg.NewestFirst(parseMoonrakerFiles(m), mqttpkg.MaxListedFiles), nil
}

// parseMoonrakerFiles reads GET /server/files/list?root=gcodes:
// {"result":[{"path":"a.gcode","modified":1700000000.5,"size":123}]}.
func parseMoonrakerFiles(m map[string]interface{}) []mqttpkg.PrinterFile {
	list, _ := m["result"].([]interface{})
	var out []mqttpkg.PrinterFile
	for _, it := range list {
		f, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		name := stringAny(f["path"])
		if name == "" {
			continue
		}
		out = append(out, mqttpkg.PrinterFile{
			Name:       name,
			SizeBytes:  int64(floatAny(f["size"])),
			ModifiedAt: unixToRFC3339(floatAny(f["modified"])),
		})
	}
	return out
}

// parseOctoPrintFiles reads GET /api/files/local?recursive=true:
// {"files":[{"name":"a.gcode","path":"sub/a.gcode","size":123,"date":1700000000,"type":"machinecode"}]}.
// Folders are walked through their children; only machinecode (printable)
// files are listed, under their folder path.
func parseOctoPrintFiles(m map[string]interface{}) []mqttpkg.PrinterFile {
	list, _ := m["files"].([]interface{})
	var out []mqttpkg.PrinterFile
	for _, it := range list {
		f, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		switch stringAny(f["type"]) {
		case "folder":
			out = append(out, parseOctoPrintFiles(map[string]interface{}{"files": f["children"]})...)
		case "machinecode":
			name := stringAny(f["path"])
			if name == "" {
				name = stringAny(f["name"])
			}
			if name == "" {
				continue
			}
			out = append(out, mqttpkg.PrinterFile{
				Name:       name,
				SizeBytes:  int64(floatAny(f["size"])),
				ModifiedAt: unixToRFC3339(floatAny(f["date"])),
			})
		}
	}
	return out
}

func unixToRFC3339(sec float64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(int64(sec), 0).UTC().Format(time.RFC3339)
}

// PrintFile starts a file that is already on the HTTP printer: Klipper
// through /printer/print/start, OctoPrint by selecting it with print=true.
// Refused while the printer prints or is paused (PrintReady).
func (c *Controller) PrintFile(name, file string) error {
	file = strings.TrimPrefix(strings.TrimSpace(file), "/")
	// A quote or a control character (CR, LF) could close the file name in
	// the printer's G-code command and start another one.
	if file == "" || slices.Contains(strings.Split(file, "/"), "..") ||
		strings.ContainsAny(file, "\"\\") ||
		strings.IndexFunc(file, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errors.New("Pick a file from the printer's list.")
	}
	if err := c.PrintReady(name); err != nil {
		return err
	}
	c.mu.RLock()
	p := c.printers[name]
	state := c.states[name]
	c.mu.RUnlock()
	switch p.Kind() {
	case configpkg.KindOctoPrint:
		client := &http.Client{Timeout: 10 * time.Second, Transport: insecureTransport()}
		headers := moonrakerAuthHeaders(p)
		headers["Content-Type"] = "application/json"
		_, err := sendJSONRequest(client, "POST", octoPrintURL(p, octoPrintFilePath(file)), headers,
			strings.NewReader(`{"command":"select","print":true}`))
		if err != nil {
			switch err.Error() {
			case "HTTP 404":
				return errNoSuchFile
			case "HTTP 409":
				return errors.New("OctoPrint is not connected to the printer, or a print is already running.")
			}
		}
		return err
	case configpkg.KindPrusaLink, configpkg.KindHomeAssistant:
		return errNoFileList
	}
	return c.sendKlipperCommand(p, state, "start", map[string]interface{}{"file_name": file})
}

// octoPrintFilePath is /api/files/local/<path>, each segment escaped.
func octoPrintFilePath(file string) string {
	parts := strings.Split(file, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return "/api/files/local/" + strings.Join(parts, "/")
}
