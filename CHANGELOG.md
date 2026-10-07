# Changelog

## Unreleased

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
