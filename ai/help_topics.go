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
		ID:       "edit-printer",
		Title:    "Changing a saved printer (new IP address, rename, new access code)",
		Keywords: []string{"edit", "change", "rename", "new ip", "ip changed", "update", "pencil", "modify", "access code changed", "wrong serial", "moved"},
		Body: `A saved printer can be changed without removing it. Click the pencil icon on the printer's card, change the details, and click Save.

What can be changed depends on the type. Bambu LAN mode: name, IP address, serial number and LAN access code. Bambu Cloud: name, and the IP address used for the camera. Klipper: name, Moonraker URL, Moonraker API key and webcam URL. The printer type itself cannot be changed; remove the printer and add it again for that.

The access code and API key fields start blank because the dashboard never receives saved secrets. Leaving them blank keeps the saved value.

Saving reconnects only that printer, and only if a connection detail changed. Other printers keep running.

A printer cannot be renamed during a print, because the running print is tracked under its name and its history record would be lost. The IP address can be changed during a print. Local print history from before a rename stays with the printer.

In FoxTrack, a renamed Bambu printer keeps its link because FoxTrack follows the serial number. A renamed Klipper printer shows up in FoxTrack as a new device, because FoxTrack knows Klipper printers by name. Changing a Bambu serial number has the same effect. To move the link: in FoxTrack, click the printer, set its Bridge device section to Unlinked, then link the new device from "Detected on your network, not linked" in the Fleet view. The old device stays under "Detected on your network, not linked" and shows as offline. A FoxTrack owner or admin can clear it with its Remove button, which appears once the device is offline.`,
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

A camera can be hidden per printer. That setting is saved on the Bridge, so it applies in every browser, and it only affects the dashboard; snapshot sending to FoxTrack is unaffected.

If a feed is black or blank: confirm the printer's own interface shows the camera, check the printer is reachable at all (an offline printer has no camera either), and for Klipper confirm the webcam URL is right and reachable from the Bridge's machine rather than only from your laptop.

In FoxTrack, the live picture only loads when the browser runs on the same computer as the Bridge. Everywhere else FoxTrack shows the latest snapshot. The Bridge sends a snapshot about every 25 seconds, and only while a print is running or paused, so an idle printer's snapshot does not update. Klipper printers send snapshots only when a webcam URL is set on the printer.`,
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

The Bridge never controls a print on its own. It can pause, stop, set speed and set fan for LAN-mode Bambu printers and Klipper printers, only when a person presses the button.`,
	},
	{
		ID:       "controls",
		Title:    "What the control buttons do, and which printers have them",
		Keywords: []string{"pause", "stop", "resume", "cancel", "light", "speed", "silent", "sport", "ludicrous", "fan", "gcode", "console", "command", "advanced"},
		Body: `Available controls depend on how the printer is connected.

Bambu Lab in LAN mode: pause, stop, light toggle, print speed (Silent, Standard, Sport, Ludicrous), fan speed, and a GCode console for single commands. The GCode console only appears in Advanced mode.

Bambu Lab over the cloud: the light toggle only. Pause, stop, speed, fan and GCode are hidden because Bambu's 2025 and later firmware rejects them from third-party clients. LAN mode is the only way to get those back.

Klipper / Moonraker: pause, stop and fan speed.

AMS slot display, the speed selector and the GCode console are Bambu-only. Fan control works on both Bambu and Klipper.`,
	},
	{
		ID:       "foxtrack-link",
		Title:    "Connecting the Bridge to FoxTrack",
		Keywords: []string{"foxtrack", "api key", "token", "sync", "cloud", "remote", "relay", "rejected", "revoked", "plan", "integrations"},
		Body: `Linking to FoxTrack is optional. The dashboard works fully offline without it.

To link: in FoxTrack, open Settings > Integrations > FoxTrack Bridge and create a Bridge token (it starts with ftb_ and is shown once). In the Bridge, open Settings, paste it into the FoxTrack sync field, and click Save. The Bridge integration is a FoxTrack Pro feature, and creating a token needs the owner or admin role.

