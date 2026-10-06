package mqtt

// Reading what the printer command needs out of a sliced Bambu Studio file
// (.gcode.3mf, a zip). The plate gcode itself is never inflated.

import (
	"archive/zip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var plateGcodeRe = regexp.MustCompile(`^Metadata/plate_(\d+)\.gcode$`)

// maxMetaBytes caps how much of any metadata entry is read.
const maxMetaBytes = 32 << 20

// threeMFInfo is what PrintProjectFile needs from the file.
type threeMFInfo struct {
	Plate int
	// MD5 of the plate gcode, lowercase hex; "" when the file has no md5 entry.
	MD5 string
	// FilamentCount is the project's filament count: the length of the
	// ams_mapping array.
	FilamentCount int
	// PlateFilaments are the 0-based positions (slice_info filament id - 1) the
	// plate uses. nil when the file does not say.
	PlateFilaments []int
}

var (
	errNoPlate   = errors.New("This file has no sliced plate. Slice it in Bambu Studio and send the .gcode.3mf file.")
	errNotA3MF   = errors.New("This is not a Bambu Studio .gcode.3mf file.")
	errPlateGone = errors.New("The file does not contain the plate that was chosen.")
)

func inspect3MF(path string, wantPlate int) (threeMFInfo, error) {
	var info threeMFInfo
	zr, err := zip.OpenReader(path)
	if err != nil {
		return info, errNotA3MF
	}
	defer zr.Close()

	files := map[string]*zip.File{}
	var plates []int
	for _, f := range zr.File {
		files[f.Name] = f
		if m := plateGcodeRe.FindStringSubmatch(f.Name); m != nil {
			n, _ := strconv.Atoi(m[1])
			plates = append(plates, n)
		}
	}
	sort.Ints(plates)
	switch {
	case wantPlate > 0:
		if files[fmt.Sprintf("Metadata/plate_%d.gcode", wantPlate)] == nil {
			return info, errPlateGone
		}
		info.Plate = wantPlate
	case len(plates) == 0:
		return info, errNoPlate
	case len(plates) > 1:
		return info, fmt.Errorf("This file has %d plates. Send a file with only one sliced plate, or choose the plate.", len(plates))
	default:
		info.Plate = plates[0]
	}

	if f := files[fmt.Sprintf("Metadata/plate_%d.gcode.md5", info.Plate)]; f != nil {
		if b, err := readEntry(f, 256); err == nil {
			md5 := strings.ToLower(strings.TrimSpace(string(b)))
			if len(md5) == 32 && isHex(md5) {
				info.MD5 = md5
			}
		}
	}

	if f := files["Metadata/project_settings.config"]; f != nil {
		if b, err := readEntry(f, maxMetaBytes); err == nil {
			var ps struct {
				FilamentColour []string `json:"filament_colour"`
			}
			if json.Unmarshal(b, &ps) == nil {
				info.FilamentCount = len(ps.FilamentColour)
			}
		}
	}

	var maxID int
	if f := files["Metadata/slice_info.config"]; f != nil {
		if b, err := readEntry(f, maxMetaBytes); err == nil {
			ids, mapCount := plateFilamentIDs(b, info.Plate)
			for _, id := range ids {
				if id >= 1 {
					info.PlateFilaments = append(info.PlateFilaments, id-1)
					maxID = max(maxID, id)
				}
			}
			if info.FilamentCount == 0 {
				info.FilamentCount = mapCount
			}
		}
	}
	if info.FilamentCount == 0 {
		info.FilamentCount = maxID
	}
	return info, nil
}

// plateFilamentIDs reads slice_info.config: the filament ids of the plate with
// the given index (the only plate when there is just one) and the number of
// values in its filament_maps metadata.
func plateFilamentIDs(b []byte, plate int) (ids []int, mapCount int) {
	var si struct {
		Plates []struct {
			Metadata []struct {
				Key   string `xml:"key,attr"`
				Value string `xml:"value,attr"`
			} `xml:"metadata"`
			Filaments []struct {
				ID int `xml:"id,attr"`
			} `xml:"filament"`
		} `xml:"plate"`
	}
	if xml.Unmarshal(b, &si) != nil {
		return nil, 0
	}
	for _, p := range si.Plates {
		idx := 0
		maps := 0
		for _, m := range p.Metadata {
			switch m.Key {
			case "index":
				idx, _ = strconv.Atoi(strings.TrimSpace(m.Value))
			case "filament_maps":
				maps = len(strings.Fields(m.Value))
			}
		}
		if idx == plate || (idx == 0 && len(si.Plates) == 1) {
			for _, f := range p.Filaments {
				ids = append(ids, f.ID)
			}
			return ids, maps
		}
	}
	return nil, 0
}

func readEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, limit))
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
