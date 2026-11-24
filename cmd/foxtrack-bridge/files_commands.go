package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"foxtrack-bridge/config"
	mqttpkg "foxtrack-bridge/mqtt"
)

// Files on the printer (Bridge 2.5.2). Two bridge_commands run off the poll
// loop, like print_file, because an FTPS listing or a print start can take
// seconds and would hold up pause and stop for the other printers:
//
//	list_files          -> done, with the list in result.data
//	print_printer_file  -> args {"name"}: start a file already on the printer
//
// Both are deduped on the command id (printJobs.hasSeen) because FoxTrack
// re-sends a pending command on every poll, and both are acked "running"
// before they start (acceptBridgeCommand), so FoxTrack does not expire them
// while they run.

const filesCommandTimeout = 45 * time.Second

// print_printer_file may first download the file (up to 200 MB) to find its
// sliced plate, then wait for the printer to start.
const printPrinterFileTimeout = 5 * time.Minute

func isFilesCommand(command string) bool {
	return command == "list_files" || command == "print_printer_file"
}

// acceptBridgeCommand tells FoxTrack that Bridge has taken a command that runs
// off the poll loop (status "running"), the way startPrintFile does for
// print_file. FoxTrack fails a command that is still pending after 2 minutes as
// "expired" and then answers 404 to its real result; a print start can take 5
// minutes and a home 2. It reports whether to run the command, and marks the
// id seen whenever no later poll may try it again. One try: this runs on the
// poll loop, and every retry would hold up pause and stop for the other
// printers.
func acceptBridgeCommand(apiKey string, cmd bridgeCommand, printerName string) bool {
	err := ackBridgeCommand(apiKey, bridgeCommandResult{CommandID: cmd.ID, Status: "running"})
	var statusErr *httpStatusError
	switch {
	case err == nil:
	case errors.As(err, &statusErr) && statusErr.code == http.StatusNotFound:
		printJobs.markSeen(cmd.ID)
		log.Printf("[bridge-commands] %s %s/%s: skipped, FoxTrack says another Bridge took it or it expired", cmd.ID, printerName, cmd.Command)
		return false
	case errors.As(err, &statusErr) && statusErr.code < 500:
		log.Printf("[bridge-commands] %s %s/%s: could not accept: %v", cmd.ID, printerName, cmd.Command, err)
		return false // not marked seen: the next poll tries again
	default:
		// No answer at all, or a 5xx (a gateway error can hide an ack that
		// went through). FoxTrack may have applied the ack, and then never
		// hands the command out again. Run it: the final done/failed ack is
		// accepted from pending or running.
		log.Printf("[bridge-commands] %s %s/%s: no answer to the running ack (%v), running it anyway", cmd.ID, printerName, cmd.Command, err)
	}
	printJobs.markSeen(cmd.ID)
	return true
}

// startFilesCommand runs on the poll loop: match, dedupe, ack "running", go.
func startFilesCommand(apiKey string, cmd bridgeCommand) {
	if printJobs.hasSeen(cmd.ID) {
		return
	}
	p, isBambu, mq, ok := findBridgePrinter(cmd.ExternalID)
	if !ok {
		return // another Bridge's printer: leave it pending
	}
	if !acceptBridgeCommand(apiKey, cmd, p.Name) {
		return
	}
	printJobs.wg.Add(1)
	go func() {
		defer printJobs.wg.Done()
		timeout := filesCommandTimeout
		if cmd.Command == "print_printer_file" {
			timeout = printPrinterFileTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		result := bridgeCommandResult{CommandID: cmd.ID, Status: "done"}
		var err error
		if cmd.Command == "list_files" {
			var files []mqttpkg.PrinterFile
			if isBambu {
				files, err = mqttpkg.ListPrinterFiles(ctx, mq)
			} else {
				files, err = lanCtrl.ListFiles(p.Name)
			}
			if err == nil {
				if files == nil {
					files = []mqttpkg.PrinterFile{}
				}
				result.Data, err = json.Marshal(files)
			}
		} else {
			name, _ := cmd.Args["name"].(string)
			plate, ok := 0, true // 0 = find it from the file
			if v, present := cmd.Args["plate"]; present && v != nil {
				if plate, ok = intArg(v); !ok || plate < 1 {
					err = errors.New("Pick a plate number from 1 up.")
				}
			}
			if err == nil {
				err = printPrinterFile(ctx, p.Name, isBambu, mq, name, plate)
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("The printer did not answer in time. Try again.")
		}
		if err != nil {
			result.Status = "failed"
			result.ErrorMessage = truncateChars(err.Error(), maxErrorChars)
			log.Printf("[bridge-commands] %s %s/%s failed: %v", cmd.ID, p.Name, cmd.Command, err)
		} else {
			log.Printf("[bridge-commands] %s %s/%s done", cmd.ID, p.Name, cmd.Command)
		}
		ackWithRetry(apiKey, result, func(format string, a ...interface{}) {
			log.Printf("[bridge-commands] %s %s/%s: %s", cmd.ID, p.Name, cmd.Command, fmt.Sprintf(format, a...))
		})
	}()
}

// printPrinterFile starts a file on the printer. The printer lock keeps it
// from racing a print_file upload to the same printer. plate is for Bambu
// only (0 = the sliced plate of the file); the LAN printers print the file as it is.
func printPrinterFile(ctx context.Context, name string, isBambu bool, mq mqttpkg.Printer, file string, plate int) error {
	if !printJobs.lockPrinter(name) {
		return errPrintBusy
	}
	defer printJobs.unlockPrinter(name)
	if isBambu {
		return mqttpkg.PrintPrinterFile(ctx, mq, file, plate)
	}
	return lanCtrl.PrintFile(name, file)
}

// errCannotStartFile is the answer for a printer kind that has no stored
// files Bridge can start.
var errCannotStartFile = errors.New("Bridge cannot start a stored file on this printer. It works on Bambu printers in LAN mode, Klipper and OctoPrint.")

// printerCanStartFile reports whether Bridge can start a file that is already
// on the printer: Bambu in LAN mode, Klipper and OctoPrint. Bambu Cloud only
// takes the light, PrusaLink has no file list and Home Assistant is read-only.
func printerCanStartFile(p config.Printer) bool {
	switch p.Kind() {
	case config.KindBambu:
		return isBambuPrinterConfig(p) && !p.IsCloud()
	case config.KindKlipper, config.KindOctoPrint:
		return true
	}
	return false
}

// startStoredFile starts a file that is already on the named printer, for the
// dashboard's "Start print" and the queue: the same path FoxTrack's
// print_printer_file command takes, so every kind that can start a file does.
func startStoredFile(ctx context.Context, printerName, file string) error {
	configMutex.RLock()
	var (
		p     config.Printer
		mq    mqttpkg.Printer
		found bool
	)
	if configStore != nil {
		for _, cp := range configStore.Printers {
			if cp.Name == printerName {
				p, mq, found = cp, mqttPrinter(cp, configStore), true
				break
			}
		}
	}
	configMutex.RUnlock()
	if !found || !printerCanStartFile(p) {
		return errCannotStartFile
	}
	err := printPrinterFile(ctx, p.Name, isBambuPrinterConfig(p), mq, file, 0)
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("The printer did not answer in time. Try again.")
	}
	return err
}
