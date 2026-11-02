package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	mqttpkg "foxtrack-bridge/mqtt"
)

// Files on the printer (Bridge 2.5.2). Two bridge_commands run off the poll
// loop, like print_file, because an FTPS listing or a print start can take
// seconds and would hold up pause and stop for the other printers:
//
//	list_files          -> done, with the list in result.data
//	print_printer_file  -> args {"name"}: start a file already on the printer
//
// Both are deduped on the command id (printJobs.hasSeen) because the
// command stays pending in FoxTrack until the ack lands.

const filesCommandTimeout = 45 * time.Second

// print_printer_file may first download the file (up to 200 MB) to find its
// sliced plate, then wait for the printer to start.
const printPrinterFileTimeout = 5 * time.Minute

func isFilesCommand(command string) bool {
	return command == "list_files" || command == "print_printer_file"
}

// startFilesCommand runs on the poll loop: match, dedupe, go.
func startFilesCommand(apiKey string, cmd bridgeCommand) {
	if printJobs.hasSeen(cmd.ID) {
		return
	}
	p, isBambu, mq, ok := findBridgePrinter(cmd.ExternalID)
	if !ok {
		return // another Bridge's printer: leave it pending
	}
	printJobs.markSeen(cmd.ID)
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