Once linked, the Bridge relays telemetry, print history and camera snapshots to FoxTrack so printers can be monitored remotely. Each printer then appears on the FoxTrack Printers page: in the Live view, and in the Fleet view under "Detected on your network, not linked". Pick a FoxTrack printer in its "Link to printer" menu to link them.

Printers that already exist in FoxTrack should be linked, not deleted. Deleting a printer in FoxTrack also deletes its maintenance schedules, maintenance log and installed hardware. FoxTrack recognizes a Bambu printer by its serial number and a Klipper printer by its name in the Bridge, so the names do not have to match.

If the relay stops working the dashboard reports the problem rather than retrying silently. The three causes that never fix themselves on a retry are: the key was revoked, the key was mistyped, or the FoxTrack workspace is on a plan that no longer covers the Bridge. All three need action in FoxTrack, not in the Bridge.

A fourth warning is about a printer limit. A FoxTrack plan limits how many Bridge printers a workspace can have (5 on Pro, unlimited on Enterprise). Old devices left behind by a rename still count. To make room: in FoxTrack, open Printers, find the old offline device under "Detected on your network, not linked", and click Remove.

Pause, resume, stop and light commands sent from FoxTrack reach the Bridge within a few seconds, and FoxTrack shows the result. A command the Bridge does not pick up within 2 minutes expires, so it never fires late.`,
	},
	{
		ID:       "config-files",
		Title:    "Where settings and history are stored",
		Keywords: []string{"config", "config.json", "history.json", "file", "location", "folder", "directory", "backup", "corrupt", "lost", "missing", "printers gone", "port"},
		Body: `The Bridge stores config.json and history.json in the standard per-user configuration directory:

- Linux: ~/.config/FoxTrack-Bridge/ (or $XDG_CONFIG_HOME/FoxTrack-Bridge if set)
- macOS: ~/Library/Application Support/FoxTrack-Bridge/
- Windows: %AppData%\FoxTrack-Bridge\

If config.json is not found there, the older location (the executable's directory, then config/config.json) is read automatically and migrated on the next save. This fallback is config-only. history.json has no equivalent, so history written by a very old install at the legacy location does not appear after upgrading.

If either file fails to load, for example because it is corrupted, the Bridge renames the broken file next to itself as config.json.corrupt-<timestamp> or history.json.corrupt-<timestamp> and starts fresh. It never deletes or overwrites the original, so a broken file can always be recovered by hand.

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
		Title:    "Print history and browser notifications",
		Keywords: []string{"history", "past prints", "record", "log", "notification", "notify", "push", "alert", "finished", "browser"},
		Body: `Print history is stored locally in history.json and shown in the History view. Each record holds the printer name, file name, start and end time, duration, result (finished, cancelled or error), and temperature samples taken during the print.

History is only recorded while the Bridge is running. A print that finishes while the Bridge is stopped leaves no record.

The dashboard can raise browser push notifications when prints finish, fail or pause. These are browser notifications, so the browser must have granted permission and the dashboard tab's origin must be allowed to send them.`,
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

The Bridge dashboard has no login. Anyone who can reach it on your network can use the assistant and spend credit on the configured key.`,
	},
	{
		ID:       "getting-help",
		Title:    "Getting help from a person",
		Keywords: []string{"help", "support", "bug", "report", "issue", "github", "contact", "broken", "stuck", "feature request"},
		Body: `When the Bridge itself is at fault, or nothing in this help covers the problem, report it on GitHub: https://github.com/FoxesRCool1/FoxTrack-Bridge/issues

A useful report includes the Bridge version shown in the dashboard footer, the operating system, how the printer is connected (Bambu LAN, Bambu Cloud, or Klipper), what was expected, what happened, and the Bridge's log output from around the time it happened.

Questions about the FoxTrack web app, billing or a workspace plan belong in FoxTrack, not here.

Printer error codes reported during a print come from the printer's own firmware. The printer manufacturer's documentation is the authority on those.`,
	},
}
