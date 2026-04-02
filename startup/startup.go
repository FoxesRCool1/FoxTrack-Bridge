// Package startup handles registering FoxTrack Bridge to run at system login.
package startup

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)
	switch runtime.GOOS {
	case "windows":
		return enableWindows(exe)
	case "darwin":
		return enableMacOS(exe)
	default:
		return enableLinux(exe)
	}
}

func Disable() error {
	switch runtime.GOOS {
	case "windows":
		return disableWindows()
	case "darwin":
		return disableMacOS()
	default:
		return disableLinux()
	}
}

func IsEnabled() bool {
	switch runtime.GOOS {
	case "windows":
		return isEnabledWindows()
	case "darwin":
		return isEnabledMacOS()
	default:
		return isEnabledLinux()
	}
}

// ── Linux (XDG autostart) ────────────────────────────────────────────────────

func autostartDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "autostart")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "autostart")
}

func desktopFilePath() string {
	return filepath.Join(autostartDir(), "foxtrack-bridge.desktop")
}

func enableLinux(exe string) error {
	if err := os.MkdirAll(autostartDir(), 0755); err != nil {
		return err
	}
	content := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=FoxTrack Bridge\nExec=%s\nHidden=false\nNoDisplay=false\nX-GNOME-Autostart-enabled=true\n", exe)
	return os.WriteFile(desktopFilePath(), []byte(content), 0644)
}

func disableLinux() error {
	err := os.Remove(desktopFilePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func isEnabledLinux() bool {
	_, err := os.Stat(desktopFilePath())
	return err == nil
}

// ── macOS (LaunchAgent) ──────────────────────────────────────────────────────

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "com.foxtrack.bridge.plist")
}

func enableMacOS(exe string) error {
	if err := os.MkdirAll(filepath.Dir(plistPath()), 0755); err != nil {
		return err
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.foxtrack.bridge</string>
  <key>ProgramArguments</key><array><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><false/>
</dict></plist>`, exe)
	return os.WriteFile(plistPath(), []byte(content), 0644)
}

func disableMacOS() error {
	err := os.Remove(plistPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func isEnabledMacOS() bool {
	_, err := os.Stat(plistPath())
	return err == nil
}
