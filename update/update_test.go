package update

import "testing"

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
		{Name: "FoxTrack-Bridge-Windows-Arm64.exe", BrowserDownloadURL: "https://example/windows-arm"},
		{Name: "FoxTrack-Bridge-Linux", BrowserDownloadURL: "https://example/linux"},
		{Name: "FoxTrack-Bridge-macOS-Apple-Silicon.zip", BrowserDownloadURL: "https://example/mac-arm"},
		{Name: "FoxTrack-Bridge-macOS-Intel.zip", BrowserDownloadURL: "https://example/mac-intel"},
	}

	a, ok := pickAssetFor(assets, "linux", "amd64")
	if !ok || a.Name != "FoxTrack-Bridge-Linux" {
		t.Fatalf("linux pick failed: %+v, %v", a, ok)
	}

	a, ok = pickAssetFor(assets, "windows", "amd64")
	if !ok || a.Name != "FoxTrack-Bridge-Windows.exe" {
		t.Fatalf("windows amd64 pick failed: %+v, %v", a, ok)
	}

	a, ok = pickAssetFor(assets, "windows", "arm64")
	if !ok || a.Name != "FoxTrack-Bridge-Windows-Arm64.exe" {
		t.Fatalf("windows arm64 pick failed: %+v, %v", a, ok)
	}

	a, ok = pickAssetFor(assets, "darwin", "arm64")
	if !ok || a.Name != "FoxTrack-Bridge-macOS-Apple-Silicon.zip" {
		t.Fatalf("darwin arm64 pick failed: %+v, %v", a, ok)
	}
}
