# Changelog

## Unreleased

The Bridge now stands on its own as a full printer dashboard: everything below
works without an account, a cloud or the FoxTrack app, and every feature is
off until you turn it on. Existing configs load unchanged.

**Alerts and watching**
- Notifications to Discord, Slack, Telegram, ntfy, Pushover, Gotify, email
  and webhooks, for prints started, paused, resumed, finished, failed or
  cancelled, printer errors, printers going offline and maintenance due. With
  a camera picture, per-printer and per-event filters, quiet hours and a test
  button. Prints started on the printer screen are covered too.
- Bambu HMS and print error codes show Bambu's own description (4,928 HMS and
  959 print error codes), on the card, in notifications and in FoxTrack.
- A Bambu printer that refuses control (Developer Mode off) says so on its
  card, with what to change. Pause and stop are always sent.
- Connection problems in plain words: a wrong access code, an unreachable
  address, a TLS failure or a wrong serial number. The MQTT keepalive is now
  30 s with a 10 s ping timeout.
- Cameras for the X1, H2 and P2S series (RTSP, through ffmpeg; included in
  the Docker image). One stream per printer is shared by every viewer.
- Timelapse of every print, made by the Bridge: MP4 with ffmpeg, an animated
  picture without it. Gallery with playback, download and delete.
- Skip objects during a print: Bambu (LAN Only Mode with Developer Mode) and
  Klipper (`[exclude_object]`), with a drawing of the plate.

**Records**
- Print history records filament (grams per spool for Bambu, length for
  Klipper and OctoPrint) and smart-plug energy. History shows what each print
  used and cost; the CSV export has the same columns.
- Costs: price per kg (per material) and per kWh, with a 30-day summary.
- Maintenance reminders after hours of printing, with suggested tasks per
  printer.
- Spoolman: filament used is booked to the mapped spool after each print,
  matched by RFID tag when no spool is mapped.

**Connecting**
- Home Assistant over MQTT with discovery: each printer is a device with
  sensors, pause, resume, stop and the light.
- Prometheus metrics at `/metrics` (off by default).
- Optional dashboard password, with API tokens for scripts. `--reset-password`
  removes a forgotten one.

**Dashboard**
- Settings is grouped into Printing, Connect and Bridge; links name the
  section (`#settings/notifications`) and Back works between views and closes
  dialogs.
- Command menu (Ctrl+K or ⌘K) and keyboard shortcuts (`?`).
- English, Deutsch, Español, Français, Italiano, Nederlands and Português.
- The FoxTrack app is one optional section: the header chip shows only once
  it is linked, and the welcome screen no longer asks for a token up front.
- Phones: dialogs are sheets that a swipe down closes, notches and home bars
  are respected, and every control has a 44 px touch area.
- Card-shaped placeholders while loading, steady numbers, balanced headings.

**Security and maintenance**
- Go 1.26 and golang.org/x/net 0.62 (17 known vulnerabilities in the old
  version, none reachable from the Bridge). CI and Docker build with the
  version in go.mod.
- Third-party GitHub Actions are pinned to commit hashes.
- With a password set, the dashboard cannot be framed by another site.
- Control commands carry an idempotency key, so a retried click runs once.
- Removed unused code (the old v1 status webhook, an unused MQTT helper).

## v2.5.3

**Smart plugs**
- A Bambu printer added through a Bambu account can now have a smart plug:
  the Smart plug section shows on its edit form. The plug is switched on your
  network, so it works the same as on a LAN printer.
- The dashboard card shows the plug, and its Plug on button, while the printer
  itself is off or not reporting. Before, it waited for a printer report that
  never came once the plug had cut the power.
- FoxTrack keeps seeing a printer its plug switched off: while the printer is
  quiet and the plug is off, the Bridge reports the plug itself every minute
  (gcode_state OFFLINE, only the plug commands), and once more when the plug
  comes back on. FoxTrack shows Powered off and Turn plug on keeps working;
  before, the printer went to Bridge offline with every button off.
