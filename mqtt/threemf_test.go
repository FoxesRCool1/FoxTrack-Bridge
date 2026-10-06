package mqtt

import (
	"archive/zip"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// make3MF writes a zip with the given entries and returns its path.
func make3MF(t *testing.T, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "job.gcode.3mf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

const sliceInfoTwoPlates = `<?xml version="1.0" encoding="UTF-8"?>
<config>
  <header><header_item key="X-BBL-Client-Type" value="slicer"/></header>
  <plate>
    <metadata key="index" value="1"/>
    <metadata key="filament_maps" value="1 1 1"/>
    <filament id="1" type="PLA" color="#FF0000"/>
    <filament id="3" type="PETG" color="#00FF00"/>
  </plate>
  <plate>
    <metadata key="index" value="2"/>
    <filament id="2" type="PLA" color="#0000FF"/>
  </plate>
</config>`

const sliceInfoOnePlate = `<config><plate>
  <metadata key="index" value="1"/>
  <metadata key="filament_maps" value="1 1 1 1"/>
  <filament id="2" type="PLA"/>
</plate></config>`

func TestInspect3MF(t *testing.T) {
	const md5Upper = "0123456789ABCDEF0123456789ABCDEF"
	project := `{"filament_colour":["#FF0000","#00FF00","#0000FF","#FFFFFF","#000000"]}`

	cases := []struct {
		name    string
		entries map[string]string
		plate   int
		want    threeMFInfo
		wantErr error
		errText string
	}{
		{
			name: "one plate, md5 and project filament count",
			entries: map[string]string{
				"Metadata/plate_1.gcode":           "G1 X1",
				"Metadata/plate_1.gcode.md5":       md5Upper + "\n",
				"Metadata/project_settings.config": project,
				"Metadata/slice_info.config":       sliceInfoOnePlate,
			},
			want: threeMFInfo{Plate: 1, MD5: "0123456789abcdef0123456789abcdef", FilamentCount: 5, PlateFilaments: []int{1}},
		},
		{
			name: "plate 3 is the only one, md5 missing, count from filament_maps",
			entries: map[string]string{
				"Metadata/plate_3.gcode":     "G1",
				"Metadata/slice_info.config": `<config><plate><metadata key="index" value="3"/><metadata key="filament_maps" value="1 1 1 1"/><filament id="1"/></plate></config>`,
			},
			want: threeMFInfo{Plate: 3, FilamentCount: 4, PlateFilaments: []int{0}},
		},
		{
			name: "count falls back to the highest filament id",
			entries: map[string]string{
				"Metadata/plate_1.gcode":     "G1",
				"Metadata/slice_info.config": `<config><plate><metadata key="index" value="1"/><filament id="2"/><filament id="4"/></plate></config>`,
			},
			want: threeMFInfo{Plate: 1, FilamentCount: 4, PlateFilaments: []int{1, 3}},
		},
		{
			name: "chosen plate of two",
			entries: map[string]string{
				"Metadata/plate_1.gcode":           "G1",
				"Metadata/plate_2.gcode":           "G1",
				"Metadata/slice_info.config":       sliceInfoTwoPlates,
				"Metadata/project_settings.config": project,
			},
			plate: 2,
			want:  threeMFInfo{Plate: 2, FilamentCount: 5, PlateFilaments: []int{1}},
		},
		{
			name:    "no plate",
			entries: map[string]string{"3D/3dmodel.model": "x"},
			wantErr: errNoPlate,
		},
		{
			name: "two plates, none chosen",
			entries: map[string]string{
				"Metadata/plate_1.gcode": "G1",
				"Metadata/plate_2.gcode": "G1",
			},
			errText: "This file has 2 plates. Send a file with only one sliced plate, or choose the plate.",
		},
		{
			name:    "chosen plate missing",
			entries: map[string]string{"Metadata/plate_1.gcode": "G1"},
			plate:   4,
			wantErr: errPlateGone,
		},
		{
			name: "bad md5 entry is ignored",
			entries: map[string]string{
				"Metadata/plate_1.gcode":     "G1",
				"Metadata/plate_1.gcode.md5": "not a hash",
			},
			want: threeMFInfo{Plate: 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := inspect3MF(make3MF(t, c.entries), c.plate)
			switch {
			case c.wantErr != nil:
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
			case c.errText != "":
				if err == nil || err.Error() != c.errText {
					t.Fatalf("err = %v, want %q", err, c.errText)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, c.want) {
					t.Fatalf("got %+v, want %+v", got, c.want)
				}
			}
		})
	}
}

func TestInspect3MF_NotAZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.gcode.3mf")
	os.WriteFile(p, []byte("plain text"), 0o600)
	if _, err := inspect3MF(p, 0); !errors.Is(err, errNotA3MF) {
		t.Fatalf("err = %v, want errNotA3MF", err)
	}
}
