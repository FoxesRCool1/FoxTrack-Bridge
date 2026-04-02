//go:build !windows

package startup

// Stubs so the Windows-only functions are satisfied on non-Windows builds.
func enableWindows(_ string) error  { return nil }
func disableWindows() error         { return nil }
func isEnabledWindows() bool        { return false }
