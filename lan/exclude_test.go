package lan

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	configpkg "foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

const excludeSample = `{
	"result": {
		"eventtime": 1234.5,
		"status": {
			"exclude_object": {
				"objects": [
					{"name": "Cube_0", "center": [20, 25], "polygon": [[10, 10], [30, 10], [30, 40], [10, 40]]},
					{"name": "Cube_1", "center": [100, 100], "polygon": [[90, 90], [110, 90], [110, 110], [90, 110]]},
					{"name": "Cube_2", "center": [100, 100]}
				],
				"excluded_objects": ["Cube_0"],
				"current_object": "Cube_1"
			},
			"toolhead": {
				"axis_minimum": [0, 0, 0, 0],
				"axis_maximum": [250, 210, 250, 0]
			}
		}
	}
}`

func TestParseExcludeObjects(t *testing.T) {
	list, err := parseExcludeObjects(mustJSON(t, excludeSample))
	if err != nil {
		t.Fatalf("parseExcludeObjects: %v", err)
	}
	if len(list.Objects) != 3 {
		t.Fatalf("want 3 objects, got %d: %+v", len(list.Objects), list.Objects)
	}
	cube0, cube1, cube2 := list.Objects[0], list.Objects[1], list.Objects[2]
	if cube0.ID != "Cube_0" || cube0.Name != "Cube_0" || !cube0.Skipped || cube0.Current {
		t.Fatalf("Cube_0 wrong: %+v", cube0)
	}
	if cube1.ID != "Cube_1" || cube1.Skipped || !cube1.Current {
		t.Fatalf("Cube_1 wrong: %+v", cube1)
	}
	if cube2.Skipped || cube2.Current {
		t.Fatalf("Cube_2 wrong: %+v", cube2)
	}
	if cube0.BBox == nil || cube0.BBox[0] != 10 || cube0.BBox[1] != 170 || cube0.BBox[2] != 30 || cube0.BBox[3] != 200 {
		t.Fatalf("Cube_0 bbox not flipped to picture coordinates: %v", cube0.BBox)
	}
	if len(cube0.Polygon) != 4 || cube0.Polygon[0] != [2]float64{10, 200} || cube0.Polygon[2] != [2]float64{30, 170} {
		t.Fatalf("Cube_0 polygon wrong: %v", cube0.Polygon)
	}
	// A centre-only object gets a 10x10 box around its centre.
	if cube2.BBox == nil || cube2.BBox[0] != 95 || cube2.BBox[1] != 105 || cube2.BBox[2] != 105 || cube2.BBox[3] != 115 {
		t.Fatalf("Cube_2 bbox wrong: %v", cube2.BBox)
	}
	if len(cube2.Polygon) != 0 {
		t.Fatalf("Cube_2 should have no polygon: %v", cube2.Polygon)
	}
	if list.Bed == nil || list.Bed.W != 250 || list.Bed.H != 210 {
		t.Fatalf("bed wrong: %+v", list.Bed)
	}
}

func TestParseExcludeObjectsWithoutToolhead(t *testing.T) {
	body := `{
		"result": {
			"status": {
				"exclude_object": {
					"objects": [
						{"name": "Cube_0", "polygon": [[10, 10], [30, 10], [30, 40], [10, 40]]},
						{"name": "Cube_1", "center": [100, 100]}
					]
				}
			}
		}
	}`
	list, err := parseExcludeObjects(mustJSON(t, body))
	if err != nil {
		t.Fatalf("parseExcludeObjects: %v", err)
	}
	if list.Bed != nil {
		t.Fatalf("bed should be nil without toolhead axes: %+v", list.Bed)
	}
	// Origin from the object points: smallest x (10), largest y (100).
	cube0 := list.Objects[0]
	if cube0.BBox == nil || cube0.BBox[0] != 0 || cube0.BBox[1] != 60 || cube0.BBox[2] != 20 || cube0.BBox[3] != 90 {
		t.Fatalf("Cube_0 bbox wrong: %v", cube0.BBox)
	}
	cube1 := list.Objects[1]
	if cube1.BBox == nil || cube1.BBox[0] != 85 || cube1.BBox[1] != -5 || cube1.BBox[2] != 95 || cube1.BBox[3] != 5 {
		t.Fatalf("Cube_1 bbox wrong: %v", cube1.BBox)
	}
}

