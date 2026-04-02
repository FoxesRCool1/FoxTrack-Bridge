package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/getlantern/systray"

	"foxtrack-bridge/startup"
)

func main() {
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconBytes)
	systray.SetTitle("FoxTrack Bridge")
	systray.SetTooltip("FoxTrack Bridge — 3D Printer Integration")

	mOpen := systray.AddMenuItem("Open Dashboard", "Open FoxTrack Bridge in browser")
	systray.AddSeparator()

	startupLabel := startupMenuLabel()
	mStartup := systray.AddMenuItem(startupLabel, "Start FoxTrack Bridge automatically at login")
	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Quit FoxTrack Bridge", "Stop the bridge and exit")

	// Start HTTP server + MQTT connections in background
	go StartServer()

	// Open browser after a short delay so the server is ready
	go func() {
		time.Sleep(1500 * time.Millisecond)
		openBrowser("http://localhost:8080")
	}()

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser("http://localhost:8080")

			case <-mStartup.ClickedCh:
				if startup.IsEnabled() {
					_ = startup.Disable()
					mStartup.SetTitle("Enable: Start at Login")
				} else {
					if err := startup.Enable(); err != nil {
						fmt.Printf("Could not enable startup: %v\n", err)
					} else {
						mStartup.SetTitle("Disable: Start at Login")
					}
				}

			case <-mQuit.ClickedCh:
				systray.Quit()
			}
		}
	}()
}

func onExit() {}

func startupMenuLabel() string {
	if startup.IsEnabled() {
		return "Disable: Start at Login"
	}
	return "Enable: Start at Login"
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
