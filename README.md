# FoxTrack Bridge

![GitHub release](https://img.shields.io/github/v/release/FoxesRCool1/FoxTrack-Bridge)
![Platforms](https://img.shields.io/badge/platforms-Windows%20%7C%20macOS%20%7C%20Linux-blue)

FoxTrack Bridge is a local dashboard and integration server for 3D printers. Run it on any machine on the same network as your printers to get a web dashboard for status, live camera feeds, print history, and basic controls. It can also connect to [FoxTrack](https://foxtrack.studio/) so you can monitor your printers remotely.

The dashboard runs fully offline. Its CSS, fonts, and icons are embedded in the binary, so no external network or CDN is needed to load the UI.

## A note on AI

Among many other tools, AI was used to develop this program. If you have a problem with that, you may look for alternative programs made by people with more time or contribute human-made code.

<img width="2519" height="1255" alt="screenshot-2026-05-21_16-45-12" src="https://github.com/user-attachments/assets/3b25ea5a-f462-459b-b5b3-6c7a5244bb86" />

---

## Features

- Live status for Bambu Lab and Klipper/Moonraker printers
- Print progress, temperatures, elapsed time, and ETA
- Live camera feed and light toggle
- AMS filament slot display with colors and material types (Bambu Lab only)
- Print speed selector: Silent, Standard, Sport, Ludicrous (Bambu Lab only)
- Fan speed control (Bambu Lab and Klipper)
- GCode console for sending single commands (Bambu Lab, Advanced mode only)
- Per-printer camera hiding, saved on the Bridge so it applies in every browser
- Bambu Cloud sign-in for printers that are not in LAN mode (experimental)
- Browser push notifications when prints finish, fail, or pause
- Print history stored locally, shown in the History view
- Automatic updates with staged installation
- Headless mode for servers, NAS devices, and Raspberry Pi

---

## Bridge runs in the background

FoxTrack Bridge is a persistent background service, not an app you open to check on a print. MQTT connections to Bambu Lab printers, print history recording, browser push notifications, and syncing to FoxTrack all only happen while the Bridge process is running. Anything a printer does while Bridge is stopped is not recorded and is not synced.

On Windows and macOS, use the system tray's "Enable: Start at Login" option to keep it running continuously. On Linux, see [Running on Linux](#running-on-linux) below for the equivalent systemd setups.

---

## Supported platforms

| Platform | Asset | Tray or headless |
|---|---|---|
| Windows x64 | `FoxTrack-Bridge-Windows.exe` | System tray |
| macOS Intel | `FoxTrack-Bridge-macOS-Intel.zip` | System tray |
| macOS Apple Silicon | `FoxTrack-Bridge-macOS-Apple-Silicon.zip` | System tray |
| Linux x64 | `FoxTrack-Bridge-Linux` | Headless |
| Linux ARM64 | `FoxTrack-Bridge-Linux-ARM64` | Headless |
| Linux ARM32 | `FoxTrack-Bridge-Linux-ARM32` | Headless |

Download the latest builds from the [releases page](https://github.com/FoxesRCool1/FoxTrack-Bridge/releases/latest).

Every Linux build is headless: there is no system tray, so control it from the terminal or systemd instead (see [Running on Linux](#running-on-linux)). Linux ARM32 is built for ARMv7 and later; it will not run on ARMv6 boards such as the original Raspberry Pi Zero or Raspberry Pi 1 (see [Raspberry Pi and other ARM boards](#raspberry-pi-and-other-arm-boards)).

---

## Verifying a download

Every release includes a `checksums.txt` with SHA-256 hashes for all six assets above. After downloading a binary, verify it before running it:

```bash
curl -fsSLO https://github.com/FoxesRCool1/FoxTrack-Bridge/releases/latest/download/checksums.txt
grep <asset-name> checksums.txt | sha256sum -c -
```

Replace `<asset-name>` with the file you downloaded, for example `FoxTrack-Bridge-Linux-ARM64`. On macOS, use `shasum -a 256 -c -` instead of `sha256sum -c -`; macOS doesn't ship `sha256sum` by default.

---

## Quick start

1. Run the Bridge. Download the binary for your platform (see [Supported platforms](#supported-platforms)) and launch it. On Windows and macOS, a system tray icon appears with an "Open Dashboard" menu. Linux builds are headless: run the binary from a terminal, or see [Running on Linux](#running-on-linux) for a desktop launcher and background-service setup. The startup output lists the local addresses where the dashboard is available.
2. Open the dashboard at [http://localhost:8080](http://localhost:8080). From other devices on the same network, use the IP address shown in the startup output (for example `http://192.168.x.x:8080`). You may need to disable WiFi AP isolation on your router.
3. Connect to FoxTrack (optional). In Settings, paste your FoxTrack API key and click Save. Get your key from [FoxTrack Settings > Integrations](https://foxtrack.studio/settings).
4. Add a printer. In Printers, click "Add Printer", select the printer type, fill in the required fields, and click Connect.

---

## Adding printers

### Bambu Lab

1. On the printer touchscreen, enable **LAN Only Mode** under Network settings.
2. Enable **Developer Mode** under About.
3. Note the **IP address**, **Serial Number**, and **LAN Access Code** (shown on the screen after enabling LAN Only Mode).
4. In the dashboard under Settings, select **Bambu Lab**, enter those values, and click Connect.

### Klipper / Moonraker

1. Find your Moonraker URL, usually `http://192.168.x.x:7125`.
2. In the dashboard under Settings, select **Klipper / Moonraker**, enter the URL, and click Connect.
3. If Moonraker has authentication enabled, also provide the Moonraker API key.

---

## Configuration and history files

FoxTrack Bridge stores `config.json` and `history.json` in your OS's standard per-user configuration directory:

| OS | Location |
|---|---|
| Linux | `~/.config/FoxTrack-Bridge/` (or `$XDG_CONFIG_HOME/FoxTrack-Bridge` if set) |
| macOS | `~/Library/Application Support/FoxTrack-Bridge/` |
| Windows | `%AppData%\FoxTrack-Bridge\` |

If `config.json` isn't found there, older builds' location, `<the executable's directory>/config/config.json`, is read automatically and migrated to the new location on the next save. This fallback is config-only: `history.json` has no equivalent, so history recorded by a very old install at the legacy location will not appear after upgrading.

If either file fails to load, for example because it's corrupted, Bridge backs the broken file up next to itself as `config.json.corrupt-<timestamp>` or `history.json.corrupt-<timestamp>` and starts fresh rather than deleting or overwriting it.

---

## Changing the listen port

Bridge listens on port 8080 by default. Change it with the `--port` flag or the `FOXTRACK_BRIDGE_PORT` environment variable; if both are set, `--port` wins:

```bash
./foxtrack-bridge --port 9000
# or
FOXTRACK_BRIDGE_PORT=9000 ./foxtrack-bridge
```

This matters on Klipper setups in particular: 8080 is also the default webcam port for mjpg-streamer on Mainsail and Fluidd, so running Bridge on the same machine can collide with it unless you move one of them off 8080.

---

## Running on Linux

### Manual install (recommended)

Download the asset matching your CPU architecture (see [Supported platforms](#supported-platforms)), verify it (see [Verifying a download](#verifying-a-download)), then place it somewhere your own user account owns, so auto-update can work later (see [Auto-update and install location](#auto-update-and-install-location)):

```bash
mkdir -p ~/.local/bin
mv FoxTrack-Bridge-Linux-ARM64 ~/.local/bin/foxtrack-bridge
chmod +x ~/.local/bin/foxtrack-bridge
```

Make sure `~/.local/bin` is on your `PATH`, then run it:

```bash
foxtrack-bridge
```

The startup output lists the local addresses where the dashboard is available. To keep it running in the background, see [Systemd user service](#systemd-user-service-recommended-for-desktop-linux) below.

### Scripted install (linux/install.sh)

The repository includes `linux/install.sh`, a per-user installer that installs the binary, an icon, a desktop launcher, and a systemd user unit under `$HOME`, with no sudo required. The release binary and `checksums.txt` always come from the GitHub release. Run it from a checkout of this repository and it reads the icon, desktop entry, and unit file from that checkout, which is useful for testing unreleased changes:

```bash
git clone https://github.com/FoxesRCool1/FoxTrack-Bridge.git
cd FoxTrack-Bridge
./linux/install.sh
```

Run it without a checkout and it downloads those same three files from the matching release's source tarball instead, so they always match the binary being installed:

```bash
curl -fsSL https://raw.githubusercontent.com/FoxesRCool1/FoxTrack-Bridge/main/linux/install.sh | sh
```

This installs the service but does not enable it; it prints the `systemctl --user enable --now` and `loginctl enable-linger` commands to run afterward (also covered below). It also installs a desktop launcher entry named "FoxTrack Bridge" that opens the dashboard in your browser; the launcher assumes the default port 8080. See [Uninstalling](#uninstalling) for `linux/install.sh --uninstall`.

### Systemd user service (recommended for desktop Linux)

Fetch the unit file from the repository and enable it as a user service:

```bash
mkdir -p ~/.config/systemd/user
curl -fsSL -o ~/.config/systemd/user/foxtrack-bridge.service \
  https://raw.githubusercontent.com/FoxesRCool1/FoxTrack-Bridge/main/linux/foxtrack-bridge-user.service
systemctl --user enable --now foxtrack-bridge
```

A user service normally only runs while you're logged in. To have it run at boot without a login session, enable linger for your account:

```bash
loginctl enable-linger "$(whoami)"
```

This is the same unit `linux/install.sh` installs to the same path.

### Systemd system service (servers and appliances)

For a machine dedicated to running Bridge under a specific system account, use the system-wide template unit instead:

```bash
sudo cp FoxTrack-Bridge-Linux-ARM64 /usr/local/bin/foxtrack-bridge
sudo cp linux/foxtrack-bridge@.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now foxtrack-bridge@<username>
```

Replace `<username>` with the account that should run the process; it doesn't need to be the account you're logged in as. Replace the asset name with the one matching your hardware from [Supported platforms](#supported-platforms). A binary copied in with `sudo cp` is owned by root, which affects auto-update; see below.

### Auto-update and install location

Whether Bridge can update itself depends on whether the account running it can write to the directory holding the running binary, not on who owns the machine. When it can't, Bridge doesn't fail silently: it logs an `[auto-update] unavailable in this environment` message explaining that the binary location is read-only, and the dashboard's update prompt shows a manual download link instead of an "Update now" button.

This is why `~/.local/bin` is recommended: a binary placed there by your own account, as in [Manual install](#manual-install-recommended) and the [systemd user service](#systemd-user-service-recommended-for-desktop-linux) above, stays writable by that account. A binary copied in with `sudo cp` into `/usr/local/bin`, as in the system service above, is owned by root; if the system service then runs it as a normal, non-root user, that user can't write to `/usr/local/bin`, so auto-update is unavailable there. The same applies inside Docker, where the binary's directory is never writable; update by pulling a new image instead.

### Firewall access

If Bridge and your browser are on different machines, the listen port (8080 by default, see [Changing the listen port](#changing-the-listen-port)) needs to be reachable across your LAN:

```bash
# ufw (Debian, Ubuntu)
sudo ufw allow 8080/tcp

# firewalld (Fedora, RHEL, openSUSE)
sudo firewall-cmd --permanent --add-port=8080/tcp
sudo firewall-cmd --reload
```

Some Wi-Fi routers also block device-to-device traffic by default (AP or client isolation); if the dashboard is unreachable from another device on the same network even with the firewall open, check that setting on your router.

### Raspberry Pi and other ARM boards

Linux ARM64 covers 64-bit Raspberry Pi OS on a Pi 3 or later, including the Pi Zero 2 W. Linux ARM32 covers 32-bit Raspberry Pi OS and targets ARMv7; it will not run on ARMv6 boards such as the original Raspberry Pi Zero or Raspberry Pi 1 (`linux/install.sh` detects an ARMv6 host and refuses to install rather than fetching a build that can't run).

### Uninstalling

If you installed with `linux/install.sh`:

```bash
./linux/install.sh --uninstall
```

This stops and disables the user service, then removes the binary, icon, desktop entry, and systemd user unit it installed. It does not touch `~/.config/FoxTrack-Bridge`, so your printers, settings, and print history are kept (see [Configuration and history files](#configuration-and-history-files)).

For a manual install with the user service, remove the same pieces yourself:

```bash
systemctl --user disable --now foxtrack-bridge
rm -f ~/.local/bin/foxtrack-bridge
rm -f ~/.config/systemd/user/foxtrack-bridge.service
rm -f ~/.local/share/applications/foxtrack-bridge.desktop
rm -f ~/.local/share/icons/hicolor/512x512/apps/foxtrack-bridge.png
```

For a system service install:

```bash
sudo systemctl disable --now foxtrack-bridge@<username>
sudo rm -f /usr/local/bin/foxtrack-bridge
sudo rm -f /etc/systemd/system/foxtrack-bridge@.service
sudo systemctl daemon-reload
```

Either way, your configuration and print history stay in place (see [Configuration and history files](#configuration-and-history-files)) unless you remove that directory yourself.

---

## Building from source

Requires Go 1.24 or later.

Always pass `-X foxtrack-bridge/version.AppVersion=<version>` in `-ldflags`. Without it, `AppVersion` stays `dev`, which the dashboard footer shows as `vdev`, and update checks are disabled by design: `dev` isn't a version number GitHub releases can be compared against.

Windows and macOS (with system tray):

```bash
go build -ldflags="-s -w -X foxtrack-bridge/version.AppVersion=<version>" -o foxtrack-bridge .
```

These builds need CGO enabled (the Go default) and a C toolchain: Xcode Command Line Tools on macOS, a MinGW-w64/gcc toolchain on Windows.

Linux, headless (recommended, no system tray, no GTK dependencies):

```bash
CGO_ENABLED=0 go build -tags headless -ldflags="-s -w -X foxtrack-bridge/version.AppVersion=<version>" -o foxtrack-bridge .
```

Linux, with system tray (requires `gcc` and the `gtk3` and `libayatana-appindicator3` development headers, for example `sudo apt-get install gcc libgtk-3-dev libayatana-appindicator3-dev` on Debian or Ubuntu):

```bash
CGO_ENABLED=1 go build -ldflags="-s -w -X foxtrack-bridge/version.AppVersion=<version>" -o foxtrack-bridge .
```

Cross-compiling for another architecture, for example a 64-bit Raspberry Pi (use `GOARCH=arm GOARM=7` instead of `GOARCH=arm64` for the ARM32/ARMv7 build):

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags headless -ldflags="-s -w -X foxtrack-bridge/version.AppVersion=<version>" -o foxtrack-bridge-arm64 .
```

Docker:

```bash
docker build --build-arg APP_VERSION=v2.3.0 -t foxtrack-bridge .
docker run -d --name foxtrack-bridge -p 8080:8080 -v foxtrack-bridge-data:/data --restart unless-stopped foxtrack-bridge
```

See [Docker documentation](docs/docker.md) for fork, branch, and server deployment examples.

Run tests:

```bash
go test -tags headless ./...
```

The `headless` tag runs the suite without the system tray dependency and includes the embedded-asset tests.

---

## Bambu Cloud (printers not in LAN mode)

This feature is experimental. It is labelled that way in the dashboard too.

Bambu printers can also be added through your Bambu account instead of LAN mode. Open **Settings > Bambu Cloud**, sign in with your Bambu email and password, and enter the code Bambu emails you. Then add a printer with the type **Bambu Lab (cloud, experimental)** and pick it from the list.

What cloud printers can do:

- Status, temperatures, progress, AMS and the light toggle work.
- Pause, stop, speed, fan and GCode do **not** work over the cloud. Bambu's 2025+ firmware rejects every command except the light from third-party clients, so those buttons are hidden. Use LAN mode for full control.
- The camera works when the printer is on the same network as the Bridge. The account hands back the printer's LAN access code, and the printer reports its own LAN address, so nothing needs typing. You can also enter the IP when adding the printer.

How the Bridge stays inside Bambu's limits (Bambu bans accounts for 24 hours to 7 days when it sees connection churn):

- One MQTT connection for the whole account, no matter how many cloud printers.
- Reconnects back off from 5 seconds to 5 minutes with jitter. After 10 failures, or 3 refused connections in a row, it pauses for 30 minutes and tells you why.
- A rejected sign-in is final: the Bridge stops and asks you to link the account again. It never signs in on its own.
- Sign-in is limited to once a minute, a Cloudflare block pauses sign-in for 5 minutes, and the device list is cached for a minute.
- A full state request goes to a printer at most once a minute.

Tokens last about 90 days and cannot be renewed. The dashboard warns a week before the token expires. If sign-in is blocked by Cloudflare, "Paste a token instead" accepts a token from a signed-in Bambu session.

Config: the account is stored under `bambu_cloud` in `config.json` (the file is mode 0600) and each cloud printer carries `"connection": "cloud"`. Both are additive; existing configs and LAN printers are untouched. Design notes and sources: [docs/bambu-cloud.md](docs/bambu-cloud.md).

---

## Regenerating dashboard assets

The dashboard loads no external resources. Tailwind CSS, the fonts, and the icons are committed under `web/` and embedded in the binary. Attribution is in [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).

Design: the dashboard follows the FoxTrack web app's design system. Colours are tokens (`--bg`, `--panel`, `--panel-2`, `--surface`, `--border`, `--text`, `--text-2`, `--accent`, `--accent-2`, `--link`, `--danger`, `--warning`) defined in the `<style>` block of `web/ui.html` for two themes, Paper (light, the default) and Ink (dark). The theme button in the top bar switches them; the choice is stored in the browser under `foxtrack.theme`. Tailwind utilities such as `bg-panel`, `text-text-2`, `border-border` and `bg-brand` map onto the tokens through `web/tailwind.config.js`. Never put a Tailwind palette colour or a hex value in the markup; `go test -tags headless ./...` fails if one appears.

Tailwind: after changing classes in `web/ui.html`, regenerate `web/tailwind.css` with the standalone CLI (no Node required):

```bash
# one-time: download the pinned CLI (git-ignored)
curl -sL -o web/tailwindcss \
  https://github.com/tailwindlabs/tailwindcss/releases/download/v3.4.17/tailwindcss-linux-x64
chmod +x web/tailwindcss
./web/tailwindcss -c web/tailwind.config.js -i web/tailwind.input.css -o web/tailwind.css --minify
```

Fonts: `web/fonts/inter-latin.woff2` is a latin subset of Inter (variable, weights 400 to 600) for every control and label, and `web/fonts/cormorant-garamond-latin-500.woff2` is the heading face used for titles and big numbers (`font-heading`). Both are served through `web/fonts.css`. Replace a file to update it.

Icons: `web/icons.css` holds the Lucide icons used by `web/ui.html`, encoded as inline SVG masks so they take the current text colour. Class names keep the historical `.fa-<name>` form. To add an icon, add a `.fa-<name>` rule with the Lucide SVG body (same format as the existing rules). `go test -tags headless ./...` fails if `web/ui.html` uses an icon that `web/icons.css` does not define.

Logos: `assets/logo-light.png` and `assets/logo-dark.png` are the FoxTrack header marks, one per theme, served at `/logo-light.png` and `/logo-dark.png`. `assets/logo.png` stays as the app icon and notification image.

The generated files are committed. No asset tooling runs during `go build` or the Docker build.

---

## Contributing

Contributions are welcome. If you find a bug, want a feature, or want to improve the docs, open an issue or a pull request. Small fixes and additions are appreciated, including edits to this README.

---

## License

MIT. Free to use, modify, and redistribute with attribution.