- The dashboard shows such a printer as Powered off instead of its last
  state (a Bambu account printer kept showing Idle).

**Other changes**
- Settings has been revamped to be simpler and cleaner.
- Shorter and condensed text everywhere.
- Printer cards show Pause, Resume, Stop and Start print only when they
  apply. The light button always reads Light; its colour shows on or off.
- Colours match the FoxTrack web app again.

## v2.5.2

The v2.7.0 notes below were never released on their own. Everything in them ships in v2.5.2.

**Printer console: temperatures and home**
- FoxTrack's opened printer, and the card's new Temps panel, can set the
  nozzle target (0 to 350 C) and the bed target (0 to 120 C; 0 turns the
  heater off) and home the axes. Home is refused while a print runs. Bambu Lab in LAN mode (M104 / M140 / G28),
  Klipper (the same G-code through Moonraker) and OctoPrint (its printer API).
  PrusaLink has no such call and answers "This printer does not take
  temperature commands from the Bridge"; Home Assistant stays read-only. A
  target out of range is refused before anything is sent. The Bridge announces
  `console` to FoxTrack, which hands these commands only to a Bridge that
  lists it.

**Files on the printer**
- FoxTrack's opened printer lists the files already on the printer (name,
  size, date, newest first, up to 200) and can start one with a Print button.
  Bambu Lab in LAN mode: the `.3mf` files on the card and in its cache folder,
  read over FTPS; Klipper: Moonraker's gcodes folder; OctoPrint: its local
  storage. The list is read only when FoxTrack asks, never in the poll loop.
  A print is refused while the printer prints or is paused. PrusaLink and
  Home Assistant printers are not listed. The Bridge announces `files`.
  A Bambu `.3mf` starts on the plate it was sliced for (read from the file),
  unless FoxTrack names a `plate`.

