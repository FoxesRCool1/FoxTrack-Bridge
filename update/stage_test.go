package update

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The apply step must never destroy the installed binary or bundle before a
// verified replacement is in place: a failure at that point leaves the user
// with nothing to run. On Linux staging only writes a sibling file; the
// running binary is untouched until applyLinuxUpdate renames over it.
func TestStageLinuxUpdate_StagesSiblingAndLeavesBinaryAlone(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "foxtrack-bridge")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, "payload")
	if err := os.WriteFile(payload, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged, err := stageLinuxUpdate(payload, exe)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if staged != exe+".update" {
		t.Errorf("staged at %q, want the sibling path %q", staged, exe+".update")
	}
	if got := readFileString(t, staged); got != "new" {
		t.Errorf("staged binary = %q, want %q", got, "new")
	}
	fi, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("staged binary is not executable: %v", fi.Mode())
	}
	if got := readFileString(t, exe); got != "old" {
		t.Errorf("staging modified the running binary: %q", got)
	}
}

func TestStageDarwinUpdate_CopiesBeforeRemoving(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "FoxTrack Bridge.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(app, "Contents", "MacOS", "foxtrack-bridge")
	if err := os.WriteFile(exe, []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	// stageDarwinUpdate unzips first, so hand it a real (if tiny) zip holding a
	// bundle — see zipAppBundle below.
	payload := zipAppBundle(t, dir)

	path, err := stageDarwinUpdate(dir, payload, exe)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	script := readScript(t, path)
	// The destructive rm of the installed app must come after the copy.
	mustOrder(t, "darwin", script, `.incoming"`, `rm -rf "`+app+`"`)
	if !strings.Contains(script, `mv "`+app+`.incoming" "`+app+`"`) {
		t.Errorf("darwin script does not swap the staged bundle into place:\n%s", script)
	}
}

func TestStageWindowsUpdate_StagesAndChecksErrors(t *testing.T) {
	dir := t.TempDir()
	exe := `C:\Program Files\FoxTrack\foxtrack-bridge.exe`
	path, err := stageWindowsUpdate(dir, filepath.Join(dir, "payload.exe"), exe)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	script := readScript(t, path)
	if !strings.Contains(script, exe+".update") {
		t.Error("windows script does not stage through a sibling path")
	}
	if !strings.Contains(script, "if errorlevel 1 exit /b 1") {
		t.Error("windows script does not stop on a failed replace")
	}
	mustOrder(t, "windows", script, "copy /Y", "move /Y", "start ")
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func readScript(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return string(b)
}

// mustOrder asserts each needle appears in the script, in the given order.
func mustOrder(t *testing.T, name, script string, needles ...string) {
	t.Helper()
	at := -1
	for _, n := range needles {
		i := strings.Index(script, n)
		if i < 0 {
			t.Fatalf("%s script is missing %q:\n%s", name, n, script)
		}
		if i < at {
			t.Fatalf("%s script has %q out of order:\n%s", name, n, script)
		}
		at = i
	}
}

// zipAppBundle writes a minimal zip holding a FoxTrack Bridge.app directory,
// which is what stageDarwinUpdate expects to unpack.
func zipAppBundle(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "payload.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, err := zw.Create("FoxTrack Bridge.app/Contents/MacOS/foxtrack-bridge")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new binary")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
