## FoxTrack Bridge

FoxTrack Bridge runs on the same local network as your printer and sends printer data to FoxTrack (https://foxtrack.studio/).

<img width="1877" height="899" alt="screenshot-2026-04-21_15-43-47" src="https://github.com/user-attachments/assets/6e4efe37-e0eb-4be4-ac98-4c042312bfa4" />

Today the Bridge supports Bambu Lab local LAN access and Klipper printers through the Moonraker HTTP API.

## What it does

- Connects local printers to FoxTrack with your FoxTrack API token
- Shows printer status, current file, progress, temperatures, light state, and time remaining
- Sends telemetry to FoxTrack through the configured webhook
- Exposes local controls for supported printers: pause, resume, stop, light toggle, camera preview, and start print
- Opens a local dashboard at `http://localhost:8080`

## Current support

- Bambu Lab: supported
- Klipper (Moonraker): supported

## Downloads

Release builds are published for these targets only:

- Windows x64
- Windows Arm
- macOS Apple Silicon
- macOS Intel
- Linux x64

Get the latest build from the GitHub releases page:

- https://github.com/FoxesRCool1/FoxTrack-Bridge/releases/latest

## Setup

### 1. In FoxTrack

- Open `Settings > Integrations > 3D Printer Integration`
- Copy your API token

### 2. On the machine running the Bridge

- Download the correct build for your operating system
- Launch the app
- Open `http://localhost:8080` if the dashboard does not open automatically

### 3. Add your printer

For Bambu Lab:

- Put the printer in LAN Only Mode
- Enable Developer Mode
- Find the printer IP address
- Find the Serial Number (Visible in BambuStudio > Devices > Update)
- Find the LAN Access Code
- Enter those values into the Bridge dashboard

For Klipper (Moonraker):

- Use your Moonraker URL (typically `http://printer-ip:7125`)
- Add an API key only if Moonraker authentication is enabled

## Notes about controls

- Pause, resume, stop, and camera preview are available for configured printers
- Start print is available as an advanced action and currently expects a printer-accessible file path or URL
- If a start command fails, the file path or printer firmware behavior is the first thing to check

## Development

The normal app uses a system tray.

For environments that cannot compile tray dependencies, there is also a headless dev build mode:

```bash
go build -tags headless .
```

Headless builds start the local web server without the tray UI.

## Build targets

The project is configured to build only these release targets:

- Windows x64
- Windows Arm64
- macOS Apple Silicon
- macOS Intel
- Linux x64
