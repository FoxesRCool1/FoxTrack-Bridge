//go:build headless

package main

import (
	"log"
)

func main() {
	port := mustResolvePort()
	log.Println("FoxTrack Bridge (headless dev build) starting...")
	StartServer(port)
}
