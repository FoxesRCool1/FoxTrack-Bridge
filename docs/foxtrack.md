# Using FoxTrack Bridge with FoxTrack

FoxTrack Bridge runs on a computer on your network and talks to your printers.
When you connect it to [FoxTrack](https://foxtrack.studio), your printers show up
in FoxTrack with live status, print history and camera pictures. You can also
pause, resume, stop and toggle the light from FoxTrack.

The Bridge works on its own too. Connecting it to FoxTrack is optional.

- [Before you start](#before-you-start)
- [Set it up](#set-it-up)
- [I already have printers in FoxTrack](#i-already-have-printers-in-foxtrack)
- [Change a printer later](#change-a-printer-later)
- [Cameras](#cameras)
- [Troubleshooting](#troubleshooting)

## Before you start

- **A FoxTrack Pro or Enterprise workspace.** The Free plan does not include the
  Bridge.
- **Owner or admin access** in that workspace, to create a Bridge token.
- **A computer that stays on**, on the same network as your printers. A desktop,
  a spare laptop or a Raspberry Pi all work. FoxTrack only gets updates while the
  Bridge is running.

## Set it up

### 1. Create a Bridge token in FoxTrack

1. In FoxTrack, open **Settings → Integrations → FoxTrack Bridge**.
2. Click **Generate token**, give the token a name, and click **Generate**.
3. Copy the token now. It starts with `ftb_`, and FoxTrack shows it only once.

Treat the token like a password. If it leaks, revoke it on the same page and
create a new one.

### 2. Install and open the Bridge

Follow the [Quick start](../README.md#quick-start) in the README. When the Bridge
runs, open its dashboard at [http://localhost:8080](http://localhost:8080).

### 3. Paste the token into the Bridge

1. In the Bridge dashboard, open **Settings**.
2. Paste the token into **FoxTrack sync**.
3. Click **Save**.

### 4. Add your printers to the Bridge

In the Bridge dashboard, open **Printers** and click **Add printer**. See
[Adding printers](../README.md#adding-printers) for the details each printer type
needs.

### 5. Link each printer in FoxTrack

Within a few seconds, each printer shows up in FoxTrack on the **Printers** page:

- The **Live** view shows a tile for every printer the Bridge reports.
- The **Fleet** view lists new ones at the bottom under **Detected on your
  network, not linked**.

To link one, pick a FoxTrack printer in its **Link to printer…** menu. A linked
printer shows live status, the camera and the controls in its detail view. You
can change or remove the link later from the printer's **Bridge device**
section.

If you have not added the printer in FoxTrack yet, add it under **Fleet** first,
then link it.

## I already have printers in FoxTrack

**Do not delete them.** Add the printers to the Bridge, then link each Bridge
printer to the FoxTrack printer you already have (step 5 above).

Deleting a printer in FoxTrack also deletes its maintenance schedules,
maintenance log and installed hardware. Its orders, print queue items and print
history stay, but they are no longer assigned to a printer. Linking keeps all of
it.

The names do not have to match. FoxTrack recognizes a Bambu Lab printer by its
serial number, and a Klipper printer by the name you gave it in the Bridge.

## Change a printer later

If a printer gets a new IP address, or you want to rename it, you do not need to
remove it and add it again.

1. In the Bridge dashboard, click the pencil icon on the printer's card.
2. Change the details.
3. Click **Save**. The Bridge reconnects the printer if its connection details
   changed.

Good to know:

- **Leave the access code or API key blank** to keep the saved one.
- **Renaming a Bambu Lab printer is safe.** FoxTrack keeps the link and shows the
  new name.
- **Renaming a Klipper printer makes FoxTrack see a new device,** because
  FoxTrack knows Klipper printers by name. See
  [Move a link to the new device](#move-a-link-to-the-new-device).
- **Print history in the Bridge** from before a rename stays with the printer.
- **You cannot rename a printer during a print.** Finish or stop the print first.
  You can change the IP address at any time.
- **Changing a Bambu Lab serial number** also makes FoxTrack see a new device.
- **To avoid IP changes,** give each printer a fixed address (a DHCP reservation)
  in your router settings.

### Move a link to the new device

FoxTrack offers a printer in the **Link to printer…** menu only when it is not
linked yet. So unlink the old device first:

1. In FoxTrack, open **Printers** and click the printer.
2. In its **Bridge device** section, choose **Unlinked**.
3. In the **Fleet** view, find the new device under **Detected on your network,
   not linked**, and link it to the printer.

The old device stays in the list and shows as offline.

## Cameras

### In the Bridge dashboard

Click **Camera** on a printer's card to see a live picture.

**Bambu Lab:** the Bridge shows the camera of **A1, A1 mini, P1P and P1S**
printers. Other models, such as the X1 and H2 series, send their camera over a
different kind of video stream (RTSP) that the Bridge cannot read yet. Their
status and controls still work.

**Klipper:** the Bridge finds the webcam through Moonraker. If that does not
work, set **Webcam URL** on the printer (click the pencil icon).

### In FoxTrack

FoxTrack shows a camera picture in two ways:

- **Live picture.** This works only when your browser runs on the same computer
  as the Bridge.
- **Snapshot.** On other computers and phones, FoxTrack shows the most recent
  picture the Bridge sent. The Bridge sends one about every 25 seconds, and only
  while a print is running or paused. When the printer is idle, the snapshot
  does not update. For Klipper printers, the Bridge sends snapshots only when
  **Webcam URL** is set on the printer.

## Troubleshooting

### My printers do not show up in FoxTrack

- Check that the token is in the **FoxTrack sync** field, not in
  **FoxTrack (legacy)**.
- Check that the Bridge is running, and that the printer shows status in the
  Bridge dashboard.
- If the Bridge dashboard shows a warning that FoxTrack rejected the token, the
  token was revoked or mistyped, or the workspace is no longer on a plan that
  includes the Bridge. Create a new token and paste it again.

### The camera keeps loading or says "Camera unavailable"

1. **Check the printer model.** For Bambu Lab, only the A1 and P1 series work
   today (see [Cameras](#cameras)).
2. **Check the LAN access code.** It changes when you turn LAN mode off and on
   again. Copy the code from the printer screen, click the pencil icon, and paste
   the new code.
3. **Check the IP address.** It is on the printer screen under network settings.
4. **Bambu Lab cloud printers** need to be on the same network as the Bridge for
   the camera. The Bridge learns the IP from the printer, or you can type it
   with the pencil icon.

If a printer accepts the camera connection but sends no picture within 15
seconds, the Bridge stops waiting and shows "Camera unavailable". To see the
reason, click the terminal icon in the left sidebar to open the Bridge log. Look
for a line that starts with `[camera/`.

### A command from FoxTrack did nothing

If the Bridge is offline, FoxTrack queues the command. The Bridge runs it when it
reconnects, but a command that waits longer than 2 minutes expires. Start the
Bridge and send the command again.