func TestParseExcludeObjectsUnsupported(t *testing.T) {
	cases := map[string]string{
		"missing exclude_object": `{"result": {"status": {"toolhead": {"axis_minimum": [0,0,0,0], "axis_maximum": [250,210,250,0]}}}}`,
		"empty objects":          `{"result": {"status": {"exclude_object": {"objects": [], "excluded_objects": [], "current_object": ""}}}}`,
	}
	for name, body := range cases {
		_, err := parseExcludeObjects(mustJSON(t, body))
		var su *mqttpkg.SkipUnsupported
		if !errors.As(err, &su) {
			t.Fatalf("%s: want *mqttpkg.SkipUnsupported, got %v", name, err)
		}
		if !strings.Contains(su.Reason, "[exclude_object]") {
			t.Fatalf("%s: reason should mention [exclude_object]: %q", name, su.Reason)
		}
	}
}

func TestSafeObjectName(t *testing.T) {
	cases := map[string]bool{
		"Cube_1": true, "benchy.v2": true, "Wuerfel(2)": true, "方块": true,
		"a b": false, "x;M112": false, "x\nM112": false, "": false, `a"b`: false,
	}
	for in, want := range cases {
		if got := safeObjectName(in); got != want {
			t.Errorf("safeObjectName(%q) = %v, want %v", in, got, want)
		}
	}
}

// skipMoonraker is a fake Moonraker that answers the exclude_object query
// and records every gcode script it receives.
func skipMoonraker(t *testing.T, body string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var scripts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "k" {
			t.Errorf("missing or wrong X-Api-Key header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/printer/objects/query":
			_, _ = w.Write([]byte(body))
		case r.Method == "POST" && r.URL.Path == "/printer/gcode/script":
			mu.Lock()
			scripts = append(scripts, r.URL.Query().Get("script"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"eventtime": 1.0}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &scripts
}

func skipController(t *testing.T, p configpkg.Printer, status string) *Controller {
	t.Helper()
	c := NewController()
	c.printers[p.Name] = p
	if status != "" {
		c.states[p.Name] = &mqttpkg.TelemetryData{Status: status}
	}
	return c
}

func TestExcludeObjectEndToEnd(t *testing.T) {
	srv, scripts := skipMoonraker(t, excludeSample)
	c := skipController(t, configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL, APIKey: "k"}, "printing")

	list, err := c.ExcludeObject(context.Background(), "Voron", []string{"Cube_1"})
	if err != nil {
		t.Fatalf("ExcludeObject: %v", err)
	}
	if len(*scripts) != 1 || (*scripts)[0] != "EXCLUDE_OBJECT NAME=Cube_1" {
		t.Fatalf("scripts: %v", *scripts)
	}
	if len(list.Objects) != 3 || list.Bed == nil || list.Bed.W != 250 {
		t.Fatalf("fresh list wrong: %+v", list)
	}
}

func TestExcludeObjectUnknownID(t *testing.T) {
	srv, scripts := skipMoonraker(t, excludeSample)
	c := skipController(t, configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL, APIKey: "k"}, "printing")

	_, err := c.ExcludeObject(context.Background(), "Voron", []string{"Nope"})
	var su *mqttpkg.SkipUnsupported
	if !errors.As(err, &su) || !strings.Contains(su.Reason, "not on the printer's plate") {
		t.Fatalf("want not-on-plate error, got %v", err)
	}
	if len(*scripts) != 0 {
		t.Fatalf("no POST expected, got %v", *scripts)
	}
}

func TestExcludeObjectAlreadySkipped(t *testing.T) {
	srv, scripts := skipMoonraker(t, excludeSample)
	c := skipController(t, configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL, APIKey: "k"}, "printing")

	_, err := c.ExcludeObject(context.Background(), "Voron", []string{"Cube_0"})
	var su *mqttpkg.SkipUnsupported
	if !errors.As(err, &su) || !strings.Contains(su.Reason, "already skipped") {
		t.Fatalf("want already-skipped error, got %v", err)
	}
	if len(*scripts) != 0 {
		t.Fatalf("no POST expected, got %v", *scripts)
	}
}

