package ai

// The Bridge's own help library. It is the ONLY thing the assistant is allowed
// to know about how the Bridge behaves: the system prompt forbids answering a
// how-does-it-work question from anywhere else, and search_help / get_help_topic
// read nothing but this file.
//
// Every claim below is taken from README.md, docs/bambu-cloud.md, docs/docker.md
// or the handler it describes. A sentence here becomes an instruction a user
// follows at their printer, so a guess costs them a failed print. When the code
// changes, change the topic in the same commit.

// Topic is one help article. Keywords carry the words a user would actually
// type for a symptom ("offline", "spaghetti", "8883") which rarely appear in
// Title and are the difference between the search finding the topic and not.
type Topic struct {
	ID       string
	Title    string
	Keywords []string
	Body     string
}

// TopicIDs returns every topic ID, in Topics order. The tool schema enumerates
// these so the model cannot ask for a topic that does not exist.
func TopicIDs() []string {
	ids := make([]string, 0, len(Topics))
	for _, t := range Topics {
		ids = append(ids, t.ID)
	}
	return ids
}

// FindTopic returns the topic with the given ID, and whether it was found.
func FindTopic(id string) (Topic, bool) {
	for _, t := range Topics {
		if t.ID == id {
			return t, true
		}
	}
	return Topic{}, false
}

