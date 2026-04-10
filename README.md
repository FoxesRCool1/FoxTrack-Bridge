## FoxTrack Bridge

FoxTrack Bridge runs on the same local network as your printer and sends printer data to FoxTrack (https://foxtrack.studio/).

## What it does

- Connects local printers to FoxTrack with your FoxTrack API token
- Shows printer status, current file, progress, temperatures, light state, and time remaining
- Sends telemetry to FoxTrack through the configured webhook
- Exposes local controls for supported printers: pause, resume, stop, light toggle, camera preview, and start print
- Opens a local dashboard at `http://localhost:8080`

## Current support

- Bambu Lab: supported
- Creality: setup flow exists, full printer integration is not confirmed to work yet
- Prusa: setup flow exists, full printer integration is not confirmed to work yet
  
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

For Creality and Prusa:

- Find and enter the printer's IP address
- (Prusa Only) Find and enter the Prusa API key

## Notes about controls

- Pause, resume, and stop are wired into the Bridge for supported Bambu printers. Coming soon to Prusa and Creality printers
