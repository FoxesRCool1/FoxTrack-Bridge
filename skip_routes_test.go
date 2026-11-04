//go:build headless

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// skipTestConfig returns a config with one Bambu LAN, one Klipper and one
// PrusaLink printer, the three kinds the skip routes dispatch on.
func skipTestConfig() *config.Config {
	return &config.Config{
		APIKey: "fox",
		Printers: []config.Printer{
			{ID: "b1", Name: "Skip-Bambu", Serial: "01P00A000000001", LANCode: "12345678"},
			{ID: "k1", Name: "Skip-Klipper", MoonrakerURL: "http://127.0.0.1:1"},
			{ID: "pl1", Name: "Skip-Prusa", PrusaLinkURL: "http://127.0.0.1:2"},
		},
	}
}

// swapSkipConfig replaces configStore and restores it on cleanup.
func swapSkipConfig(t *testing.T) {
	t.Helper()
	configMutex.Lock()
	old := configStore
	configStore = skipTestConfig()
	configMutex.Unlock()
	t.Cleanup(func() {
		configMutex.Lock()
		configStore = old
		configMutex.Unlock()
	})
}

// setSkipStubs replaces skipList and skipRun with stubs that record their
// arguments, and restores the originals on cleanup.
func setSkipStubs(t *testing.T, listFn func(context.Context, config.Printer) (mqttpkg.SkipList, error), runFn func(context.Context, config.Printer, []string) (mqttpkg.SkipList, error)) {
	t.Helper()
	oldList, oldRun := skipList, skipRun
	skipList = listFn
	skipRun = runFn
	t.Cleanup(func() {
		skipList = oldList
		skipRun = oldRun
	})
}

// setBambuPrinting sets the Bambu printer's live state to printing and
// restores it on cleanup.
func setBambuPrinting(t *testing.T, status string) {
	t.Helper()
	name := "Skip-Bambu"
	mqttpkg.UpdatePrinterState(name, mqttpkg.TelemetryData{Status: status})
	t.Cleanup(func() {
		mqttpkg.RemovePrinterState(name)
	})
}

// decodeSkip parses a skip response body.
func decodeSkip(t *testing.T, body []byte) skipResponse {
	t.Helper()
	var resp skipResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("bad response JSON: %v: %s", err, body)
	}
	return resp
}

func TestSkipRoutes_GetPrinting(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")

	var mu sync.Mutex
	var gotPrinter config.Printer
	var gotCalled bool
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			mu.Lock()
			gotPrinter = p
			gotCalled = true
			mu.Unlock()
			return mqttpkg.SkipList{
				Objects: []mqttpkg.SkipObject{{ID: "1065"}, {ID: "1066"}},
				Bed:     &mqttpkg.SkipBed{W: 256, H: 250},
			}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			t.Error("skipRun called on GET")
			return mqttpkg.SkipList{}, nil
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/api/objects/b1", nil)
	rec := httptest.NewRecorder()
	handleObjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := decodeSkip(t, rec.Body.Bytes())
	if !resp.Supported {
		t.Error("supported = false, want true")
	}
	if len(resp.Objects) != 2 || resp.Objects[0].ID != "1065" || resp.Objects[1].ID != "1066" {
		t.Errorf("objects = %+v, want two in order", resp.Objects)
	}
	if resp.Bed == nil || resp.Bed.W != 256 || resp.Bed.H != 250 {
		t.Errorf("bed = %+v, want 256x250", resp.Bed)
	}
	mu.Lock()
	if !gotCalled {
		t.Error("skipList not called")
	}
	if gotPrinter.ID != "b1" {
		t.Errorf("stub saw printer %q, want b1", gotPrinter.ID)
	}
	mu.Unlock()
}

func TestSkipRoutes_UnknownPrinter(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			t.Error("skipList called for unknown printer")
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, httptest.NewRequest(http.MethodGet, "/api/objects/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown printer: status = %d, want 404", rec.Code)
	}
}

func TestSkipRoutes_WrongMethod(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, httptest.NewRequest(http.MethodDelete, "/api/objects/b1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: status = %d, want 405", rec.Code)
	}
}

func TestSkipRoutes_BadPath(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, httptest.NewRequest(http.MethodGet, "/api/objects/b1/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("bad path: status = %d, want 404", rec.Code)
	}
}

func TestSkipRoutes_GetIdle(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "idle")

	var called bool
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			called = true
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, httptest.NewRequest(http.MethodGet, "/api/objects/b1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := decodeSkip(t, rec.Body.Bytes())
	if resp.Supported {
		t.Error("supported = true, want false when idle")
	}
	if !strings.Contains(resp.Reason, "running or paused") {
		t.Errorf("reason = %q, want it to mention running or paused", resp.Reason)
	}
	if called {
		t.Error("skipList called while idle")
	}
}

func TestSkipRoutes_GetUnsupported(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")

	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: "Klipper needs [exclude_object] configured."}
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, httptest.NewRequest(http.MethodGet, "/api/objects/b1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"supported":false`) {
		t.Errorf("body = %s, want supported false", body)
	}
	if !strings.Contains(body, "Klipper needs [exclude_object] configured.") {
		t.Errorf("body = %s, want the unsupported reason", body)
	}
	if !strings.Contains(body, `"objects":[]`) {
		t.Errorf("body = %s, want objects to be [] not null", body)
	}
}