func TestExcludeUnsupportedPrinters(t *testing.T) {
	idle := skipController(t, configpkg.Printer{Name: "Idle", MoonrakerURL: "http://unused", APIKey: "k"}, "idle")
	if _, err := idle.ExcludeObjects(context.Background(), "Idle"); err == nil {
		t.Fatal("idle printer should be refused")
	} else {
		var su *mqttpkg.SkipUnsupported
		if !errors.As(err, &su) {
			t.Fatalf("want *mqttpkg.SkipUnsupported, got %v", err)
		}
	}
	if _, err := idle.ExcludeObject(context.Background(), "Idle", []string{"Cube_0"}); err == nil {
		t.Fatal("idle printer should be refused")
	} else {
		var su *mqttpkg.SkipUnsupported
		if !errors.As(err, &su) {
			t.Fatalf("want *mqttpkg.SkipUnsupported, got %v", err)
		}
	}

	prusa := skipController(t, configpkg.Printer{Name: "MK4", PrusaLinkURL: "http://unused", APIKey: "k"}, "printing")
	if _, err := prusa.ExcludeObjects(context.Background(), "MK4"); err == nil {
		t.Fatal("PrusaLink printer should be refused")
	} else {
		var su *mqttpkg.SkipUnsupported
		if !errors.As(err, &su) {
			t.Fatalf("want *mqttpkg.SkipUnsupported, got %v", err)
		}
	}
	if _, err := prusa.ExcludeObject(context.Background(), "MK4", []string{"Cube_0"}); err == nil {
		t.Fatal("PrusaLink printer should be refused")
	} else {
		var su *mqttpkg.SkipUnsupported
		if !errors.As(err, &su) {
			t.Fatalf("want *mqttpkg.SkipUnsupported, got %v", err)
		}
	}

	if _, err := idle.ExcludeObjects(context.Background(), "Ghost"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown printer should not be found: %v", err)
	}
}

func TestExcludeObjectUnsafeName(t *testing.T) {
	body := `{
		"result": {
			"status": {
				"exclude_object": {
					"objects": [
						{"name": "a b", "center": [10, 10], "polygon": [[5, 5], [15, 5], [15, 15], [5, 15]]}
					],
					"excluded_objects": [],
					"current_object": ""
				}
			}
		}
	}`
	srv, scripts := skipMoonraker(t, body)
	c := skipController(t, configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL, APIKey: "k"}, "printing")

	_, err := c.ExcludeObject(context.Background(), "Voron", []string{"a b"})
	var su *mqttpkg.SkipUnsupported
	if !errors.As(err, &su) || !strings.Contains(su.Reason, "unusual characters") {
		t.Fatalf("want unsafe-name error, got %v", err)
	}
	if len(*scripts) != 0 {
		t.Fatalf("no POST expected, got %v", *scripts)
	}
}

func TestExcludeObjectsListsOnly(t *testing.T) {
	srv, scripts := skipMoonraker(t, excludeSample)
	c := skipController(t, configpkg.Printer{Name: "Voron", MoonrakerURL: srv.URL, APIKey: "k"}, "paused")

	list, err := c.ExcludeObjects(context.Background(), "Voron")
	if err != nil {
		t.Fatalf("ExcludeObjects: %v", err)
	}
	if len(list.Objects) != 3 || !list.Objects[0].Skipped || !list.Objects[1].Current {
		t.Fatalf("list wrong: %+v", list)
	}
	if len(*scripts) != 0 {
		t.Fatalf("no POST expected, got %v", *scripts)
	}
}