**Fixes**
- Security: the dashboard answers only to a LAN name or address (an IP,
  localhost, a name with no dot, .local, .lan, .home.arpa, .internal, or this
  computer's name). A web page can no longer reach it by pointing its own
  domain at your computer (DNS rebinding). Behind a reverse proxy on a real
  domain, list that name in `FOXTRACK_BRIDGE_ALLOWED_HOSTS` (comma-separated).
- Security: the connection check uses a saved access code, API key or token
  only for the saved printer's own address, so it can no longer be sent to
  another host.
- Security: a file name with a line break or a quote is refused, so it can no
  longer slip a second command to a Bambu printer's FTP server or into a
  Klipper print command.
- Security: the OctoPrint API key goes only to OctoPrint itself, never to a
  webcam on another machine or along a redirect.
- Security: another web site can no longer start a network scan.
- Safety: Plug off, auto-off and Home are refused while a Bambu print is
  starting (PREPARE, SLICING) and whenever the printer's state is unknown. With
  the printer unreachable, a remembered state older than 2 minutes no longer
  counts.
- Console commands run beside the command loop: a Klipper Home no longer holds
  up pause and stop for the other printers, and a lost reply to FoxTrack no
  longer runs the command a second time.
- The connection check no longer blames the serial number when a Bambu printer
  is only slow to answer.
- OctoPrint: one slow reply no longer drops the webcam address and model for
  5 minutes.
- Smart plug check: the watts read right (it showed 10 W for 100 W).
- Network scan: a saved OctoPrint printer, or one saved with spaces around its
  address, shows as added; a Bambu printer found by its certificate gets a
  name; a printer that moved to a remembered address is no longer hidden.
- Printer replies are capped at 16 MB.
- Safety: a smart plug can no longer be switched off, or auto-off, while the
  Bridge has lost the printer mid-print. With the printer unreachable the
  Bridge goes by the last state it saw, and refuses if it never saw one.
- Security: the dashboard API refuses writes a browser sends from another
  site, so a web page can no longer change the FoxTrack token or clear the
  printers. Request bodies are capped at 1 MB.
- A config save that cannot be written to disk now shows an error instead of
  "saved". An unreadable (not corrupt) config stops the Bridge instead of
  being moved aside; a corrupt config falls back to an older valid copy.
- Updates install only when the release's checksums are published, and never
  inside Docker.
- Bambu Lab: a target temperature of 0 now shows as 0; a print that ends while
  the printer is offline is still recorded; partial AMS reports no longer wipe
  the other trays; no blank status is posted right after a reconnect; error
  codes no longer flicker; renaming a printer mid-print keeps the print.
  Listing files works on the A1 and A1 mini.
- Bambu Cloud: failed device lookups and token sign-ins are paced like the
  rest, and a cloud printer that moved to a new IP is found again.
- Klipper: Home no longer reports a failure while the printer homes; lifetime
  print hours are read from the right Moonraker endpoint; the camera stream is
  no longer cut every 8 seconds.
- PrusaLink and OctoPrint: a blank or offline status gets the same grace as a
  lost connection instead of cancelling the print; OctoPrint keeps the file
  name and progress on a finished print and lists only printable files.
- FoxTrack posts: status posts for one printer always arrive in order, and a
  finished print's history is retried for up to 24 hours.
- Files and console commands report their result even after a FoxTrack blip.
- Dashboard: a printer name with "/" or "%" works with every button; a slow
  status poll can no longer show older data; the Auto-update switch no longer
  saves half-typed tokens.
- Virtual printer: read timeouts, upload size and concurrency limits.

## v2.7.0 notes (never released on their own; ship in v2.5.2)

**OctoPrint printers**
- Printers run by OctoPrint can be added with their address and an API key.
  The Bridge reports status, progress, temperatures, file name and time left,
  records finished prints, can pause, resume and stop, and accepts a `.gcode`
  file sent from FoxTrack (uploaded to OctoPrint and started). The webcam is
  read from OctoPrint's own settings. The network scan finds OctoPrint on
  ports 80 and 5000.

**Printer alarms**
- Every status report now carries the printer's open alarms, and FoxTrack
  lists them in its Alarms view and rings the bell for serious ones: Bambu HMS
  codes (formatted as Bambu does, with their severity) and print error codes,
  Klipper firmware shutdowns and print errors with Klipper's own message,
  PrusaLink error and attention text, OctoPrint printer errors. The printer
  card shows them too; a Bambu HMS code links to the Bambu wiki page for it.

**Smart plugs**
- A printer can be powered through a Shelly, Tasmota or Kasa (older firmware)
  plug, set on the printer in the Bridge. The card shows on/off and watts, a
  Plug on / Plug off button (off is refused while printing, paused or hot),
  and the Bridge can turn the plug off a set number of minutes after a print
  once the printer is cool. The energy each print used goes to FoxTrack with
  the print history (Reports, Printer use). FoxTrack's own Turn plug on/off
  buttons work through the Bridge.

**Virtual printer**
- Settings, Virtual printer: the Bridge can look like an OctoPrint host to
  OrcaSlicer and PrusaSlicer (port 8090, its own API key). A file the slicer
  sends goes to FoxTrack's Printers page, From the slicer, never to a printer.
  Needs the Bridge connected to FoxTrack.

The Bridge now announces `alarms`, `plug_on` and `plug_off` to FoxTrack, which
hands plug commands only to a Bridge that lists them. Config changes are
additive: an existing `config.json` loads unchanged.

**Easier setup**
- A first-run welcome finds printers on your network; click Add and type the
  access code or API key. FoxTrack is optional: connect it or skip it.
  Bambu printers are found on the first scan, even on weak Wi-Fi or with
  Bambu Studio open on the same computer.
- The Bridge checks the connection before saving a printer and tells you what
  to fix (wrong access code, wrong API key, unreachable address).
- A header chip shows whether FoxTrack is connected or the Bridge is
  standalone, and you can paste an `ftb_` token to connect.
- A Settings card, "Open on another device", lists the addresses to use from
  your phone or another computer.
- The startup log shows the dashboard address on your network too.
- `docker-compose.yml` runs the published image with host networking, so
  printers can be found from Docker.
- The Linux installer can start the service for you and open the dashboard
  (`--yes` skips the question).

## v2.5.1

**PrusaLink and Home Assistant printers**
- Prusa printers with PrusaLink (MK4, XL, MINI, Core One) can be added with
  their address and API key. The Bridge reports status, progress, temperatures,
  file name and time left to FoxTrack, records finished prints in the history,
  and can pause, resume and stop a print. It also accepts a `.gcode` file sent
  from FoxTrack, puts it on the printer's USB drive and starts it.
- Printers that Home Assistant already shows can be added read-only: enter the
  Home Assistant address, a long-lived access token and the entity IDs that
  hold the printer's state and, if you have them, progress, temperatures, file
  name and time left. FoxTrack sees the status; pause, stop and printing a file
  are not available for these printers.
- Choose **Prusa (PrusaLink)** or **Home Assistant (read-only)** as the
  printer type when adding a printer. Both can be edited later like any other
  printer, and the Home Assistant token stays on this computer.

**Filament use**
- When a print ends, the Bridge tells FoxTrack how far it got: the progress when
  it stopped, or 100 for a finished print. A cancelled or failed print no longer
  looks like a whole one.
- Bambu Lab: the Bridge also sends how full each AMS slot and the external spool
  was at the start and at the end of the print, so FoxTrack can take the grams
  used off the right spool. A slot the printer cannot measure is sent as unknown.
- Klipper: the Bridge also sends the length of filament the print used.
- Klipper: a short Moonraker or network outage during a print (up to ten
  minutes) no longer ends the print early. The print reaches FoxTrack once,
  with its whole filament length, instead of as a cancelled part plus a
  second print that repeats the total.
- The Bridge announces this to FoxTrack as the new `filament_use` capability.

**Spool identity (Bambu Lab)**
- For each AMS slot and the external spool, FoxTrack now gets the spool's RFID
  tag, its Bambu filament name (for example PLA Basic), its net weight and its
  filament profile id. FoxTrack can use them to add a Bambu Lab spool from its
  tag. A spool without a tag sends none.

**Print history**
- Pausing a print no longer ends it in the history: a paused and resumed print is
  one print, not a cancelled one followed by a new one.
- Bambu Lab: if the printer connection drops for under 10 minutes during a
  print, the print keeps its start time and its history when the connection
  comes back.

## v2.5.0

**Print from FoxTrack**
- FoxTrack can now send a sliced file to a printer and start it. The Bridge
  downloads the file from FoxTrack, checks its size and checksum, puts it on the
  printer and starts the print. Bambu Lab printers take a Bambu Studio
  `.gcode.3mf` file (LAN Only Mode and Developer Mode on, the same setup pause
  and stop need). Klipper printers take a `.gcode` file through Moonraker.
- Bambu Cloud printers cannot be sent a file, and FoxTrack does not offer them.
- The Bridge keeps the files it downloaded in a `print-cache` folder in its
  config folder (at most 20 files and 1 GB, oldest removed first), so printing the
  same file again does not download it again.
- A Bambu printer stores every file sent from FoxTrack under one name
  (`foxtrack-print.gcode.3mf`), so its SD card does not fill up. The printer
  screen still shows your file's name.
- The Bridge reports the result of a print to FoxTrack for up to about 3 minutes
  if FoxTrack cannot be reached, and a print counts as started as soon as the
  printer leaves idle.
- The download link must be on the FoxTrack address the Bridge uses and is never
  redirected, and it is never shown in an error message.
- Progress shows in the Bridge log, and a failed job tells you why in FoxTrack.
- For testing, the `FOXTRACK_SUPABASE_URL` environment variable points the Bridge
  at another FoxTrack project (https, or http on this computer only).
- The Bridge checks that the printer is idle before it downloads the file, so a
  busy printer never costs a download.
- A Bambu printer's file is deleted before the new one is sent, so the second
  print from FoxTrack no longer fails on the same file name.
- If a Bambu printer refuses the start command, FoxTrack shows the printer's
  reason, and the Bridge log shows the printer's full answer.
- A Klipper printer that puts the file in its queue instead of starting it is
  reported as failed, and a file that is printing right now is named as the reason
  instead of a wrong API key.
- Error messages in FoxTrack are plain words; the technical detail goes to the
  Bridge log.
- Clearer failures: an expired download link says to press Print again, a file
  FoxTrack no longer has says so, and a full disk is no longer blamed on the
  download.
- If the printer connection drops for a moment during a long upload, the Bridge
  waits up to 15 seconds for it to come back instead of failing the print.
- The job gives up after 12 minutes (was 14), so its result always reaches
  FoxTrack before FoxTrack's own 15-minute limit. A big file gets longer, the
  same as FoxTrack gives it: a 50 MB file about 50 minutes, enough for a slow
  printer Wi-Fi. Uploads to the printer are no longer cut at 10 minutes
  (Klipper) or 15 minutes (Bambu Lab); the job's own limit ends them.
- A Klipper file name made only of non-ASCII letters is sent as `print.gcode`.

**Set filament (Bambu Lab, LAN Only Mode + Developer Mode)**
- Click an AMS slot, or the new external spool dot, on a printer card to set its
  material and color. The Bridge waits until the printer reports the change, and
  says so if the printer refuses it (usually Developer Mode is off).
- FoxTrack can do the same through the new `set_filament` command, and now sees
  the external spool. A changed material or color reaches FoxTrack at once.
- If the printer drops offline while it waits, or does not take the change, the
  message says so. Bambu Lab spools with a tag set their own filament.
- Not yet: AMS HT slots, and the external spools of dual-nozzle printers (H2D).

**Connecting to FoxTrack**
- Settings checks the Bridge token when you save it and shows **Connected to
  FoxTrack.**, or what to fix: a wrong token, a plan without the Bridge, or no
  internet. Before, a wrong token was silent until a printer reported.
- A refused token or plan also shows the warning on the Printers page before any
  printer has reported, and the warning clears as soon as FoxTrack accepts the
  token again.
- A newly saved token is tried at once. Before, the Bridge could wait up to 5
  minutes after FoxTrack refused the old one.
- The FoxTrack link in Settings opens the Integrations page directly (it went to
  a page that does not exist), and the GitHub link points at the right repository.
- The token field is called **Bridge token**. The old **FoxTrack (legacy)**
  field only shows on installs that still use it.
- FoxTrack's plan names are right everywhere: Pro and Farm.

**Known (not fixed yet)**
- Files stay on the printer after the print: `foxtrack-print.gcode.3mf` on the
  Bambu SD card, and one file per name in the Klipper gcodes folder.
- A Bambu print counts as started at PREPARE/SLICING. An AMS mapping error that
  shows up later, during heat-up, is not reported back to FoxTrack.

## v2.4.0

**Assistant (new, experimental)**
- Added an assistant to the dashboard. It answers questions about your printers,
  walks you through connecting one, diagnoses a printer that will not come
  online, and can look at a printer's camera to tell you how a print actually
  looks. Open it with the sparkle button in the top bar.
- You supply the AI provider and pay for it; the Bridge ships with none and the
  assistant stays off until you set one up in **Settings > Assistant**. Anything
  that speaks the OpenAI chat-completions API works: OpenAI, Google Gemini and
  Anthropic through their OpenAI-compatible endpoints, any custom endpoint, or a
  local model on your own machine (Ollama, llama.cpp, LM Studio). Requests go
  from the machine running the Bridge straight to the provider and never through
  FoxTrack.
- The assistant is read-only. It cannot add, edit or remove a printer, cannot
  start, pause or stop a print, and cannot change a setting. It can only tell you
  what to press.
- It answers from tools and from a help library built into the binary, never from
  memory: it will say it does not know rather than invent a printer, a
  temperature or a log line, and it will not guess what a printer error code
  means.
- Letting it look at printer cameras is a separate switch and is **off** by
  default. With a cloud provider, switching it on sends a photograph of your
  printer to that provider; with a local model it stays on your machine. Every
  frame sent is written to the Bridge log.
- Stored secrets (printer access codes, API keys, the Bambu token) are stripped
  out of log lines before the assistant ever sees them.
- Your provider key is only ever sent to the provider it was saved for. If you
  change the provider or its base URL without entering a new key, the saved key
  is cleared. The assistant accepts requests only from the dashboard itself, so
  another website you visit cannot change its settings or use your key.
- Note: the dashboard has no login, so anyone who can reach it on your network
  can use the assistant and spend credit on the key you save.

**Printers**
- You can edit a saved printer. Click the pencil icon on its card to change the
  name, IP address, serial number, access code, Moonraker URL or webcam URL. You
  no longer have to remove a printer and add it again when its IP address
  changes. Leave the access code or API key blank to keep the saved one.
- Saving reconnects only the printer you edited, and only when a connection
  detail changed. Other printers keep running.
- A printer cannot be renamed during a print, because that print's history
  record would be lost. Changing the IP address during a print is allowed.
- Print history for a renamed printer still includes the prints from before the
  rename. For that reason a printer cannot take a name that another printer had
  before.
- Two saves at the same moment can no longer write an older copy of your
  settings over a newer one. Before, a printer you had just added could go
  missing from the file until the next save.

**FoxTrack Connection**
- Fixed Bambu Lab printers showing as "Unknown" in FoxTrack. Most updates from a
  Bambu printer leave out its state, and the Bridge sent that blank state on to
  FoxTrack. A printer showed the right state for a moment after each full
  report, then went back to "Unknown". The Bridge now always sends the printer's
  last known state.
- The log no longer says "skipping webhook: API key not configured" for
  printers that use a FoxTrack Bridge token. A printer with no key at all logs
  that once.
- The log no longer shows "MQTT skip (no usable data)" for empty `{}` messages,
  which some Bambu firmware sends.
- The Bridge now talks to FoxTrack much less when nobody is looking. While a
  FoxTrack page with your printers is open, it sends updates about every 10
  seconds and checks for Pause and Stop about every 5 seconds. Otherwise it
  sends status changes at once and everything else once a minute, which keeps
  printers online, and it checks for commands every 30 seconds. FoxTrack tells
  the Bridge which pace to use, so it catches up within 30 seconds of someone
  opening the page.
- Camera snapshots follow the same rule: about every 30 seconds while someone is
  looking, every 10 minutes otherwise.
- When FoxTrack turns the Bridge token down (revoked, or a plan without Bridge),
  the Bridge stops retrying every few seconds and asks again every 5 minutes.

**Camera**
- A Bambu Lab camera that sends no picture now fails after 15 seconds with
  "Camera unavailable", instead of loading forever. The Bridge log says why.
  A stream that stops sending pictures for 30 seconds is closed.
- The "Camera unavailable" message on Bambu Lab cards says which models work
  (A1 and P1 series) and what to check.
- FoxTrack camera snapshots stop after 3 failed tries in a row and try again
  every 10 minutes. X1 and H2 series printers no longer log
  "snapshot capture: header read: EOF" every 25 seconds during a print.

**Docs**
- Added [Using FoxTrack Bridge with FoxTrack](docs/foxtrack.md): setup, linking
  printers you already have in FoxTrack, editing printers, cameras and
  troubleshooting.
- The Settings page now names the right place to create a Bridge token and
  links to the guide.
- The guide and the assistant now describe FoxTrack commands correctly (they
  reach the Bridge in seconds and expire after 2 minutes), explain the printer
  limit warning, and say how to remove an old device in FoxTrack after a rename.

## v2.3.1

**Updates (Linux)**
- Fixed the update restart on Linux. Pressing "restart to apply", or letting
  auto-update restart the bridge, could leave the bridge stopped and still on
  the old version. The bridge handed the install to a helper script that it
  started as a child process. Under systemd that script lived in the service
  cgroup, so systemd killed it along with the service before it could put the
  new binary in place. The bridge now swaps the binary itself, before it exits.
- The bridge repairs its own systemd unit on startup. Units shipped before
  v2.3.1 used `Restart=on-failure`; the update path exits cleanly on purpose, so
  systemd read that as a deliberate stop and left the bridge down. The bridge
  now writes a drop-in beside the unit setting `Restart=always`. Your own unit
  file is not modified, and you can delete the drop-in at any time.
- The shipped systemd units now use `Restart=always` for new installs.
- Under systemd the bridge no longer relaunches itself after an update, so a
  second unsupervised copy can no longer race the one systemd starts.

**If your bridge is stuck on an older version**

A bridge running a build from before v2.3.1 cannot update itself: the broken
updater is the part that would have to do it. Install once by hand and every
update after this one works normally:

```
curl -fsSL https://raw.githubusercontent.com/FoxesRCool1/FoxTrack-Bridge/main/linux/install.sh | sh
```

Your printers, settings and print history are not touched.

## v2.3.0

**Dashboard**
- Updated the UI to match FoxTrack's new UI.
- Printer cards update in place instead of being rebuilt every 5 seconds. Open camera, G-code, speed and fan panels stay open.
- You can hide the camera for one printer. The setting is saved on the bridge, so it applies in every browser. FoxTrack still gets the snapshots.
- A camera stream now really closes when you hide it or remove the printer.
- The auto-update switch now shows its real state. Before, it always showed as on.
- Removing several printers at once no longer stops at the first failure. It now tells you how many failed.
- The empty state no longer sits in the wrong place on wide screens.

**Printers**
- Each printer now has a fixed ID that does not change when you rename it. A rename keeps the saved LAN code and API key.
- A new printer can no longer take a name that another printer already uses.
- A printer with a blank name is refused. Before, it broke controls, camera and history.
- Deleting a printer that does not exist now reports an error instead of saying "ok".

**Bambu Cloud (new)**
- You can add Bambu printers through your Bambu account when the printer is not in LAN mode (experimental).
- Sign in under Settings > Bambu Cloud, with your password and the emailed code, or a pasted token.
- The bridge learns the printer's LAN IP from its own reports, so the camera works on the same network.
- Only the light can be controlled over the cloud on current firmware. The other buttons are hidden.
- One connection per account, slow retries with a hold after repeated failures, and a stop on auth errors.

**FoxTrack Connection**
- Klipper printers no longer go offline on the website while idle. The bridge now sends telemetry at least once a minute.
- Controls on the website now work against the bridge on your network. Before, every command failed in the browser, ran once anyway, then ran a second time from the cloud queue.
- Token and plan errors now show on the dashboard. Before, they failed in silence and the dashboard looked healthy.
- A camera snapshot over 2 MB is now refused on the bridge with a clear reason, instead of being re-sent every 25 seconds.

**Stability**
- Fixed a crash that could take down the whole bridge, and every printer with it, when a Bambu LAN connection dropped twice.
- Fixed a connection leak on Klipper and webcam polling that would run the bridge out of file handles over time.
- Saving config can no longer write a half-finished printer list when two changes happen at once.
- Saving Settings can no longer turn auto-update off by accident.

**Updating**
- A failed update no longer leaves you with no app on macOS or a broken file on Windows. The new version is put in place first, then swapped in.
- A failed download now cleans up after itself instead of leaving the files on disk.
- Builds made from source no longer try to update themselves into a release build. The toggle is disabled and says why.

**Linux and Raspberry Pi**
- New one-command installer at linux/install.sh. It checks the download, installs under your home folder, needs no sudo, and supports --uninstall.
- New per-user systemd service, foxtrack-bridge@.service, plus a desktop launcher entry.
- New ARM32 build for ARMv7 boards. Note: it does not run on ARMv6 (Pi 1, original Pi Zero).
- You can change the port with --port or FOXTRACK_BRIDGE_PORT. It was fixed at 8080 before.
- The README was rewritten to be more accurate.
