package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

const defaultPort = 8080

// defaultPortFromEnv resolves the port to use when --port is not passed:
// FOXTRACK_BRIDGE_PORT if set and valid, otherwise defaultPort.
func defaultPortFromEnv() (int, error) {
	v := os.Getenv("FOXTRACK_BRIDGE_PORT")
	if v == "" {
		return defaultPort, nil
	}
	p, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid FOXTRACK_BRIDGE_PORT=%q: %w", v, err)
	}
	return p, nil
}

// resolvePort parses --port out of args against the given default. It is
// the single source of truth for the bridge's listen port: every
// self-referential URL (startup banner, systray "Open Dashboard") must read
// the value it returns rather than assuming a port.
func resolvePort(args []string, defaultPort int) (int, error) {
	fs := flag.NewFlagSet("foxtrack-bridge", flag.ContinueOnError)
	port := fs.Int("port", defaultPort, "port for the dashboard and API to listen on (env FOXTRACK_BRIDGE_PORT)")
	if err := fs.Parse(args); err != nil {
		// flag package already printed the error and usage to fs.Output().
		return 0, err
	}
	return *port, nil
}

// mustResolvePort is the process-level entry point: exits 0 on -h/--help
// (usage already printed by fs.Parse), exits 1 on any flag parse error
// (ditto), and exits 1 with its own message on a bad FOXTRACK_BRIDGE_PORT.
func mustResolvePort() int {
	envDefault, err := defaultPortFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	port, err := resolvePort(os.Args[1:], envDefault)
	if err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(1)
	}
	return port
}
