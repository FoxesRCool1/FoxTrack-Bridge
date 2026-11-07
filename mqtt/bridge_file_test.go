package mqtt

import (
	"sync"
	"testing"
	"time"
)

// deleteCalls records what the stubbed ftpsDelete was asked to remove.
type deleteCalls struct {
	mu    sync.Mutex
	calls []string
}

func (d *deleteCalls) add(host, accessCode, remoteName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, host+" "+accessCode+" "+remoteName)
	return nil
}

func (d *deleteCalls) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func stubBridgeFileRemoval(t *testing.T, wait time.Duration) *deleteCalls {
	t.Helper()
	d := &deleteCalls{}
	oldDelete, oldWait := ftpsDelete, bridgeFileWait
	ftpsDelete = d.add
	bridgeFileWait = wait
	t.Cleanup(func() { ftpsDelete, bridgeFileWait = oldDelete, oldWait })
	return d
}

func TestBridgePrintEnd_RemovesStoredFileOnce(t *testing.T) {
	const name = "bridge-del-finish"
	d := stubBridgeFileRemoval(t, 10*time.Millisecond)
	_, p := setupSkip(t, name, "idle")
	noteBridgeJob(p, "FoxTrack print")
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING","gcode_file":"Metadata/plate_1.gcode"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH"}}`))

	deadline := time.Now().Add(2 * time.Second)
	for len(d.list()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	got := d.list()
	want := []string{p.IP + " " + p.LANCode + " " + printerStoredName}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("delete calls = %v, want %v", got, want)
	}
}

func TestNonBridgePrintEnd_NeverDeletes(t *testing.T) {
	const name = "bridge-del-user"
	d := stubBridgeFileRemoval(t, 10*time.Millisecond)
	setupSkip(t, name, "idle")
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING","gcode_file":"Metadata/plate_1.gcode"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH"}}`))
	time.Sleep(100 * time.Millisecond)
	if got := d.list(); len(got) != 0 {
		t.Fatalf("delete calls = %v, want none", got)
	}
}

func TestBridgePrintEnd_SkipsDeleteWhenPrintingAgain(t *testing.T) {
	const name = "bridge-del-again"
	d := stubBridgeFileRemoval(t, 50*time.Millisecond)
	_, p := setupSkip(t, name, "idle")
	noteBridgeJob(p, "FoxTrack print")
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING","gcode_file":"Metadata/plate_1.gcode"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH"}}`))
	UpdatePrinterState(name, TelemetryData{Status: "printing"})
	time.Sleep(200 * time.Millisecond)
	if got := d.list(); len(got) != 0 {
		t.Fatalf("delete calls = %v, want none while printing again", got)
	}
}

func TestBridgePrintEnd_SkipsDeleteWhenNewUploadStarted(t *testing.T) {
	const name = "bridge-del-upload"
	d := stubBridgeFileRemoval(t, 50*time.Millisecond)
	_, p := setupSkip(t, name, "idle")
	noteBridgeJob(p, "FoxTrack print")
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"RUNNING","gcode_file":"Metadata/plate_1.gcode"}}`))
	noteSkipFields(name, []byte(`{"print":{"gcode_state":"FINISH"}}`))
	noteBridgeUpload(name) // a queued next print starts uploading while the printer is still idle
	time.Sleep(200 * time.Millisecond)
	if got := d.list(); len(got) != 0 {
		t.Fatalf("delete calls = %v, want none while a new upload is under way", got)
	}
}
