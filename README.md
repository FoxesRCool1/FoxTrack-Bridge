<div align="center">
  <img src="https://raw.githubusercontent.com/FoxnTrain/FoxTrack-Bridge/main/assets/logo.png" alt="FoxTrack Bridge" width="80">

  # FoxTrack Bridge

  **Connect your local 3D printers (BambuLab, Creality, Prusa) to [FoxTrack](https://foxtrack.app) without cloud APIs.**

  [![Release](https://img.shields.io/github/v/release/FoxnTrain/FoxTrack-Bridge?style=flat-square&color=f97316)](https://github.com/FoxnTrain/FoxTrack-Bridge/releases/latest)
  [![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20macOS%20%7C%20Linux-informational?style=flat-square)](https://github.com/FoxnTrain/FoxTrack-Bridge/releases/latest)

</div>

---

> **⚠️ Compatibility & Testing Notice**
> Currently, the **BambuLab** integration has been tested on **Windows** and **Linux**. 
> Integration for **Creality** and **Prusa** printers, as well as the **macOS** version of the bridge, have been recently added and are *currently untested*. 
> 
> If you have a Creality or Prusa printer, or if you are testing the bridge on a Mac, we would love your help! Please join our [Discord server](https://discord.gg/3hd96AFYBf) to share your feedback, report bugs, and help us improve the bridge.

FoxTrack Bridge is a lightweight background app that runs on your local network and connects your 3D printers directly to FoxTrack LAN.

It sits in your system tray, should start automatically at login, and silently relays real-time printer status (print state, file name, progress, errors) to your FoxTrack dashboard.

## Download

Get the latest version for your platform:

| Platform | Download |
|----------|----------|
| Windows | [FoxTrack-Bridge-Windows.exe](https://github.com/FoxnTrain/FoxTrack-Bridge/releases/latest/download/FoxTrack-Bridge-Windows.exe) |
| macOS (Apple Silicon) | [FoxTrack-Bridge-macOS-Apple-Silicon](https://github.com/FoxnTrain/FoxTrack-Bridge/releases/latest/download/FoxTrack-Bridge-macOS-Apple-Silicon) |
| macOS (Intel) | [FoxTrack-Bridge-macOS-Intel](https://github.com/FoxnTrain/FoxTrack-Bridge/releases/latest/download/FoxTrack-Bridge-macOS-Intel) |
| Linux | [FoxTrack-Bridge-Linux](https://github.com/FoxnTrain/FoxTrack-Bridge/releases/latest/download/FoxTrack-Bridge-Linux) |

## Setup

### 1. Prepare your printer

Your printer must be connected to the same local network as the machine running the bridge. The specific requirements depend on your printer brand:

**For BambuLab:**
Your printer must be in **LAN Only Mode** to connect directly without BambuLab's cloud.
- On the printer touchscreen: **Settings → Network → LAN Only Mode**
- Scroll down and select **Developer Mode:** **Settings → Network → Developer Mode**
- You'll need your **Serial Number** (Settings → Device Info) and **LAN Access Code & IP** (Settings → LAN Mode).

**For Creality & Prusa:**
Ensure your printer has local network access enabled and note down your printer's **IP Address** and any required local API credentials or access tokens required by your specific model. 

### 2. Get your FoxTrack credentials

In FoxTrack, go to **Settings → Integrations → 3D Printer Integration** and copy your **API Key**

### 3. Run FoxTrack Bridge

**Run on Startup:** To make sure FoxTrack always is connected to the bridge, ensure the bridge is set to run on startup.

**Windows:** Double-click the `.exe`. Windows may show a SmartScreen warning — click "More info" → "Run anyway". This is expected for unsigned apps.

**macOS:** Right-click the file → Open → Open. macOS will warn about an unidentified developer on first launch — this is expected for apps not distributed through the App Store.

**Linux:** Make the file executable and run it:
```bash
chmod +x FoxTrack-Bridge-Linux
./FoxTrack-Bridge-Linux
