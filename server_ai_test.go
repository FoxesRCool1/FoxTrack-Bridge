//go:build headless

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"foxtrack-bridge/ai"
	"foxtrack-bridge/config"
)

// isolateConfigDir points config.ConfigDir() at a temp directory so a handler
// that saves cannot touch the developer's real config.json.
func isolateConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func aiPrinters() []config.Printer {
	return []config.Printer{
		{ID: "p1", Name: "Monsieur", IP: "192.168.87.22", Serial: "01P00C580801716", LANCode: "81f1aafd"},
		{ID: "p2", Name: "Sherlock", MoonrakerURL: "http://192.168.87.30:7125"},
	}
}

// The v2.2.0 hazard in a new place: saving the assistant's settings must not be
// able to take the printer list with it.
func TestHandleAISettings_SavePreservesPrinters(t *testing.T) {
	isolateConfigDir(t)
	configMutex.Lock()
	configStore = &config.Config{APIKey: "fox", Printers: aiPrinters()}
	configMutex.Unlock()

	body := `{"enabled":true,"preset":"openai","model":"gpt-4o-mini","api_key":"sk-secret-value","allow_camera":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/ai/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleAISettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	configMutex.RLock()
	defer configMutex.RUnlock()
	if len(configStore.Printers) != 2 {
		t.Fatalf("printers = %d, want 2 — an AI settings save wiped the printer list", len(configStore.Printers))
	}
	if configStore.APIKey != "fox" {
		t.Errorf("FoxTrack key = %q, want it preserved", configStore.APIKey)
	}
	if configStore.AI == nil || configStore.AI.APIKey != "sk-secret-value" {
		t.Errorf("AI key was not stored")
	}
}

// The provider key must never reach a response body, on any path.
func TestHandleAISettings_ResponseNeverCarriesTheKey(t *testing.T) {
	isolateConfigDir(t)
	const secret = "sk-do-not-leak-this-value"
	configMutex.Lock()
	configStore = &config.Config{
		Printers: aiPrinters(),
		AI:       &config.AI{Enabled: true, Preset: "openai", Model: "gpt-4o-mini", APIKey: secret},
	}
	configMutex.Unlock()

	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"get", httptest.NewRequest(http.MethodGet, "/api/ai/settings", nil)},
		{"post", httptest.NewRequest(http.MethodPost, "/api/ai/settings", strings.NewReader(`{"enabled":true,"preset":"openai","model":"gpt-4o-mini"}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleAISettings(rec, tc.req)
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("response leaked the provider key: %s", rec.Body.String())
			}
			var view redactedAI
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !view.APIKeySet {
				t.Error("api_key_set = false, want true")
			}
		})
	}
}

// An omitted api_key means "keep the stored one". Without this a user who edits
// the model name loses the key they cannot see.
func TestHandleAISettings_OmittedKeyKeepsStoredKey(t *testing.T) {
	isolateConfigDir(t)
	configMutex.Lock()
	configStore = &config.Config{
		Printers: aiPrinters(),
		AI:       &config.AI{Enabled: true, Preset: "openai", Model: "old", APIKey: "sk-keep-me"},
	}
	configMutex.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/ai/settings", strings.NewReader(`{"enabled":true,"preset":"openai","model":"new"}`))
	handleAISettings(httptest.NewRecorder(), req)

	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore.AI.APIKey != "sk-keep-me" {
		t.Errorf("APIKey = %q, want it kept", configStore.AI.APIKey)
	}
	if configStore.AI.Model != "new" {
		t.Errorf("Model = %q, want %q", configStore.AI.Model, "new")
	}
}

// An explicit empty string is the only way to clear the key.
func TestHandleAISettings_ExplicitEmptyKeyClearsIt(t *testing.T) {
	isolateConfigDir(t)
	configMutex.Lock()
	configStore = &config.Config{Printers: aiPrinters(), AI: &config.AI{APIKey: "sk-old"}}
	configMutex.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/ai/settings", strings.NewReader(`{"preset":"openai","model":"m","api_key":""}`))
	handleAISettings(httptest.NewRecorder(), req)

	configMutex.RLock()
	defer configMutex.RUnlock()
	if configStore.AI.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", configStore.AI.APIKey)
	}
}