var Topics = []Topic{
	{
		ID:       "getting-started",
		Title:    "What the Bridge is and how to open it",
		Keywords: []string{"start", "setup", "install", "dashboard", "open", "localhost", "8080", "port", "first", "background", "service", "tray", "headless"},
		Body: `FoxTrack Bridge is a local dashboard and integration server for 3D printers. It runs on a machine on the same network as the printers. It shows status, camera feeds, print history and basic controls, and it can sync to FoxTrack for remote monitoring.

Open the dashboard at http://localhost:8080 on the machine running the Bridge. From another device on the same network, use the IP address printed in the Bridge's startup output, for example http://192.168.x.x:8080. If other devices cannot reach it, WiFi AP isolation on the router is the usual cause.

The Bridge is a background service, not an app you open to check a print. MQTT connections to Bambu printers, print history recording, browser push notifications and FoxTrack syncing only happen while the Bridge process is running. Anything a printer does while the Bridge is stopped is never recorded and never synced.

On Windows and macOS there is a system tray icon with "Open Dashboard" and an "Enable: Start at Login" option. Every Linux build is headless: there is no tray, so run it from a terminal or set up a systemd service.

Setup order: run the Bridge, open the dashboard, optionally paste a FoxTrack API key in Settings, then add a printer under Printers with "Add Printer".`,
	},
	{
		ID:       "add-bambu-lan",
		Title:    "Adding a Bambu Lab printer in LAN mode",
		Keywords: []string{"bambu", "lan", "add", "connect", "serial", "access code", "lan code", "developer mode", "x1", "p1", "a1", "h2d", "8883", "mqtt"},
		Body: `LAN mode is the full-featured way to connect a Bambu Lab printer. All controls work.

On the printer's touchscreen:
1. Turn on LAN Only Mode under Network settings.
2. Turn on Developer Mode under About.
3. Write down the IP address, the Serial Number, and the LAN Access Code. The access code is shown on the screen after LAN Only Mode is enabled.

In the dashboard:
4. Go to Printers and click "Add Printer".
5. Choose the type Bambu Lab.
6. Enter the IP address, serial number and LAN access code, then click Connect.

The Bridge reaches the printer over MQTT on TCP port 8883. If the printer is not on the same network segment as the Bridge, or a firewall blocks 8883, the connection fails.

The serial number and access code are case sensitive and must be typed exactly. A wrong access code looks the same as an unreachable printer from the dashboard: the printer never comes online.`,
	},
	{
		ID:       "add-bambu-cloud",
		Title:    "Adding a Bambu printer through Bambu Cloud (experimental)",
		Keywords: []string{"cloud", "bambu account", "sign in", "login", "email", "code", "token", "not in lan mode", "experimental", "ban", "banned", "expired"},
		Body: `Bambu Cloud is for printers that are not in LAN mode. It is experimental and it is labelled that way in the dashboard.

To link the account: open Settings > Bambu Cloud, sign in with the Bambu email and password, then enter the code Bambu emails back. Then add a printer with the type "Bambu Lab (cloud, experimental)" and pick it from the list.

What cloud printers can do: status, temperatures, progress, AMS and the light toggle work.

What they cannot do: pause, stop, speed, fan and GCode do not work over the cloud. Bambu's 2025 and later firmware rejects every command except the light from third-party clients, so those buttons are hidden. LAN mode is the only way to get full control.

The camera works when the printer is on the same network as the Bridge. The account hands back the printer's LAN access code and the printer reports its own LAN address, so nothing needs typing. The IP can also be entered when adding the printer.

Tokens last about 90 days and cannot be renewed. The dashboard warns a week before a token expires; after that, link the account again.

Bambu bans accounts for 24 hours to 7 days when it sees connection churn, so the Bridge paces itself deliberately: one MQTT connection for the whole account however many cloud printers there are, reconnect backoff from 5 seconds to 5 minutes with jitter, a 30 minute pause after 10 failures or 3 refused connections in a row, sign-in limited to once a minute, a 5 minute pause after a Cloudflare block, a one minute device list cache, and a full state request to a printer at most once a minute. A rejected sign-in is final: the Bridge stops and asks for the account to be linked again rather than retrying on its own.

If sign-in is blocked by Cloudflare, "Paste a token instead" accepts a token taken from a signed-in Bambu session in a browser.`,
	},
	{
		ID:       "add-klipper",
		Title:    "Adding a Klipper or Moonraker printer",
		Keywords: []string{"klipper", "moonraker", "mainsail", "fluidd", "7125", "url", "api key", "add", "connect", "voron", "ratrig"},
		Body: `Klipper printers connect over Moonraker's HTTP API.

1. Find the Moonraker URL. It is usually http://192.168.x.x:7125.
2. In the dashboard go to Printers, click "Add Printer" and choose Klipper / Moonraker.
3. Enter the URL and click Connect.
4. If Moonraker has authentication turned on, also enter the Moonraker API key.

The URL must include the scheme (http:// or https://) and the port. The Bridge checks reachability by calling /printer/info on that URL, so a URL that works in a browser works here.

A common clash: the Bridge listens on port 8080 by default, and 8080 is also the default webcam port for mjpg-streamer on Mainsail and Fluidd. Running both on the same machine collides unless one is moved. Change the Bridge with --port 9000 or the FOXTRACK_BRIDGE_PORT environment variable; if both are set, --port wins.`,
	},
	{
		ID:       "add-prusalink",
		Title:    "Adding a Prusa printer through PrusaLink",
		Keywords: []string{"prusa", "prusalink", "mk4", "xl", "mini", "core one", "usb drive", "api key", "add", "connect"},
		Body: `Prusa printers with PrusaLink (MK4, XL, MINI and Core One) connect over PrusaLink's HTTP API.

1. On the printer, turn on PrusaLink in the network settings. Write down the printer's IP address and the API key shown on its PrusaLink settings screen.
2. In the dashboard go to Printers, click "Add Printer" and choose Prusa (PrusaLink).
3. Enter the URL (for example http://192.168.1.50) and the API key, then click Connect.
4. PrusaLink has no camera stream. If there is a separate webcam, enter its URL.

The Bridge reads status, progress, temperatures, file name and time left, and it records finished prints in the history. FoxTrack can pause, resume and stop a print. FoxTrack can also send a .gcode file: the Bridge puts it on the printer's USB drive and starts it. FoxTrack knows a PrusaLink printer by its name in the Bridge.

The Bridge checks reachability by calling /api/version on that URL.`,
	},
	{
		ID:       "add-home-assistant",
		Title:    "Adding a printer through Home Assistant (read-only)",
		Keywords: []string{"home assistant", "homeassistant", "entity", "entities", "entity id", "access token", "long-lived", "read-only", "read only", "8123", "add", "connect"},
		Body: `A Home Assistant printer is read-only. If Home Assistant already shows the printer (for example through its PrusaLink, OctoPrint or Bambu Lab integration), the Bridge can read the printer's status from Home Assistant. FoxTrack then sees the status but cannot pause, stop or start a print.

1. In Home Assistant, open your profile, then Security, and create a long-lived access token.
2. Find the printer's entity IDs under Settings > Devices & services > Entities. The entity that holds the printer's state is required. Progress, nozzle temperature, bed temperature, file name and time left are optional.
3. In the dashboard go to Printers, click "Add Printer" and choose Home Assistant (read-only).
4. Enter the Home Assistant URL (usually http://homeassistant.local:8123), the token and the entity IDs, then click Connect.

The state entity can read idle, printing, paused, finished or error (running, standby, complete and failed work too). Unavailable or unknown shows the printer as offline. A time left entity can count seconds, minutes or hours (the Bridge reads its unit), or hold the time the print will finish.

The Bridge checks reachability by calling /api/ on the Home Assistant URL. That call carries no token, so it shows that Home Assistant answers, not that the token is right. FoxTrack knows a Home Assistant printer by its name in the Bridge.`,
	},
	{
		ID:       "edit-printer",
		Title:    "Changing a saved printer (new IP address, rename, new access code)",
		Keywords: []string{"edit", "change", "rename", "new ip", "ip changed", "update", "pencil", "modify", "access code changed", "wrong serial", "moved"},
		Body: `A saved printer can be changed without removing it. Click the pencil icon on the printer's card, change the details, and click Save.

What can be changed depends on the type. Bambu LAN mode: name, IP address, serial number and LAN access code. Bambu Cloud: name, and the IP address used for the camera. Klipper: name, Moonraker URL, Moonraker API key and webcam URL. PrusaLink: name, PrusaLink URL, API key and webcam URL. Home Assistant: name, Home Assistant URL, token, the state entity ID, the optional entity IDs (progress, nozzle temperature, bed temperature, file name, time left) and webcam URL. The printer type itself cannot be changed; remove the printer and add it again for that.

The access code, API key and Home Assistant token fields start blank because the dashboard never receives saved secrets. Leaving them blank keeps the saved value.

Saving reconnects only that printer, and only if a connection detail changed. Other printers keep running.

A printer cannot be renamed during a print, because the running print is tracked under its name and its history record would be lost. The IP address can be changed during a print. Local print history from before a rename stays with the printer.

In FoxTrack, a renamed Bambu printer keeps its link because FoxTrack follows the serial number. A renamed Klipper, PrusaLink or Home Assistant printer shows up in FoxTrack as a new device, because FoxTrack knows those printers by name. Changing a Bambu serial number has the same effect. To move the link: in FoxTrack, click the printer, set its Bridge device section to Unlinked, then link the new device from "Detected on your network, not linked" in the Fleet view. The old device stays under "Detected on your network, not linked" and shows as offline. A FoxTrack owner or admin can clear it with its Remove button, which appears once the device is offline.`,
	},
	{
		ID:       "connection-problems",
		Title:    "A printer will not connect or shows as offline",
		Keywords: []string{"offline", "disconnected", "not connecting", "unreachable", "timeout", "refused", "cannot reach", "no status", "stuck", "unknown", "connection", "firewall", "network"},
		Body: `Work through these in order. The first three cover almost every case.

1. Is the Bridge on the same network as the printer? The Bridge reaches printers directly over the LAN. A printer on a guest network, a separate VLAN, or a different WiFi band that the router isolates cannot be reached. WiFi AP isolation blocks this too.

2. Is the address right and still current? Most printers get their IP from DHCP, so it changes after a reboot or a lease expiry. A printer that worked yesterday and is offline today usually has a new IP. Check the address on the printer, then click the pencil icon on the printer's card in the dashboard, change the IP address and click Save. There is no need to remove the printer and add it again. A DHCP reservation on the router stops it happening again.

3. Are the credentials right? For Bambu LAN mode the serial number and LAN access code are case sensitive. The access code changes every time LAN Only Mode is toggled off and on. A wrong code looks identical to an unreachable printer: the printer simply never comes online.

Then, by printer type:

Bambu LAN mode: the Bridge connects over MQTT on TCP port 8883. Confirm LAN Only Mode and Developer Mode are both still on; a firmware update can turn Developer Mode off. Confirm nothing is blocking 8883 between the two machines.

Bambu Cloud: check Settings > Bambu Cloud. A token lasts about 90 days and cannot be renewed, so an expired token means signing in again. If the Bridge says it has paused, it has hit its own rate limiting on purpose after repeated failures, and it will resume on its own; do not retry in a loop, because Bambu bans accounts for connection churn.

Klipper: open the Moonraker URL in a browser on the same machine as the Bridge. If it does not answer there, the problem is Moonraker or the network, not the Bridge. If Moonraker has authentication on, the API key must be filled in.

PrusaLink: open the printer's URL in a browser on the same machine as the Bridge. If it does not answer there, the problem is the printer or the network, not the Bridge. Check that the API key matches the one on the printer's PrusaLink settings screen.

Home Assistant: open the Home Assistant URL in a browser on the same machine as the Bridge. Then check that the long-lived access token is still valid and that the entity IDs match what Home Assistant shows under Settings > Devices & services > Entities. A state entity that reads unavailable or unknown shows the printer as offline.

The Bridge writes a line to its log for each connection attempt and failure, prefixed with the printer's name. The log is the fastest way to tell "wrong credentials" from "no route to host".`,
	},
	{
		ID:       "cameras",
		Title:    "Camera feeds and the live printer view",
		Keywords: []string{"camera", "webcam", "video", "feed", "stream", "snapshot", "picture", "live view", "black", "blank", "hidden", "hide", "loading", "spinning", "unavailable", "rtsp"},
		Body: `Each printer tile can show a live camera feed.

Bambu LAN mode: the Bridge pulls frames from the printer directly using its IP and LAN access code, on TCP port 6000. Nothing needs configuring beyond what the printer already needed to connect. Only the A1, A1 mini, P1P and P1S serve that stream. Other models, such as the X1 and H2 series, send their camera as an RTSP stream on port 322, which the Bridge cannot read yet; their status and controls still work.

If a Bambu printer accepts the camera connection but sends no picture within 15 seconds, the dashboard shows "Camera unavailable" and the Bridge log has a line starting with [camera/ that says why. The usual causes are a model that does not serve the port 6000 stream, or a LAN access code that changed when LAN Only Mode was toggled. A new access code can be entered with the pencil icon on the printer's card.

Bambu Cloud: the camera works only when the printer is on the same network as the Bridge, because the frame still comes from the printer over the LAN, not from Bambu's servers.

Klipper: the Bridge proxies the webcam URL configured for that printer.

PrusaLink and Home Assistant: neither has a camera stream of its own. The Bridge proxies the webcam URL only if one is set on the printer; without one there is no feed.

A camera can be hidden per printer. That setting is saved on the Bridge, so it applies in every browser, and it only affects the dashboard; snapshot sending to FoxTrack is unaffected.

If a feed is black or blank: confirm the printer's own interface shows the camera, check the printer is reachable at all (an offline printer has no camera either), and for Klipper, PrusaLink and Home Assistant confirm the webcam URL is right and reachable from the Bridge's machine rather than only from your laptop.

In FoxTrack, the live picture only loads when the browser runs on the same computer as the Bridge. Everywhere else FoxTrack shows the latest snapshot. While a print is running or paused, the Bridge sends a snapshot about every 30 seconds when a FoxTrack page with the printers is open, and about every 10 minutes otherwise. An idle printer's snapshot does not update. Klipper, PrusaLink and Home Assistant printers send snapshots only when a webcam URL is set on the printer.`,
	},
	{
		ID:       "print-problems",
		Title:    "Diagnosing a print that failed, paused or looks wrong",
		Keywords: []string{"failed", "fail", "paused", "stopped", "error", "spaghetti", "warping", "adhesion", "layer", "clog", "jam", "stringing", "nozzle", "bed", "temperature", "ams", "filament", "runout"},
		Body: `What the Bridge can tell you about a print:

- The current status, progress percentage, file name and estimated time remaining.
- Nozzle and bed temperature against their targets. A nozzle far below target during a print points at a heater or thermistor fault; a nozzle at target with no extrusion points at a clog or a stripped filament path.
- The printer's own error text, when it reports one. Bambu reports a print error code; that code is the printer's, not the Bridge's, and Bambu's documentation is the authority on what it means.
- The AMS slots with colour, material and remaining percentage, for Bambu printers. An empty or near-empty active slot explains a pause.
- The cooling fan percentage and the speed level.
- Print history: every completed print with its result (finished, cancelled or error), duration, and temperature samples taken during the print.

What the Bridge cannot tell you: it does not see the print. Nothing in the telemetry says whether a part has warped, lifted, stringed or turned to spaghetti. Only the camera shows that, and only if a camera is configured.

A paused Bambu print is usually one of: filament runout, an AMS slot change, a user pause, or the printer detecting a problem itself. The status and error fields distinguish them.

The Bridge never controls a print on its own. It can pause, stop, set speed and set fan for LAN-mode Bambu printers and Klipper printers, and pause, resume and stop for PrusaLink printers, only when a person presses the button. Home Assistant printers are read-only.`,
	},
	{
		ID:       "controls",
		Title:    "What the control buttons do, and which printers have them",
		Keywords: []string{"pause", "stop", "resume", "cancel", "pause all", "all printers", "bulk", "light", "speed", "silent", "sport", "ludicrous", "fan", "gcode", "console", "command", "advanced", "temperature", "nozzle", "bed", "heat", "preheat", "home", "temps"},
		Body: `Available controls depend on how the printer is connected.

Bambu Lab in LAN mode: pause, stop, light toggle, print speed (Silent, Standard, Sport, Ludicrous), fan speed, the Temps panel, and a GCode console for single commands. The GCode console only appears in Advanced mode.

Bambu Lab over the cloud: the light toggle only. Pause, stop, speed, fan, Temps and GCode are hidden because Bambu's 2025 and later firmware rejects them from third-party clients. LAN mode is the only way to get those back.

Klipper / Moonraker: pause, stop, fan speed and the Temps panel.

OctoPrint: pause, resume, stop and the Temps panel.

Prusa (PrusaLink): pause, resume and stop. PrusaLink has no call for temperatures or homing, so there is no Temps panel.

The Temps panel (the "Temps" button on the card) sets the nozzle target (0 to 350 C) and the bed target (0 to 120 C) with a Set button each; 0 turns that heater off. Its Home button homes X, Y and Z and is disabled while a print runs. FoxTrack's opened printer has the same fields and sends them through the Bridge.

Home Assistant (read-only): no controls. The Bridge only reads the status.

A printer with a smart plug set in the Bridge also has a "Plug on" / "Plug off" button. "Plug off" is disabled while the printer prints or is paused, and the Bridge refuses it while the nozzle is above 50 C.

AMS slot display, the speed selector and the GCode console are Bambu-only. Fan control works on both Bambu and Klipper.

The command menu (Ctrl+K or Cmd+K) also has bulk actions, shown only when they apply: Pause all printing, Resume all paused, and Turn off idle plugs (printers that are not printing or paused and whose nozzle is below 50 C). Each one asks first and lists the printers it will act on.`,
	},
	{
		ID:       "files-on-printer",
		Title:    "Files on the printer: list and print them from FoxTrack",
		Keywords: []string{"files", "file list", "sd card", "card", "cache", "reprint", "print again", "3mf", "gcode", "moonraker", "octoprint", "list_files"},
		Body: `FoxTrack's opened printer (the Printers page, click a printer) has a "Files on the printer" section. It lists the print files already on the printer, newest first, with size and date, up to 200. Each row has a Print button that starts that file on that printer. The Bridge reads the list only when FoxTrack asks for it.

Bambu Lab in LAN mode: the .3mf and .gcode.3mf files on the card and in its cache folder (the folder Bambu Studio sends to), read over FTPS with the access code. A file in the cache shows as cache/<name>.
Klipper / Moonraker: the files in Moonraker's gcodes folder.
OctoPrint: the files in OctoPrint's local storage.
PrusaLink and Home Assistant printers are not listed; FoxTrack hides the section for them.

Print is refused while the printer prints or is paused, and while the Bridge is sending another file to that printer. A Bambu printer over the cloud cannot list or print files; switch it to LAN mode.

This section is in FoxTrack, not in the Bridge dashboard. It needs FoxTrack Bridge 2.5.2 or later.`,
	},
	{
		ID:       "add-octoprint",
		Title:    "Adding an OctoPrint printer",
		Keywords: []string{"octoprint", "octopi", "api key", "application key", "add", "connect", "webcam", "snapshot", "5000"},
		Body: `Printers run by OctoPrint connect over OctoPrint's HTTP API.

1. In OctoPrint, make an API key: Settings > Application Keys (make one for the Bridge), or copy the global key under Settings > API.
2. In the dashboard go to Printers, click "Add Printer" and choose OctoPrint.
3. Enter the OctoPrint address (for example http://octopi.local, or http://192.168.1.60:5000 when OctoPrint is not behind a web server) and the API key, then click Connect.
4. The webcam is read from OctoPrint's own webcam settings. Enter a webcam URL only if the camera button does not work.

The Bridge reads status, progress, temperatures, file name and time left, records finished prints in the history, and reports an OctoPrint printer error as an alarm. FoxTrack can pause, resume and stop a print, and can send a .gcode file: the Bridge uploads it to OctoPrint and starts it. FoxTrack knows an OctoPrint printer by its name in the Bridge.

"OctoPrint is running but is not connected to the printer" means OctoPrint itself answers but its serial connection to the printer is closed: press Connect in OctoPrint.

The network scan finds OctoPrint on port 80 and port 5000. The Bridge checks reachability by calling /api/version on the address with the key; OctoPrint answers 403 to a wrong key.`,
	},
	{
		ID:       "smart-plugs",
		Title:    "Smart plugs: watts, energy per print, auto off",
		Keywords: []string{"plug", "smart plug", "shelly", "tasmota", "kasa", "tapo", "tuya", "smart life", "meross", "sonoff", "esphome", "athom", "mystrom", "wiz", "home assistant", "power", "watts", "energy", "kwh", "electricity", "auto off", "turn off", "switch"},
		Body: `A printer can be powered through a smart plug the Bridge reads and switches over your network. Supported types: Shelly, Tasmota, TP-Link Kasa, TP-Link Tapo, Tuya / Smart Life, Meross, Sonoff (DIY mode), ESPHome, myStrom, WiZ, and Home Assistant (any plug Home Assistant can switch, such as Zigbee, Matter or Hue plugs). The Bridge talks to the plug on your network and never calls a maker's cloud. It works for every printer type the Bridge controls, a Bambu printer added through a Bambu account included. What each type needs is in "Setting up each smart plug type".

To set one: open the printer's pencil (edit) or add it, scroll to Smart plug, choose the type, fill in the fields that appear (the form shows setup help for the type you pick), and click "Check plug". The check reports on or off and the watts if the plug has an energy meter, or says what is wrong. Keys, passwords and tokens are stored only on the machine running the Bridge and are never shown again; leave such a field blank when you edit the printer to keep the saved one.

The printer card then shows "Plug on · 142 W" and a "Plug on" / "Plug off" button, also while the printer itself is switched off. "Plug off" is disabled while the printer prints or is paused, refused while the nozzle is above 50 C, and refused when the Bridge cannot tell whether the printer is idle, so a print is never cut.

"Turn the plug off after a print": once a print ends (finished, stopped or failed), the Bridge waits the number of minutes you set (10 by default) with the printer idle and the nozzle under 50 C, then turns the plug off. A new print in that time cancels it. The log says "plug turned off after the print".

Energy per print: the Bridge reads the plug's energy counter when a print starts and ends (Shelly, Tasmota, Kasa), or sums watts over time for a plug without a counter (Tapo, Tuya, Meross, ESPHome, myStrom, WiZ, Home Assistant), and sends the Wh to FoxTrack with the print history. FoxTrack shows it in Reports under Printer use as kWh and a cost. A plug without a meter (or a Sonoff in DIY mode) switches but reports no watts or energy.

The plug is polled every 10 seconds. "Plug not answering" means the last poll failed; the Bridge keeps trying.`,
	},
	{
		ID:       "smart-plug-setup",
		Title:    "Setting up each smart plug type",
		Keywords: []string{"plug", "smart plug", "setup", "tapo", "kasa", "tp-link", "tuya", "smart life", "local key", "tinytuya", "meross", "key", "sonoff", "diy mode", "ewelink", "esphome", "athom", "mystrom", "token", "wiz", "home assistant", "long-lived", "entity", "shelly", "tasmota", "check plug", "not answering", "login refused"},
		Body: `Every type needs the plug's address on your network (its IP address, shown in the maker's app under device info, or in your router's list of devices). Give the plug a fixed IP address in your router so it does not move. Then pick the type and fill in what it asks for.

Shelly (Gen1 to Gen4) and Tasmota: only the address. Any plug running Tasmota works, including Sonoff, Athom and Nous plugs flashed with it.

TP-Link Kasa: older Kasa plugs need only the address. Newer Kasa firmware also needs the email and password of the TP-Link account used in the Kasa app. The plug checks them itself; the Bridge never contacts TP-Link.

TP-Link Tapo (P100, P110, P115 and others): the address plus the email and password of the TP-Link account used in the Tapo app, both case-sensitive. Both the current (KLAP) and the older Tapo login are supported. If "Check plug" says the login was refused although it is right, turn on Third-Party Compatibility in the Tapo app (Me, Tapo Lab) and check again.

Tuya / Smart Life (also sold under many other names that use the Smart Life or Tuya Smart app): the address, the device ID and the 16-character local key. The key only comes from Tuya, so get it once: 1) sign up at iot.tuya.com, create a Cloud project (Smart Home) in the data center of your Smart Life account, and link your Smart Life app to it (Devices, Link App Account, scan the QR code with the app); 2) on any computer with Python run "pip install tinytuya" and then "python -m tinytuya wizard", and give it the project's Access ID and Access Secret; 3) copy the plug's "id" and "key" from the list it prints. Leave Protocol on Auto. Re-pairing the plug in Smart Life changes its key, so fetch it again then. A Tuya plug answers one local connection at a time: close Home Assistant's Local Tuya or tinytuya if the check cannot reach it.

Meross: the address; leave the key blank, most Meross plugs answer without it. Newer models that encrypt their local traffic need the Meross key and the plug's MAC address (on its label or in the Meross app's device info; on Linux the Bridge finds the MAC itself). The key belongs to your Meross account; Home Assistant's meross_lan shows it, or run "pip install meross_iot" and then: python -c "import asyncio; from meross_iot.http_api import MerossHttpClient as C; print(asyncio.run(C.async_from_user_password(api_base_url='https://iotx-eu.meross.com', email='YOUR EMAIL', password='YOUR PASSWORD')).cloud_credentials.key)".

Sonoff: works in DIY mode (firmware 3.5 or later), which takes the plug off the eWeLink cloud; Itead's DIY instructions for each model show how to turn it on. A Sonoff in its normal eWeLink mode encrypts its local traffic and "Check plug" says so. DIY mode has no power reading: for watts, flash the plug with Tasmota or ESPHome, or add it through Home Assistant.

ESPHome (Athom plugs ship with it): only the address, as long as the device YAML has "web_server:". The Bridge finds the relay and the power sensor itself; type their names only if it picks the wrong ones, and the login only if the web server has one.

myStrom: the address, plus the API token if you secured the plug with one in the myStrom app.

WiZ: the address, with "Allow local communication" turned on in the WiZ app settings.

Home Assistant: the Home Assistant URL (for example http://homeassistant.local:8123), a long-lived access token (Home Assistant: your profile, Security, Long-lived access tokens, Create token), the switch entity (for example switch.printer_plug) and, for watts, the plug's power sensor entity. This covers any plug Home Assistant can switch, including Zigbee, Matter, Hue, Meross and Sonoff plugs in eWeLink mode.

If "Check plug" fails: "did not answer" means the address is wrong or the plug is offline or on another network; a login, key or token message means that value is wrong. The plug and the machine running the Bridge must be on the same network (with Docker, use host networking for WiZ, which uses UDP).`,
	},
	{
		ID:       "virtual-printer",
		Title:    "Virtual printer: send files from OrcaSlicer or PrusaSlicer to FoxTrack",
		Keywords: []string{"virtual printer", "slicer", "orcaslicer", "orca", "prusaslicer", "bambu studio", "send", "upload", "print host", "physical printer", "8090", "api key"},
		Body: `The Bridge can look like an OctoPrint printer to a slicer, so the slicer's "send to printer" button sends the sliced file to FoxTrack instead. The file never goes to a printer directly: it shows on FoxTrack's Printers page under "From the slicer", where you attach it to a product and print it from FoxTrack.

Turn it on in Settings > Virtual printer. It needs the Bridge connected to FoxTrack (a Bridge token), because that is where the files go. The card then shows the address to type into the slicer (this computer's address with port 8090) and an API key with a Copy button.

In OrcaSlicer or PrusaSlicer: add a physical printer / print host of type OctoPrint, enter that address as the host and the key as the API key, and press Test. Then slice and use the send or upload button.

FoxTrack takes .gcode and .gcode.3mf files up to 50 MB, and at most 50 files can wait there at once. The slicer shows FoxTrack's answer when a file is refused: wrong file type, too large, storage full, too many waiting files, or a plan that does not include the Bridge.

Bambu Studio cannot send to it: Bambu Studio only talks to Bambu printers, not to OctoPrint hosts. Use OrcaSlicer for Bambu printers.

The virtual printer listens on port 8090 by default; the Bridge log says if the port could not be opened.`,
	},
	{
		ID:       "alarms",
		Title:    "Printer alarms: HMS codes and error messages",
		Keywords: []string{"alarm", "hms", "error code", "warning", "fault", "shutdown", "attention", "bell", "alert"},
		Body: `The Bridge reports what a printer says is wrong, and shows it on the printer's card and in FoxTrack's Printers page, Alarms view.

Bambu Lab: HMS codes (the printer's health messages) with their severity (fatal, serious, common, info) and the print error code when a print fails. The card shows the code as Bambu does, for example 0300_1200_0002_0001, with a "What it means" link to the Bambu wiki page for that code, and Bambu's own words for the code when the Bridge knows them.

Klipper: a firmware shutdown or error (the state message Klipper shows, for example "MCU 'mcu' shutdown: Timer too close") as a fatal alarm, and a print that ended in error as a serious one.

PrusaLink: the error or attention text the printer shows. OctoPrint: the printer error OctoPrint reports.

An alarm clears when the printer stops reporting it. Alarms come from the printer itself: one can clear before you reach the printer, so its screen may show nothing.

Bridge notifications (Telegram, Discord, ntfy and the rest): all the alarms a printer raises at once arrive as one message. Once they have been gone for 2 minutes, a quiet "Alarm cleared" message follows, to every channel that gets alarms. An alarm that flickers on and off sends no more than one "cleared" in 30 minutes. A printer that drops off the network again within 30 minutes of coming back is reported offline only after 15 minutes. The same text never goes to the same channel twice within 2 minutes.

In FoxTrack, a fatal or serious alarm rings the bell and sends the printer error alert (Telegram, WhatsApp, Discord, Slack or email, whichever is set up). Home Assistant printers report no alarms.`,
	},
	{
		ID:       "foxtrack-link",
		Title:    "Connecting the Bridge to FoxTrack",
		Keywords: []string{"foxtrack", "api key", "token", "sync", "cloud", "remote", "relay", "rejected", "revoked", "plan", "integrations"},
		Body: `Linking to FoxTrack is optional. The dashboard works fully offline without it.

To link: in FoxTrack, open Settings > Integrations > FoxTrack Bridge and create a Bridge token (it starts with ftb_ and is shown once). In the Bridge, open Settings, paste it into the Bridge token field, and click Save. The line under the field then says Connected to FoxTrack, or what to fix. The Bridge integration needs the FoxTrack Pro or Farm plan, and creating a token needs the owner or admin role.

Once linked, the Bridge relays telemetry, print history and camera snapshots to FoxTrack so printers can be monitored remotely. Each printer then appears on the FoxTrack Printers page: in the Live view, and in the Fleet view under "Detected on your network, not linked". Pick a FoxTrack printer in its "Link to printer" menu to link them.

Printers that already exist in FoxTrack should be linked, not deleted. Deleting a printer in FoxTrack also deletes its maintenance schedules, maintenance log and installed hardware. FoxTrack recognizes a Bambu printer by its serial number and a Klipper, PrusaLink or Home Assistant printer by its name in the Bridge, so the names do not have to match.

If the relay stops working the dashboard reports the problem rather than retrying silently. The three causes that never fix themselves on a retry are: the key was revoked, the key was mistyped, or the FoxTrack workspace is on a plan that no longer covers the Bridge. All three need action in FoxTrack, not in the Bridge.

A fourth warning is about a printer limit. A FoxTrack plan limits how many Bridge printers a workspace can have (5 on Pro, unlimited on Farm). Old devices left behind by a rename still count. To make room: in FoxTrack, open Printers, find the old offline device under "Detected on your network, not linked", and click Remove.

Pause, resume, stop and light commands sent from FoxTrack reach the Bridge within a few seconds (up to 30 seconds right after the page is opened), and FoxTrack shows the result. Home Assistant printers are read-only, so FoxTrack cannot send them commands. A command the Bridge does not pick up within 2 minutes expires, so it never fires late.`,
	},
	{
		ID:       "config-files",
		Title:    "Where settings and history are stored",
		Keywords: []string{"config", "config.json", "history.json", "queue.json", "file", "location", "folder", "directory", "backup", "backups", "corrupt", "lost", "missing", "printers gone", "port"},
		Body: `The Bridge stores config.json, history.json and queue.json (the print queue) in the standard per-user configuration directory:

- Linux: ~/.config/FoxTrack-Bridge/ (or $XDG_CONFIG_HOME/FoxTrack-Bridge if set)
- macOS: ~/Library/Application Support/FoxTrack-Bridge/
- Windows: %AppData%\FoxTrack-Bridge\

If config.json is not found there, the older location (the executable's directory, then config/config.json) is read automatically and migrated on the next save. This fallback is config-only. history.json has no equivalent, so history written by a very old install at the legacy location does not appear after upgrading.

A backups folder next to them holds a daily copy of config.json and history.json (config-YYYY-MM-DD.json, history-YYYY-MM-DD.json); the last 7 of each are kept. Settings > General > Back up and restore can restore from one of them.

If one of these files fails to load, for example because it is corrupted, the Bridge renames the broken file next to itself as <name>.corrupt-<timestamp> (config.json.corrupt-<timestamp>, for example) and starts fresh. It never deletes or overwrites the original, so a broken file can always be recovered by hand.

config.json is written with file mode 0600 because it holds printer access codes and API keys.

The listen port is 8080 by default. Change it with --port or FOXTRACK_BRIDGE_PORT; --port wins if both are set.`,
	},
	{
		ID:       "updates",
		Title:    "Updating the Bridge",
		Keywords: []string{"update", "upgrade", "version", "new version", "release", "restart", "docker", "self-update", "auto-update"},
		Body: `The Bridge checks GitHub for new releases and can install them with a staged update, then restart itself.

Automatic updates can be turned on or off in Settings.

Updates do not run inside Docker. The binary is not writable there, so self-update cannot work; pull a new image instead.

A build made without a version stamp reports its version as "dev" and has update checks disabled by design, because "dev" cannot be compared against a GitHub release number.

On Linux the Bridge repairs its own systemd unit on startup, so the unit file does not need editing by hand.`,
	},
	{
		ID:       "history-and-notifications",
		Title:    "Print history, notes and notifications",
		Keywords: []string{"history", "past prints", "record", "log", "note", "notes", "add a note", "note on a print", "comment", "csv", "export", "notification", "notify", "push", "alert", "finished", "browser", "discord", "telegram", "ntfy", "almost done", "minutes left", "quiet hours"},
		Body: `Print history is stored locally in history.json and shown in the History view. Each record holds the printer name, file name, start and end time, duration, result (finished, cancelled or error), filament and energy when known, and temperature samples taken during the print.

Each print can carry a note: click the pencil on its row in History, type, and click Save (an empty note removes it). Export CSV includes the notes.

History is only recorded while the Bridge is running. A print that finishes while the Bridge is stopped leaves no record.

Notification channels are set up in Settings > Notifications: Discord, Slack, Telegram, ntfy, Pushover, Gotify, email or a webhook. Each channel picks its printers and events (print started, almost done, paused, resumed, finished, failed, cancelled, failure spotted by the camera watch, printer alarm, offline, back online, maintenance due) and can attach a camera picture. During quiet hours only failures and errors are sent.

"Almost done" is off until it is ticked on a channel. It is sent once per print when the time left drops to the minutes set under Almost-done alert (10 by default), and only once the print is past halfway.

The dashboard can also raise browser notifications when prints finish, fail or pause. The browser must have granted permission for them.`,
	},
	{
		ID:       "assistant",
		Title:    "About this assistant and its AI settings",
		Keywords: []string{"assistant", "ai", "chat", "model", "provider", "openai", "gemini", "anthropic", "local", "ollama", "llama", "api key", "privacy", "cost"},
		Body: `This assistant runs against an AI provider that you configure and pay for. The Bridge ships with no provider and no key: until one is set in Settings > Assistant, the assistant is off.

Supported providers are anything that speaks the OpenAI chat-completions API: OpenAI, Google Gemini and Anthropic through their OpenAI-compatible endpoints, any custom endpoint, or a local model server on your own machine such as Ollama, llama.cpp or LM Studio.

The request goes out from the machine running the Bridge, straight to the provider. It does not pass through FoxTrack.

The camera tool is off by default. When it is switched on, the assistant can fetch a still frame from a printer camera and look at it, which is what lets it comment on how a print actually looks. With a cloud provider that means a picture of your printer leaves your network and goes to that provider. With a local model it stays on your machine. The provider must support image input for this to work at all.

The assistant can only read. It cannot add, change or delete a printer, cannot start, pause or stop a print, and cannot change any setting. It can only tell you what to press.

Unless a dashboard password is set in Settings > Security, anyone who can reach the dashboard on your network can use the assistant and spend credit on the configured key.

Watch prints for failures (Settings > Assistant, needs camera access) has the assistant look at each printing printer's camera every few minutes (5 by default, 2 to 60), starting 5 minutes into a print. After two checks in a row that look like a failed print, the channels that have "Failure spotted by camera watch" ticked get one notification for that print. It never pauses or stops a print. Each check sends a picture to the provider, which can cost money with a paid provider.`,
	},
	{
		ID:       "print-queue",
		Title:    "Print queue: lining up the next prints",
		Keywords: []string{"queue", "queued", "next", "up next", "start next", "line up", "batch", "farm", "after this print", "add to queue"},
		Body: `The print queue lines up files that are already on a printer. It works on printers that can start a file: Bambu Lab in LAN mode, Klipper and OctoPrint.

To add a file: open Start a print for the printer, type the name of a file on the printer, and click Add to queue. The printer's card then shows Up next with the first file. Open the list to move a file up or down or remove it. A printer holds up to 50 queued files.

When the printer is idle or finished, a Start next button appears. It asks whether the bed is clear, then starts the first file. The file only leaves the queue once the printer accepted the start; if the printer refuses, it stays first in line. The Bridge never starts a queued print by itself, because a print started on a full bed crashes into the previous one.

The queue is saved in queue.json next to config.json and survives a restart. Removing a printer drops its queue.`,
	},
	{
		ID:       "share-links",
		Title:    "Share links: a read-only view of one printer",
		Keywords: []string{"share", "link", "share link", "watch", "family", "friend", "read-only", "public", "view only", "revoke"},
		Body: `A share link lets someone watch one printer without access to anything else. Make one in Settings > Security > Share links: pick the printer, give it a name, and tick Show the camera if the page may show a camera picture (it refreshes every 10 seconds).

The page shows the printer's status, file, progress, time left and temperatures. It has no controls, shows no other printer, and never shows addresses, access codes or file lists. It works without the dashboard password, for anyone who can reach the Bridge's address, so it is meant for people on your network (or who can reach it some other way you set up).

The link is shown once, when it is made: copy it then. Only a hash of it is stored. Revoke stops it at once, and removing the printer stops it too. Up to 20 links can exist.`,
	},
	{
		ID:       "backup-restore",
		Title:    "Back up, restore and move the Bridge to another machine",
		Keywords: []string{"backup", "back up", "restore", "export", "import", "move", "migrate", "new computer", "transfer", "copy settings", "reinstall"},
		Body: `Settings > General > Back up and restore downloads the printers and settings as one file (foxtrack-bridge-backup-<date>.json), and Restore from file loads one back. The file holds the printers' access codes and API keys, so keep it private.

The dashboard password, API tokens, share links, the linked Bambu account and the assistant's API key are not in the file. A restore never changes the first four, and keeps the assistant's key unless the backup uses a different AI provider. Before a restore the current settings are saved next to config.json as config.json.before-import-<date>, so a restore can be undone by hand.

Restore also accepts a plain config.json, such as one of the daily copies in the backups folder next to config.json. A file with no printers is refused unless you confirm that every saved printer should be removed.

To move to another machine: download a backup on the old one, install the Bridge on the new one, and restore the file there.`,
	},
	{
		ID:       "getting-help",
		Title:    "Getting help from a person",
		Keywords: []string{"help", "support", "bug", "report", "issue", "github", "contact", "broken", "stuck", "feature request"},
		Body: `When the Bridge itself is at fault, or nothing in this help covers the problem, report it on GitHub: https://github.com/FoxesRCool1/FoxTrack-Bridge/issues

A useful report includes the Bridge version shown in the dashboard footer, the operating system, how the printer is connected (Bambu LAN, Bambu Cloud, Klipper, PrusaLink, OctoPrint, or Home Assistant), what was expected, what happened, and the Bridge's log output from around the time it happened.

Questions about the FoxTrack web app, billing or a workspace plan belong in FoxTrack, not here.

Printer error codes reported during a print come from the printer's own firmware. The printer manufacturer's documentation is the authority on those.`,
	},
}
