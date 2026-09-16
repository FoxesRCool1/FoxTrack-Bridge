# Changelog

## v2.3.1

**Updates (Linux)**
- Fixed the update restart on Linux. Pressing "restart to apply" — or letting
  auto-update restart the bridge — could leave the bridge stopped and still on
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

A bridge running a build from before v2.3.1 cannot update itself — the broken
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
