package mqtt

import "testing"

// A rename mid-print moves the session, so the print is still recorded.
func TestRenameSession_MovesPrintInProgress(t *testing.T) {
	sessionMu.Lock()
	activeSessions["Old"] = &printSession{FileName: "cube.3mf"}
	sessionMu.Unlock()
	RenameSession("Old", "New")
	RemovePrinterState("Old")
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if s := activeSessions["New"]; s == nil || s.FileName != "cube.3mf" {
		t.Fatalf("session not moved: %+v", activeSessions)
	}
	delete(activeSessions, "New")
}
