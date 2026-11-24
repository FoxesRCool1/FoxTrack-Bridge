package mqtt

import (
	"sync"
	"time"
)

// A user stop is gcode_state FAILED plus a print_error that says so, and the
// two can come in separate messages. When FAILED comes first with no error
// code at all, the end of the print waits (failHoldWait) for the code: the
// status stays as it was, so no session ends and no terminal relay goes out,
// until the code or the timeout decides between "cancelled" and "error". A
// FAILED that carries a code, or follows one, is never held.
var failHoldWait = 2 * time.Second // a variable so tests can shorten it

var (
	failHoldMu sync.Mutex
	failHolds  = map[string]*time.Timer{}

	// handlerLocks serialises the report handler per printer: the hold's
	// timer runs the handler from its own goroutine, beside the MQTT client's.
	handlerLocks sync.Map // name -> *sync.Mutex
)

func handlerLock(name string) *sync.Mutex {
	m, _ := handlerLocks.LoadOrStore(name, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func failHeld(name string) bool {
	failHoldMu.Lock()
	defer failHoldMu.Unlock()
	return failHolds[name] != nil
}

// startFailHold starts the wait for name unless one is running: a second bare
// FAILED does not push the deadline out.
func startFailHold(name string, expire func()) {
	failHoldMu.Lock()
	defer failHoldMu.Unlock()
	if failHolds[name] == nil {
		failHolds[name] = time.AfterFunc(failHoldWait, expire)
	}
}

func clearFailHold(name string) {
	failHoldMu.Lock()
	defer failHoldMu.Unlock()
	if t := failHolds[name]; t != nil {
		t.Stop()
		delete(failHolds, name)
	}
}

// heldFailed is the report the handler gets when a hold is over: the bare
// FAILED it kept back.
type heldFailed struct{}

func (heldFailed) Duplicate() bool   { return false }
func (heldFailed) Qos() byte         { return 0 }
func (heldFailed) Retained() bool    { return false }
func (heldFailed) Topic() string     { return "" }
func (heldFailed) MessageID() uint16 { return 0 }
func (heldFailed) Payload() []byte   { return []byte(`{"print":{"gcode_state":"FAILED"}}`) }
func (heldFailed) Ack()              {}

// removedPrinters holds the names RemovePrinterState took away, guarded by
// stateMutex. Work already under way for such a printer (a queued relay, an
// offline report, a late message) must not post or bring its state back.
// ConnectPrinter and a cloud subscribe take the name out again. A rename marks
// only the old name.
var removedPrinters = map[string]bool{}

func printerRemoved(name string) bool {
	stateMutex.RLock()
	defer stateMutex.RUnlock()
	return removedPrinters[name]
}

func revivePrinter(name string) {
	stateMutex.Lock()
	delete(removedPrinters, name)
	stateMutex.Unlock()
}

// ResetPrinterStateForTest forgets everything kept for name without marking
// it removed, so tests in other packages can reuse the name.
func ResetPrinterStateForTest(name string) {
	revivePrinter(name)
	resetPrinterState(name)
}
