package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"foxtrack-bridge/version"
)

const (
	repoOwner = "FoxesRCool1"
	repoName  = "FoxTrack-Bridge"
)

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type CheckResult struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	Available      bool   `json:"available"`
	PendingRestart bool   `json:"pendingRestart"`
	ReleaseURL     string `json:"releaseUrl"`
	CanAutoInstall bool   `json:"canAutoInstall"`
	AssetName      string `json:"assetName,omitempty"`
	Notes          string `json:"notes,omitempty"`
	DevBuild       bool   `json:"devBuild"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseResponse struct {
	TagName    string         `json:"tag_name"`
	HTMLURL    string         `json:"html_url"`
	Body       string         `json:"body"`
	Prerelease bool           `json:"prerelease"`
	Draft      bool           `json:"draft"`
	Assets     []releaseAsset `json:"assets"`
}

// stagedUpdate is a downloaded update waiting to be applied. Exactly one of
// scriptPath and binaryPath is set: Windows and macOS hand off to a helper
// script, Linux swaps the binary in-process — see applyLinuxUpdate.
type stagedUpdate struct {
	scriptPath string
	binaryPath string
	exePath    string
	version    string
	stagedAt   time.Time
}

var (
	cacheMu      sync.Mutex
	cachedAt     time.Time
	cachedResult CheckResult

	installMu         sync.Mutex
	installInProgress bool

	pendingMu     sync.Mutex
	pendingUpdate *stagedUpdate
)

// CanReplaceExecutable reports whether the directory containing the running
// binary is writable, i.e. whether a staged update can be moved into place.
// Inside Docker (or any install owned by another user) this is false and
// self-update cannot work.
func CanReplaceExecutable() bool {
	exePath, err := os.Executable()
	if err != nil {
		return false
	}
	f, err := os.CreateTemp(filepath.Dir(exePath), ".foxtrack-writecheck-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func CheckLatest(ctx context.Context) (CheckResult, error) {
	if !version.IsValid(version.AppVersion) {
		return CheckResult{
			CurrentVersion: version.AppVersion,
			DevBuild:       true,
			Notes:          "development build — update checks are disabled",
		}, nil
	}

	cacheMu.Lock()
	if time.Since(cachedAt) < 15*time.Minute && !cachedAt.IsZero() {
		res := cachedResult
		cacheMu.Unlock()
		return res, nil
	}
	cacheMu.Unlock()

	current := version.AppVersion
	res := CheckResult{CurrentVersion: current}

	rel, err := fetchRelease(ctx)
	if err != nil {
		return res, err
	}
	asset, canInstall := pickAsset(rel.Assets)

	res.LatestVersion = rel.TagName
	res.ReleaseURL = rel.HTMLURL
	res.Available = version.Compare(current, rel.TagName) < 0
	res.PendingRestart = HasPendingRestart()
	res.CanAutoInstall = canInstall && CanReplaceExecutable()
	res.AssetName = asset.Name
	res.Notes = rel.Body

	cacheMu.Lock()
	cachedResult = res
	cachedAt = time.Now()
	cacheMu.Unlock()

	return res, nil
}

func fetchRelease(ctx context.Context) (*releaseResponse, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "FoxTrack-Bridge-Updater")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("github release check failed: %s (%s)", resp.Status, strings.TrimSpace(string(b)))
	}

	var out releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Draft || out.TagName == "" {
		return nil, fmt.Errorf("no valid stable release found")
	}
	return &out, nil
}

func pickAsset(assets []releaseAsset) (Asset, bool) {
	return pickAssetFor(assets, runtime.GOOS, runtime.GOARCH, version.AppBuildVariant)
}

func pickAssetFor(assets []releaseAsset, goos, goarch, buildVariant string) (Asset, bool) {
	headless := buildVariant == "headless"
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		switch goos {
		case "windows":
			if goarch == "amd64" && strings.Contains(name, "windows") && strings.HasSuffix(name, ".exe") {
				isHeadlessAsset := strings.Contains(name, "headless")
				if headless == isHeadlessAsset {
					return Asset{Name: a.Name, URL: a.BrowserDownloadURL}, true
				}
			}
		case "linux":
			if goarch == "arm64" && strings.Contains(name, "linux") && strings.Contains(name, "arm64") {
				return Asset{Name: a.Name, URL: a.BrowserDownloadURL}, true
			}
			if goarch == "arm" && strings.Contains(name, "linux") && strings.Contains(name, "arm32") {
				return Asset{Name: a.Name, URL: a.BrowserDownloadURL}, true
			}
			if goarch == "amd64" && strings.Contains(name, "linux") && !strings.Contains(name, "arm") && !strings.HasSuffix(name, ".zip") {
				return Asset{Name: a.Name, URL: a.BrowserDownloadURL}, true
			}
		case "darwin":
			if goarch == "arm64" && strings.Contains(name, "macos") && strings.Contains(name, "apple-silicon") {
				isHeadlessAsset := !strings.HasSuffix(name, ".zip")
				if headless == isHeadlessAsset {
					return Asset{Name: a.Name, URL: a.BrowserDownloadURL}, true
				}
			}
			if goarch == "amd64" && strings.Contains(name, "macos") && strings.Contains(name, "intel") {
				isHeadlessAsset := !strings.HasSuffix(name, ".zip")
				if headless == isHeadlessAsset {
					return Asset{Name: a.Name, URL: a.BrowserDownloadURL}, true
				}
			}
		}
	}
	return Asset{}, false
}

func StartInstall(ctx context.Context) error {
	if !version.IsValid(version.AppVersion) {
		return fmt.Errorf("development build (%s) — update checks are disabled", version.AppVersion)
	}

	if !CanReplaceExecutable() {
		return fmt.Errorf("the binary location is read-only, update by pulling a new image or replacing the binary")
	}

	pendingMu.Lock()
	if pendingUpdate != nil {
		pendingMu.Unlock()
		return fmt.Errorf("an update is already staged, restart to apply it")
	}
	pendingMu.Unlock()

	installMu.Lock()
	if installInProgress {
		installMu.Unlock()
		return fmt.Errorf("update already in progress")
	}
	installInProgress = true
	installMu.Unlock()
	defer func() {
		installMu.Lock()
		installInProgress = false
		installMu.Unlock()
	}()

	rel, err := fetchRelease(ctx)
	if err != nil {
		return err
	}
	if version.Compare(version.AppVersion, rel.TagName) >= 0 {
		return fmt.Errorf("already up to date")
	}
	asset, ok := pickAsset(rel.Assets)
	if !ok {
		return fmt.Errorf("no compatible update asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	checksumAsset, hasChecksum := pickChecksumAsset(rel.Assets)
	return downloadAndStage(ctx, asset, checksumAsset, hasChecksum, rel.TagName)
}

func downloadAndStage(ctx context.Context, asset Asset, checksumAsset Asset, hasChecksum bool, latestVersion string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "FoxTrack-Bridge-Updater")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s", resp.Status)
	}

	tmpDir, err := os.MkdirTemp("", "foxtrack-update-")
	if err != nil {
		return err
	}
	// On Windows and macOS the apply script and the downloaded payload both live
	// in tmpDir and must outlive this process on the success path, so the
	// directory is only removed when staging fails. Without this every failed
	// attempt (a checksum mismatch, a truncated download) left the whole payload
	// on disk forever. Linux stages the new binary next to the executable and
	// needs nothing from tmpDir afterwards.
	keepTmpDir := false
	defer func() {
		if !keepTmpDir {
			os.RemoveAll(tmpDir)
		}
	}()

	tmpFile := filepath.Join(tmpDir, asset.Name)
	f, err := os.Create(tmpFile)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if hasChecksum {
		checksums, err := downloadChecksums(ctx, checksumAsset.URL)
		if err != nil {
			return err
		}
		expected, ok := checksums[strings.ToLower(asset.Name)]
		if !ok {
			return fmt.Errorf("checksum for %s not found in checksum asset", asset.Name)
		}
		ok, got, err := verifySHA256File(tmpFile, expected)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", asset.Name, expected, got)
		}
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	var scriptPath, binaryPath string
	switch runtime.GOOS {
	case "windows":
		scriptPath, err = stageWindowsUpdate(tmpDir, tmpFile, exePath)
	case "linux":
		binaryPath, err = stageLinuxUpdate(tmpFile, exePath)
	case "darwin":
		scriptPath, err = stageDarwinUpdate(tmpDir, tmpFile, exePath)
	default:
		return fmt.Errorf("auto update is not supported on %s", runtime.GOOS)
	}
	if err != nil {
		return err
	}

	keepTmpDir = scriptPath != ""

	pendingMu.Lock()
	pendingUpdate = &stagedUpdate{
		scriptPath: scriptPath,
		binaryPath: binaryPath,
		exePath:    exePath,
		version:    latestVersion,
		stagedAt:   time.Now(),
	}
	pendingMu.Unlock()

	cacheMu.Lock()
	cachedAt = time.Time{}
	cacheMu.Unlock()

	return nil
}

func stageWindowsUpdate(tmpDir, downloadPath, exePath string) (string, error) {
	scriptPath := filepath.Join(tmpDir, "apply-update.bat")
	// Mirror the Linux path: land the payload next to the target, then rename it
	// over the old binary. A plain copy onto a running exe can fail halfway and
	// leave a truncated binary, and the old script ignored the failure and
	// relaunched the stale build as if the update had worked.
	stagedPath := exePath + ".update"
	script := "@echo off\r\n" +
		"ping 127.0.0.1 -n 3 > nul\r\n" +
		fmt.Sprintf("copy /Y \"%s\" \"%s\" > nul\r\n", downloadPath, stagedPath) +
		"if errorlevel 1 exit /b 1\r\n" +
		fmt.Sprintf("move /Y \"%s\" \"%s\" > nul\r\n", stagedPath, exePath) +
		"if errorlevel 1 exit /b 1\r\n" +
		fmt.Sprintf("start \"\" \"%s\"\r\n", exePath)
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		return "", err
	}
	return scriptPath, nil
}

// stageLinuxUpdate copies the downloaded binary next to the running executable
// so applyLinuxUpdate can rename it into place. It writes no helper script —
// see applyLinuxUpdate for why.
func stageLinuxUpdate(downloadPath, exePath string) (string, error) {
	stagedPath := exePath + ".update"
	if err := copyFile(downloadPath, stagedPath, 0o755); err != nil {
		return "", err
	}
	return stagedPath, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	// The mode passed to OpenFile is masked by the umask, so set it explicitly.
	if err := os.Chmod(dst, mode); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

func stageDarwinUpdate(tmpDir, downloadPath, exePath string) (string, error) {
	extractDir := filepath.Join(tmpDir, "extracted")
	if err := unzip(downloadPath, extractDir); err != nil {
		return "", err
	}

	appPath, err := currentAppBundlePath(exePath)
	if err != nil {
		return "", fmt.Errorf("could not resolve app bundle path: %w", err)
	}
	newApp, err := findAppBundle(extractDir)
	if err != nil {
		return "", err
	}

	scriptPath := filepath.Join(tmpDir, "apply-update.sh")
	// Copy the new bundle in beside the old one first, and only remove the
	// installed app once that copy has succeeded. Deleting first meant a failed
	// copy (no disk space, a bad archive) left the user with no app at all.
	incoming := appPath + ".incoming"
	script := "#!/bin/sh\nset -e\n" +
		"sleep 1\n" +
		fmt.Sprintf("rm -rf \"%s\"\n", incoming) +
		fmt.Sprintf("cp -R \"%s\" \"%s\"\n", newApp, incoming) +
		fmt.Sprintf("rm -rf \"%s\"\n", appPath) +
		fmt.Sprintf("mv \"%s\" \"%s\"\n", incoming, appPath) +
		fmt.Sprintf("open \"%s\"\n", appPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return "", err
	}
	return scriptPath, nil
}

func HasPendingRestart() bool {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	return pendingUpdate != nil
}

func RestartToApply() error {
	pendingMu.Lock()
	pending := pendingUpdate
	if pending != nil {
		pendingUpdate = nil
	}
	pendingMu.Unlock()

	if pending == nil {
		return fmt.Errorf("no staged update pending restart")
	}

	cacheMu.Lock()
	cachedAt = time.Time{}
	cacheMu.Unlock()

	switch runtime.GOOS {
	case "linux":
		return applyLinuxUpdate(pending)
	case "windows":
		cmd := exec.Command("cmd", "/C", "start", "", "/B", pending.scriptPath)
		return cmd.Start()
	default:
		cmd := exec.Command("sh", pending.scriptPath)
		return cmd.Start()
	}
}

// applyLinuxUpdate swaps the staged binary into place and, only when nothing
// else will restart the bridge, relaunches it.
//
// The swap happens here instead of in a helper script because the old helper
// was started as a child of this process. Under systemd it therefore lived in
// the service cgroup, and the default KillMode=control-group killed it along
// with the service — while it was still waiting for this pid to exit, before
// it could move the new binary into place. The bridge came back on the old
// version, or with Restart=on-failure and a clean exit did not come back at
// all. rename(2) over a running executable is legal on Linux, so the swap can
// simply happen before this process exits and no helper is needed.
func applyLinuxUpdate(pending *stagedUpdate) error {
	if err := os.Rename(pending.binaryPath, pending.exePath); err != nil {
		os.Remove(pending.binaryPath)
		return fmt.Errorf("install %s: %w", pending.version, err)
	}
	if supervisorRestarts() {
		return nil
	}
	return relaunchDetached(pending.exePath)
}

// supervisorRestarts reports whether a service manager will start the bridge
// again after a clean exit. systemd sets INVOCATION_ID for every unit it
// starts; relaunching ourselves there would leave a second, unsupervised copy
// racing the one systemd starts for the same port.
func supervisorRestarts() bool {
	return strings.TrimSpace(os.Getenv("INVOCATION_ID")) != ""
}

func pickChecksumAsset(assets []releaseAsset) (Asset, bool) {
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if strings.Contains(name, "checksum") || strings.Contains(name, "sha256") {
			if strings.HasSuffix(name, ".txt") || strings.HasSuffix(name, ".sha256") || strings.HasSuffix(name, ".sha256sum") {
				return Asset{Name: a.Name, URL: a.BrowserDownloadURL}, true
			}
		}
	}
	return Asset{}, false
}

func downloadChecksums(ctx context.Context, url string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "FoxTrack-Bridge-Updater")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checksum download failed: %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}

	return parseChecksumText(string(body)), nil
}

func parseChecksumText(text string) map[string]string {
	lines := strings.Split(text, "\n")
	out := make(map[string]string)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hash := strings.TrimSpace(fields[0])
		name := strings.TrimLeft(strings.TrimSpace(fields[len(fields)-1]), "*./")
		if len(hash) == 64 {
			out[strings.ToLower(name)] = strings.ToLower(hash)
		}
	}

	if len(out) == 0 {
		return out
	}

	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	canonical := make(map[string]string, len(out))
	for _, k := range keys {
		canonical[k] = out[k]
	}
	return canonical
}

func verifySHA256File(path, expected string) (bool, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, "", err
	}
	actual := strings.ToLower(hex.EncodeToString(h.Sum(nil)))
	return actual == strings.ToLower(expected), actual, nil
}

func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		cleanName := filepath.Clean(f.Name)
		target := filepath.Join(dst, cleanName)
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid zip path: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(w, rc); err != nil {
			w.Close()
			rc.Close()
			return err
		}
		w.Close()
		rc.Close()
	}
	return nil
}

func currentAppBundlePath(exePath string) (string, error) {
	parts := strings.Split(exePath, ".app")
	if len(parts) < 2 {
		return "", fmt.Errorf("executable is not running inside an .app bundle")
	}
	return parts[0] + ".app", nil
}

func findAppBundle(root string) (string, error) {
	var appPath string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".app") {
			appPath = path
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if appPath == "" {
		return "", fmt.Errorf("no .app bundle found in downloaded update")
	}
	return appPath, nil
}
