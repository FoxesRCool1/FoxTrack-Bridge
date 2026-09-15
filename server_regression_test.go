//go:build headless

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"foxtrack-bridge/config"
)

// A nameless printer fails every name-keyed lookup (control, camera, history,
// the MQTT/Moonraker drivers) and blocks the next blank-ish name from being
// used, so POST /api/printers must refuse one instead of storing it.
func TestHandlePrinters_POST_RejectsBlankName(t *testing.T) {
	for _, name := range []string{"", "   ", "\t\n"} {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		configMutex.Lock()
		configStore = &config.Config{Printers: []config.Printer{}}
		configMutex.Unlock()

		body, _ := json.Marshal(config.Printer{Name: name, MoonrakerURL: "http://127.0.0.1:1/"})
		rec := httptest.NewRecorder()
		handlePrinters(rec, httptest.NewRequest(http.MethodPost, "/api/printers", strings.NewReader(string(body))))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name %q: status = %d, want 400", name, rec.Code)
		}
		configMutex.RLock()
		n := len(configStore.Printers)
		configMutex.RUnlock()
		if n != 0 {
			t.Fatalf("name %q: printers = %d, want 0 — a nameless printer was stored", name, n)
		}
	}
}

// A name typed with stray whitespace is trimmed on creation, so it matches the
// name-keyed lookups the rest of the bridge does.
func TestHandlePrinters_POST_TrimsName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	configStore = &config.Config{Printers: []config.Printer{}}
	configMutex.Unlock()

	rec := httptest.NewRecorder()
	handlePrinters(rec, httptest.NewRequest(http.MethodPost, "/api/printers",
		strings.NewReader(`{"name":"  Padded  ","moonraker_url":"http://127.0.0.1:1/"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	configMutex.RLock()
	got := configStore.Printers[0].Name
	configMutex.RUnlock()
	if got != "Padded" {
		t.Fatalf("name = %q, want %q", got, "Padded")
	}
}

// A full-replace payload may not introduce a nameless printer.
func TestResolveConfigUpdate_FullReplaceRejectsNewlyBlankName(t *testing.T) {
	old := &config.Config{Printers: twoPrinters()}
	_, err := resolveConfigUpdate(old, []byte(`{"printers":[{"name":"Fine"},{"name":"  "}]}`))
	if !errors.Is(err, errBlankPrinterName) {
		t.Fatalf("err = %v, want errBlankPrinterName", err)
	}
}

// A blank name already on disk is grandfathered, exactly like a pre-existing
// duplicate: the install must stay able to save the change that fixes it.
func TestResolveConfigUpdate_FullReplaceGrandfathersPreexistingBlankName(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{{ID: "a", Name: ""}, {ID: "b", Name: "Named"}}}
	got, err := resolveConfigUpdate(old, []byte(`{"printers":[{"id":"a","name":""},{"id":"b","name":"Named"}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Printers) != 2 {
		t.Fatalf("printers = %d, want 2", len(got.Printers))
	}
}

// Deleting a printer that is not there must say so rather than report a delete
// that never happened, and must leave the stored list untouched.
func TestDeleteUnknownPrinter_Returns404AndKeepsPrinters(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	configStore = &config.Config{Printers: twoPrinters()}
	configMutex.Unlock()

	rec := httptest.NewRecorder()
	handlePrinterByName(rec, httptest.NewRequest(http.MethodDelete, "/api/printers/no-such-printer", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	configMutex.RLock()
	n := len(configStore.Printers)
	configMutex.RUnlock()
	if n != 2 {
		t.Fatalf("printers = %d, want 2 — a failed delete must not change the list", n)
	}
}

// auto_update is a bool, so a Settings save made before the dashboard has read
// the stored config would otherwise send a default false and silently turn the
// user's auto-update off. An omitted key means "leave it alone".
func TestResolveConfigUpdate_OmittedAutoUpdatePreservesExisting(t *testing.T) {
	old := &config.Config{AutoUpdate: true, Printers: twoPrinters()}
	got, err := resolveConfigUpdate(old, []byte(`{"api_key":"","foxtrack2_api_key":""}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.AutoUpdate {
		t.Fatal("auto_update = false, want true — an omitted key must not clear it")
	}
}

// An explicit value still wins in both directions.
func TestResolveConfigUpdate_ExplicitAutoUpdateIsApplied(t *testing.T) {
	old := &config.Config{AutoUpdate: true, Printers: twoPrinters()}
	got, err := resolveConfigUpdate(old, []byte(`{"auto_update":false}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AutoUpdate {
		t.Fatal("auto_update = true, want false — an explicit value must be applied")
	}

	off := &config.Config{AutoUpdate: false, Printers: twoPrinters()}
	got, err = resolveConfigUpdate(off, []byte(`{"auto_update":true}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.AutoUpdate {
		t.Fatal("auto_update = false, want true")
	}
}

// PreviousNames has no redacted-view counterpart, so a client echoing back the
// config it was given always omits it. That round-trip must not drop it.
func TestApplyStoredSecrets_KeepsPreviousNames(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{
		{ID: "a", Name: "Now", PreviousNames: []string{"Before", "Older"}},
	}}
	got, err := resolveConfigUpdate(old, []byte(`{"printers":[{"id":"a","name":"Now"}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Printers[0].PreviousNames) != 2 {
		t.Fatalf("previous_names = %v, want the two stored names kept", got.Printers[0].PreviousNames)
	}
	// The kept slice must be a copy, not an alias into old.
	got.Printers[0].PreviousNames[0] = "mutated"
	if old.Printers[0].PreviousNames[0] != "Before" {
		t.Fatal("PreviousNames aliases the stored config instead of copying it")
	}
}

// Clone must share no memory with the original, so a snapshot taken under the
// mutex stays stable while the live config is being edited.
func TestConfigClone_IsDeep(t *testing.T) {
	orig := &config.Config{
		APIKey:     "k",
		BambuCloud: &config.BambuCloud{Email: "a@b.c", AccessToken: "tok"},
		Printers:   []config.Printer{{ID: "a", Name: "One", PreviousNames: []string{"Old"}}},
	}
	c := orig.Clone()
	c.Printers[0].Name = "Changed"
	c.Printers[0].PreviousNames[0] = "Changed"
	c.BambuCloud.Email = "changed"
	c.APIKey = "changed"

	if orig.Printers[0].Name != "One" || orig.Printers[0].PreviousNames[0] != "Old" {
		t.Fatal("Clone shares the printer slice with the original")
	}
	if orig.BambuCloud.Email != "a@b.c" {
		t.Fatal("Clone shares the Bambu account block with the original")
	}
	if orig.APIKey != "k" {
		t.Fatal("Clone shares top-level fields with the original")
	}
	if (*config.Config)(nil).Clone() != nil {
		t.Fatal("Clone of nil should be nil")
	}
}

// Saving marshals the config outside the mutex, so it must operate on a
// snapshot. Run with -race: an aliased save trips the detector here.
func TestConcurrentSavesDoNotRaceOnConfigStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	configStore = &config.Config{APIKey: "k", Printers: []config.Printer{
		{ID: "seed", Name: "Seed", MoonrakerURL: "http://127.0.0.1:1/"},
	}}
	configMutex.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"name":"p` + string(rune('a'+i)) + `","moonraker_url":"http://127.0.0.1:1/"}`
			handlePrinters(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodPost, "/api/printers", strings.NewReader(body)))
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleConfig(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"api_key":"x"}`)))
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			handlePrinterByName(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodDelete, "/api/printers/seed", nil))
		}()
	}
	wg.Wait()
}

// Per-printer camera visibility is now reachable from the dashboard, so the
// endpoint behind that button needs to hold up: it must persist the flag,
// report it back through the redacted view, and never disturb other printers.
func TestPatchCameraHidden_PersistsAndKeepsOtherPrinters(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	configStore = &config.Config{Printers: []config.Printer{
		{ID: "one", Name: "One", MoonrakerURL: "http://127.0.0.1:1/"},
		{ID: "two", Name: "Two", MoonrakerURL: "http://127.0.0.1:2/"},
	}}
	configMutex.Unlock()

	rec := httptest.NewRecorder()
	handlePrinterByName(rec, httptest.NewRequest(http.MethodPatch, "/api/printers/one",
		strings.NewReader(`{"camera_hidden":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var echoed redactedPrinter
	if err := json.Unmarshal(rec.Body.Bytes(), &echoed); err != nil {
		t.Fatalf("response is not a printer: %v", err)
	}
	if !echoed.CameraHidden {
		t.Error("response says camera_hidden is false")
	}

	configMutex.RLock()
	defer configMutex.RUnlock()
	if len(configStore.Printers) != 2 {
		t.Fatalf("printers = %d, want 2", len(configStore.Printers))
	}
	if !configStore.Printers[0].CameraHidden {
		t.Error("One: camera_hidden was not stored")
	}
	if configStore.Printers[1].CameraHidden {
		t.Error("Two: camera_hidden changed on the wrong printer")
	}
}

// The dashboard sends false to un-hide, so the flag has to clear as well as set.
func TestPatchCameraHidden_ClearsAgain(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	configStore = &config.Config{Printers: []config.Printer{
		{ID: "one", Name: "One", CameraHidden: true, MoonrakerURL: "http://127.0.0.1:1/"},
	}}
	configMutex.Unlock()

	rec := httptest.NewRecorder()
	handlePrinterByName(rec, httptest.NewRequest(http.MethodPatch, "/api/printers/one",
		strings.NewReader(`{"camera_hidden":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore.Printers[0].CameraHidden {
		t.Error("camera_hidden was not cleared")
	}
}

func TestPatchCameraHidden_RejectsUnknownPrinterAndMissingField(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configMutex.Lock()
	configStore = &config.Config{Printers: twoPrinters()}
	configMutex.Unlock()

	rec := httptest.NewRecorder()
	handlePrinterByName(rec, httptest.NewRequest(http.MethodPatch, "/api/printers/nope",
		strings.NewReader(`{"camera_hidden":true}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown printer: status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	handlePrinterByName(rec, httptest.NewRequest(http.MethodPatch, "/api/printers/Monsieur",
		strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing field: status = %d, want 400", rec.Code)
	}
}

// camera_hidden must survive a Settings save, like every other stored field.
func TestCameraHiddenSurvivesSettingsSave(t *testing.T) {
	old := &config.Config{Printers: []config.Printer{
		{ID: "one", Name: "One", CameraHidden: true},
		{ID: "two", Name: "Two"},
	}}
	got, err := resolveConfigUpdate(old, []byte(`{"api_key":"","foxtrack2_api_key":""}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Printers[0].CameraHidden {
		t.Error("camera_hidden was lost by a partial settings save")
	}
}
