//go:build !linux

package update

// EnsureRestartAlways is a no-op away from Linux: Windows and macOS do not run
// the bridge from a systemd unit.
func EnsureRestartAlways() (string, error) { return "", nil }
