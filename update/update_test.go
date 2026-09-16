package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"foxtrack-bridge/version"
)

func TestCheckLatestSkipsDevBuild(t *testing.T) {
	orig := version.AppVersion
	version.AppVersion = "dev"
	defer func() { version.AppVersion = orig }()

	res, err := CheckLatest(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.DevBuild {
		t.Fatalf("expected DevBuild=true for AppVersion=dev")
	}
	if res.Available {
		t.Fatalf("expected Available=false for a dev build")
	}
}

func TestStartInstallRefusesDevBuild(t *testing.T) {
	orig := version.AppVersion
	version.AppVersion = "dev"
	defer func() { version.AppVersion = orig }()

	if err := StartInstall(context.Background()); err == nil {
		t.Fatal("expected error for dev build")
	}
}

func TestParseChecksumText(t *testing.T) {
	body := "" +
		"f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0  FoxTrack-Bridge-Linux\n" +
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa *FoxTrack-Bridge-Windows.exe\n"

	got := parseChecksumText(body)
	if got["foxtrack-bridge-linux"] != "f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0" {
		t.Fatalf("missing linux checksum parse")
	}
	if got["foxtrack-bridge-windows.exe"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("missing windows checksum parse")
	}
}

func TestPickAssetFor(t *testing.T) {
	assets := []releaseAsset{
		{Name: "FoxTrack-Bridge-Windows.exe", BrowserDownloadURL: "https://example/windows"},
		{Name: "FoxTrack-Bridge-Windows-Headless.exe", BrowserDownloadURL: "https://example/windows-headless"},
		{Name: "FoxTrack-Bridge-Linux", BrowserDownloadURL: "https://example/linux"},
		{Name: "FoxTrack-Bridge-Linux-ARM64", BrowserDownloadURL: "https://example/linux-arm64"},
		{Name: "FoxTrack-Bridge-Linux-ARM32", BrowserDownloadURL: "https://example/linux-arm32"},
		{Name: "FoxTrack-Bridge-macOS-Apple-Silicon.zip", BrowserDownloadURL: "https://example/mac-arm"},
		{Name: "FoxTrack-Bridge-macOS-Apple-Silicon-Headless", BrowserDownloadURL: "https://example/mac-arm-headless"},
		{Name: "FoxTrack-Bridge-macOS-Intel.zip", BrowserDownloadURL: "https://example/mac-intel"},
		{Name: "FoxTrack-Bridge-macOS-Intel-Headless", BrowserDownloadURL: "https://example/mac-intel-headless"},
	}

	check := func(goos, goarch, variant, wantName string) {
		t.Helper()
		a, ok := pickAssetFor(assets, goos, goarch, variant)
		if !ok || a.Name != wantName {
			t.Fatalf("pickAssetFor(%q, %q, %q): got %q ok=%v, want %q", goos, goarch, variant, a.Name, ok, wantName)
		}
	}

	check("linux", "amd64", "", "FoxTrack-Bridge-Linux")
	check("linux", "arm64", "", "FoxTrack-Bridge-Linux-ARM64")
	check("linux", "arm", "", "FoxTrack-Bridge-Linux-ARM32")
	check("windows", "amd64", "", "FoxTrack-Bridge-Windows.exe")
	check("windows", "amd64", "headless", "FoxTrack-Bridge-Windows-Headless.exe")
	check("darwin", "arm64", "", "FoxTrack-Bridge-macOS-Apple-Silicon.zip")
	check("darwin", "arm64", "headless", "FoxTrack-Bridge-macOS-Apple-Silicon-Headless")
	check("darwin", "amd64", "", "FoxTrack-Bridge-macOS-Intel.zip")
	check("darwin", "amd64", "headless", "FoxTrack-Bridge-macOS-Intel-Headless")
}

// Regression: the Linux apply step used to run in a helper script spawned as a
// child of this process. Under systemd that script sat in the service cgroup
// and was killed with the service while it was still waiting for this pid to
// exit, so the binary was never swapped and the bridge came back on the old
// version. applyLinuxUpdate must do the swap itself, before the process exits.
func TestApplyLinuxUpdateSwapsBinaryWithoutHelper(t *testing.T) {
	t.Setenv("INVOCATION_ID", "0123456789abcdef")

	dir := t.TempDir()
	exe := filepath.Join(dir, "foxtrack-bridge")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := exe + ".update"
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	pending := &stagedUpdate{binaryPath: staged, exePath: exe, version: "9.9.9"}
	if err := applyLinuxUpdate(pending); err != nil {
		t.Fatalf("applyLinuxUpdate: %v", err)
	}

	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("installed binary = %q, want %q — the update did not apply", b, "new")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged file still present after apply (stat err %v)", err)
	}
}

// A failed swap must leave the installed binary runnable and not strand the
// staged copy next to it.
func TestApplyLinuxUpdateKeepsBinaryWhenSwapFails(t *testing.T) {
	t.Setenv("INVOCATION_ID", "0123456789abcdef")

	dir := t.TempDir()
	exe := filepath.Join(dir, "foxtrack-bridge")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Nothing was staged, so the rename cannot succeed.
	pending := &stagedUpdate{binaryPath: filepath.Join(dir, "missing.update"), exePath: exe, version: "9.9.9"}
	if err := applyLinuxUpdate(pending); err == nil {
		t.Fatal("expected an error when the staged binary is missing")
	}

	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "old" {
		t.Fatalf("installed binary = %q, want it left at %q", b, "old")
	}
}

// Under a service manager the bridge must not start a second copy of itself:
// systemd restarts the unit, and a detached relaunch would race it for the port.
func TestSupervisorRestartsDetectsSystemd(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	if supervisorRestarts() {
		t.Error("want supervisorRestarts=false when INVOCATION_ID is unset")
	}
	t.Setenv("INVOCATION_ID", "0123456789abcdef")
	if !supervisorRestarts() {
		t.Error("want supervisorRestarts=true when systemd set INVOCATION_ID")
	}
}