func TestSkipRoutes_GetDeadline(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")

	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, context.DeadlineExceeded
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, httptest.NewRequest(http.MethodGet, "/api/objects/b1", nil))
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("deadline: status = %d, want 504", rec.Code)
	}
}

func TestSkipRoutes_GetError(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")

	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, errors.New("boom")
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, httptest.NewRequest(http.MethodGet, "/api/objects/b1", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("error: status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("body = %s, want it to contain boom", rec.Body.String())
	}
}

func TestSkipRoutes_PostPrinting(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")

	var mu sync.Mutex
	var gotIDs []string
	var gotCalled bool
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			mu.Lock()
			gotIDs = ids
			gotCalled = true
			mu.Unlock()
			return mqttpkg.SkipList{Objects: []mqttpkg.SkipObject{{ID: "1065", Skipped: true}}}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, jsonPost("/api/objects/b1/skip", `{"ids":["1065"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := decodeSkip(t, rec.Body.Bytes())
	if !resp.Supported {
		t.Error("supported = false, want true")
	}
	if len(resp.Objects) != 1 || resp.Objects[0].ID != "1065" || !resp.Objects[0].Skipped {
		t.Errorf("objects = %+v, want the updated list", resp.Objects)
	}
	mu.Lock()
	if !gotCalled {
		t.Error("skipRun not called")
	}
	if len(gotIDs) != 1 || gotIDs[0] != "1065" {
		t.Errorf("ids = %v, want [1065]", gotIDs)
	}
	mu.Unlock()
}

func TestSkipRoutes_PostNotPrinting(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "idle")

	var called bool
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			called = true
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, jsonPost("/api/objects/b1/skip", `{"ids":["1065"]}`))
	if rec.Code != http.StatusConflict {
		t.Errorf("not printing: status = %d, want 409", rec.Code)
	}
	if called {
		t.Error("skipRun called while not printing")
	}
}

func TestSkipRoutes_PostNoJSON(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/api/objects/b1/skip", strings.NewReader(`{"ids":["1065"]}`))
	rec := httptest.NewRecorder()
	handleObjects(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no JSON content type: status = %d, want 415", rec.Code)
	}
}

func TestSkipRoutes_PostEmptyIDs(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			t.Error("skipRun called with empty ids")
			return mqttpkg.SkipList{}, nil
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, jsonPost("/api/objects/b1/skip", `{"ids":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty ids: status = %d, want 400", rec.Code)
	}
}

func TestSkipRoutes_PostTooManyIDs(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")
	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			t.Error("skipRun called with too many ids")
			return mqttpkg.SkipList{}, nil
		},
	)

	ids := make([]string, 201)
	for i := range ids {
		ids[i] = `"` + string(rune('a'+i%26)) + string(rune('0'+i%10)) + `"`
	}
	body := `{"ids":[` + strings.Join(ids, ",") + `]}`
	rec := httptest.NewRecorder()
	handleObjects(rec, jsonPost("/api/objects/b1/skip", body))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("too many ids: status = %d, want 400", rec.Code)
	}
}

func TestSkipRoutes_PostUnsupported(t *testing.T) {
	swapSkipConfig(t)
	setBambuPrinting(t, "printing")

	setSkipStubs(t,
		func(ctx context.Context, p config.Printer) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, nil
		},
		func(ctx context.Context, p config.Printer, ids []string) (mqttpkg.SkipList, error) {
			return mqttpkg.SkipList{}, &mqttpkg.SkipUnsupported{Reason: "Klipper needs [exclude_object] configured."}
		},
	)

	rec := httptest.NewRecorder()
	handleObjects(rec, jsonPost("/api/objects/b1/skip", `{"ids":["1065"]}`))
	if rec.Code != http.StatusConflict {
		t.Errorf("unsupported: status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Klipper needs [exclude_object] configured.") {
		t.Errorf("body = %s, want the unsupported reason", rec.Body.String())
	}
}

func TestSkipRoutes_RealDispatch(t *testing.T) {
	swapSkipConfig(t)

	var pl config.Printer
	configMutex.RLock()
	for _, p := range configStore.Printers {
		if p.ID == "pl1" {
			pl = p
		}
	}
	configMutex.RUnlock()

	_, err := skipListFor(context.Background(), pl)
	var unsupported *mqttpkg.SkipUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("skipListFor PrusaLink: err = %v, want *SkipUnsupported", err)
	}
	if !strings.Contains(unsupported.Reason, "Bambu and Klipper") {
		t.Errorf("reason = %q, want it to mention Bambu and Klipper", unsupported.Reason)
	}

	_, err = skipRunFor(context.Background(), pl, []string{"1"})
	unsupported = nil
	if !errors.As(err, &unsupported) {
		t.Fatalf("skipRunFor PrusaLink: err = %v, want *SkipUnsupported", err)
	}
	if !strings.Contains(unsupported.Reason, "Bambu and Klipper") {
		t.Errorf("reason = %q, want it to mention Bambu and Klipper", unsupported.Reason)
	}
}
