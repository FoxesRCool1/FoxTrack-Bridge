//go:build windows

package startup

import (
	"golang.org/x/sys/windows/registry"
)

const regKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const appName = "FoxTrackBridge"

func enableWindows(exe string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(appName, exe)
}

func disableWindows() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, regKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.DeleteValue(appName)
}

func isEnabledWindows() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, regKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(appName)
	return err == nil
}
