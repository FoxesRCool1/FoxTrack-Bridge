//go:build headless

package main

import (
	"log"
	"os"
)

func main() {
	port := mustResolvePort()
	log.Println("FoxTrack Bridge (headless dev build) starting...")
	if err := StartServer(port, nil); err != nil {
		// StartServer logged why. A non-zero exit tells systemd and Docker
		// the Bridge did not run.
		os.Exit(1)
	}
}
