//go:build linux

package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const restartDropInName = "10-restart-always.conf"

const restartDropInBody = `# Written by FoxTrack Bridge.
#
# The update path exits 0 on purpose so the replacement binary takes over.
# Units shipped before v2.3.1 used Restart=on-failure, which systemd reads as a
# deliberate stop — so the bridge stayed down after an update instead of coming
# back on the new version.
#
# Delete this file and run "systemctl --user daemon-reload" to go back to
# whatever the unit file itself says.
[Service]
Restart=always
`

// EnsureRestartAlways makes sure the systemd user unit running this bridge
// restarts it after a clean exit, so an update can hand over to the new binary.
//
// It writes a drop-in beside the unit instead of editing the unit itself: the
// user's file is never touched, and the override can be removed at any time.
// Nothing here is fatal — a bridge that cannot fix its unit still runs.
//
// The returned string says what was done, and is empty when there was nothing
// to do.
func EnsureRestartAlways() (string, error) {
	unit := currentSystemdUnit()
	if unit == "" {
		return "", nil // not running as a systemd unit
	}
	if strings.EqualFold(systemctlShow(unit, "Restart"), "always") {
		return "", nil // already correct
	}

	fragment := systemctlShow(unit, "FragmentPath")
	if fragment == "" {
		return "", fmt.Errorf("could not locate the unit file for %s", unit)
	}

	// Only ever write inside the user's own systemd directory. A unit under
	// /etc or /usr belongs to the system and is not ours to override.
	ownDir, err := userSystemdDir()
	if err != nil {
		return "", err
	}
	if filepath.Dir(fragment) != ownDir {
		return "", fmt.Errorf("unit %s lives at %s, outside %s — set Restart=always by hand", unit, fragment, ownDir)
	}

	dropInDir := fragment + ".d"
	if err := os.MkdirAll(dropInDir, 0o755); err != nil {
		return "", err
	}
	dropIn := filepath.Join(dropInDir, restartDropInName)
	if err := writeFileAtomic(dropIn, []byte(restartDropInBody), 0o644); err != nil {
		return "", err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return "", fmt.Errorf("daemon-reload after writing %s: %w (%s)", dropIn, err, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("set Restart=always for %s via %s", unit, dropIn), nil
}

// currentSystemdUnit returns the unit this process runs under, or "" when it is
// not running as one. The unit name is the last component of the cgroup path.
func currentSystemdUnit() string {
	if strings.TrimSpace(os.Getenv("INVOCATION_ID")) == "" {
		return ""
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	return unitFromCgroup(string(b))
}

func unitFromCgroup(cgroup string) string {
	for _, line := range strings.Split(cgroup, "\n") {
		path := strings.TrimRight(strings.TrimSpace(line), "/")
		name := path[strings.LastIndex(path, "/")+1:]
		if strings.HasSuffix(name, ".service") {
			return name
		}
	}
	return ""
}

func systemctlShow(unit, property string) string {
	out, err := exec.Command("systemctl", "--user", "show", unit, "-p", property, "--value").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func userSystemdDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "systemd", "user"), nil
}

// writeFileAtomic writes through a temp file in the same directory so a crash
// or a full disk can never leave a half-written drop-in for systemd to parse.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".foxtrack-dropin-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, path)
}