func TestHandleAISettings_RejectsUnknownPreset(t *testing.T) {
	isolateConfigDir(t)
	configMutex.Lock()
	configStore = &config.Config{Printers: aiPrinters()}
	configMutex.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/ai/settings", strings.NewReader(`{"preset":"definitely-not-a-provider","model":"m"}`))
	rec := httptest.NewRecorder()
	handleAISettings(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// The other half of the config-compat rule: a Settings save through
// /api/config must not disturb the assistant block, key included.
func TestResolveConfigUpdate_PreservesAISettings(t *testing.T) {
	old := &config.Config{
		APIKey:   "fox",
		Printers: aiPrinters(),
		AI:       &config.AI{Enabled: true, Preset: "openai", Model: "gpt-4o-mini", APIKey: "sk-secret", AllowCamera: true},
	}
	got, err := resolveConfigUpdate(old, []byte(`{"api_key":"newfox","auto_update":true}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AI == nil {
		t.Fatal("AI block was dropped by a partial config save")
	}
	if got.AI.APIKey != "sk-secret" {
		t.Errorf("AI.APIKey = %q, want it preserved", got.AI.APIKey)
	}
	if !got.AI.AllowCamera {
		t.Error("AI.AllowCamera was lost")
	}
	if len(got.Printers) != 2 {
		t.Errorf("printers = %d, want 2", len(got.Printers))
	}
}

// The dashboard round-trips the redacted config through /api/config. That view
// has no api_key field, so the echo must not blank the stored one.
func TestApplyStoredSecrets_RedactedAIEchoKeepsKey(t *testing.T) {
	old := &config.Config{AI: &config.AI{Preset: "openai", Model: "m", APIKey: "sk-secret"}}
	incoming := &config.Config{AI: &config.AI{Preset: "openai", Model: "m"}} // no key, as the UI sends
	applyStoredSecrets(incoming, old)
	if incoming.AI.APIKey != "sk-secret" {
		t.Errorf("APIKey = %q, want it restored from storage", incoming.AI.APIKey)
	}
}

func TestClone_DeepCopiesAISettings(t *testing.T) {
	original := &config.Config{AI: &config.AI{Preset: "openai", Model: "m", APIKey: "sk"}}
	clone := original.Clone()
	clone.AI.APIKey = "changed"
	if original.AI.APIKey != "sk" {
		t.Error("Clone shares the AI block with the original")
	}
}

func TestAIConfigured_RequiresModelAndProvider(t *testing.T) {
	cases := []struct {
		name string
		ai   *config.AI
		want bool
	}{
		{"nil", nil, false},
		{"disabled", &config.AI{Preset: "openai", Model: "m", APIKey: "k"}, false},
		{"no model", &config.AI{Enabled: true, Preset: "openai", APIKey: "k"}, false},
		{"hosted without key", &config.AI{Enabled: true, Preset: "openai", Model: "m"}, false},
		{"hosted complete", &config.AI{Enabled: true, Preset: "openai", Model: "m", APIKey: "k"}, true},
		{"local needs no key", &config.AI{Enabled: true, Preset: "local", Model: "m", BaseURL: "http://127.0.0.1:11434/v1"}, true},
		{"local without base url", &config.AI{Enabled: true, Preset: "local", Model: "m"}, false},
	}
	for _, tc := range cases {
		if got := tc.ai.Configured(); got != tc.want {
			t.Errorf("%s: Configured() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHandleAIChat_RefusesWhenNotConfigured(t *testing.T) {
	configMutex.Lock()
	configStore = &config.Config{Printers: aiPrinters()}
	configMutex.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/ai/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	handleAIChat(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412", rec.Code)
	}
}

// --- tool executor ---------------------------------------------------------

// toolValue runs a tool and returns its result as a generic map.
func toolValue(t *testing.T, name, args string) map[string]any {
	t.Helper()
	res, err := executeAITool(context.Background(), name, args)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", name, err)
	}
	encoded, err := json.Marshal(res.Value)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	return out
}

func TestExecuteAITool_UnknownToolIsAResultNotAnError(t *testing.T) {
	got := toolValue(t, "delete_everything", `{}`)
	if got["error"] != "unknown_tool" {
		t.Errorf("error = %v, want unknown_tool", got["error"])
	}
}

func TestExecuteAITool_BadArgumentsAreAResultNotAnError(t *testing.T) {
	got := toolValue(t, "get_printer_status", `{not json`)
	if got["error"] != "bad_arguments" {
		t.Errorf("error = %v, want bad_arguments", got["error"])
	}
}

func TestExecuteAITool_UnknownPrinterIsAResultNotAnError(t *testing.T) {
	configMutex.Lock()
	configStore = &config.Config{Printers: aiPrinters()}
	configMutex.Unlock()

	for _, tool := range []string{"get_printer_status", "test_printer_connection"} {
		got := toolValue(t, tool, `{"printer_name":"Imaginary"}`)
		if got["error"] != "unknown_printer" {
			t.Errorf("%s: error = %v, want unknown_printer", tool, got["error"])
		}
	}
}

func TestExecuteAITool_ListPrintersReportsKind(t *testing.T) {
	configMutex.Lock()
	configStore = &config.Config{Printers: aiPrinters()}
	configMutex.Unlock()

	got := toolValue(t, "list_printers", `{}`)
	rows, ok := got["printers"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("printers = %v, want 2 rows", got["printers"])
	}
	kinds := map[string]string{}
	for _, r := range rows {
		row := r.(map[string]any)
		kinds[row["name"].(string)] = row["kind"].(string)
	}
	if kinds["Monsieur"] != "bambu-lan" {
		t.Errorf("Monsieur kind = %q, want bambu-lan", kinds["Monsieur"])
	}
	if kinds["Sherlock"] != "klipper" {
		t.Errorf("Sherlock kind = %q, want klipper", kinds["Sherlock"])
	}
}

// The camera is off by default, and the refusal has to be a readable result so
// the model can tell the user where the switch is.
func TestExecuteAITool_CameraRefusedWhenNotAllowed(t *testing.T) {
	configMutex.Lock()
	configStore = &config.Config{Printers: aiPrinters(), AI: &config.AI{Enabled: true, AllowCamera: false}}
	configMutex.Unlock()

	got := toolValue(t, "view_printer_camera", `{"printer_name":"Monsieur"}`)
	if got["error"] != "not_permitted" {
		t.Errorf("error = %v, want not_permitted", got["error"])
	}
}

func TestToolsFor_CameraToolOnlyOfferedWhenAllowed(t *testing.T) {
	has := func(tools []ai.Tool, name string) bool {
		for _, tool := range tools {
			if tool.Function.Name == name {
				return true
			}
		}
		return false
	}
	if has(ai.ToolsFor(false), "view_printer_camera") {
		t.Error("camera tool offered with the permission off")
	}
	if !has(ai.ToolsFor(true), "view_printer_camera") {
		t.Error("camera tool missing with the permission on")
	}
}

// A log line can carry a LAN access code or an API key verbatim. None of them
// may reach the model.
func TestToolRecentLogs_RedactsStoredSecrets(t *testing.T) {
	configMutex.Lock()
	configStore = &config.Config{
		APIKey:   "fox-token-abcdef",
		Printers: []config.Printer{{Name: "Monsieur", LANCode: "81f1aafd"}},
		AI:       &config.AI{APIKey: "sk-provider-key-1234"},
	}
	configMutex.Unlock()

	logBufMu.Lock()
	logBuf = []string{
		"[Monsieur] connect failed with code 81f1aafd",
		"[relay] rejected token fox-token-abcdef",
		"[assistant] provider key sk-provider-key-1234 refused",
	}
	logBufMu.Unlock()
	t.Cleanup(func() {
		logBufMu.Lock()
		logBuf = nil
		logBufMu.Unlock()
	})

	got := toolRecentLogs("", 40)
	encoded, _ := json.Marshal(got)
	for _, secret := range []string{"81f1aafd", "fox-token-abcdef", "sk-provider-key-1234"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("log output leaked %q: %s", secret, encoded)
		}
	}
	if !strings.Contains(string(encoded), "[redacted]") {
		t.Error("nothing was redacted at all")
	}
}

func TestToolRecentLogs_FiltersByPrinter(t *testing.T) {
	configMutex.Lock()
	configStore = &config.Config{Printers: aiPrinters()}
	configMutex.Unlock()

	logBufMu.Lock()
	logBuf = []string{"[Monsieur] hello", "[Sherlock] hello", "[Monsieur] again"}
	logBufMu.Unlock()
	t.Cleanup(func() {
		logBufMu.Lock()
		logBuf = nil
		logBufMu.Unlock()
	})

	got := toolRecentLogs("Monsieur", 40)
	lines := got["lines"].([]string)
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %v", len(lines), lines)
	}
}

// The help library is the assistant's only source for how the Bridge works, so
// an empty or unsearchable one is a silent failure.
func TestHelpLibrary_AnswersTheCommonSymptoms(t *testing.T) {
	cases := map[string]string{
		"my printer is offline":        "connection-problems",
		"how do I add a bambu printer": "add-bambu-lan",
		"moonraker url":                "add-klipper",
		"where is config.json":         "config-files",
	}
	for query, wantID := range cases {
		hits := ai.SearchHelp(query)
		if len(hits) == 0 {
			t.Errorf("%q: no help found", query)
			continue
		}
		if hits[0].ID != wantID {
			t.Errorf("%q: first hit = %q, want %q", query, hits[0].ID, wantID)
		}
	}
}

func TestSystemPrompt_ForbidsGuessingPrinterErrorCodes(t *testing.T) {
	// The one rule with no judgement call in it: a guessed error code sends
	// someone to take a working printer apart.
	for _, want := range []string{"Never guess what a code means", "read-only", "Never invent a printer"} {
		if !strings.Contains(ai.SystemPrompt, want) {
			t.Errorf("system prompt no longer says %q", want)
		}
	}
}
