#!/usr/bin/env bash
# package.sh — wraps the compiled binaries into proper OS-native packages
# Run AFTER build.sh: bash build.sh && bash package.sh

set -e
OUT="./dist"
APP_NAME="FoxTrack Bridge"

echo "=== Packaging for distribution ==="

# ─── macOS .app bundle (Apple Silicon) ────────────────────────────────────────
# A .app is just a folder with a specific layout that macOS recognises natively.
# Users double-click it and it runs — no Terminal needed.

make_mac_app() {
  local ARCH=$1       # arm64 or amd64
  local BINARY=$2     # e.g. foxtrack-bridge-mac-arm64
  local LABEL=$3      # e.g. Apple-Silicon or Intel

  local APP_DIR="$OUT/${APP_NAME}-${LABEL}.app"
  local MACOS_DIR="$APP_DIR/Contents/MacOS"
  local RES_DIR="$APP_DIR/Contents/Resources"

  rm -rf "$APP_DIR"
  mkdir -p "$MACOS_DIR" "$RES_DIR"

  # Copy binary inside the bundle
  cp "$OUT/$BINARY" "$MACOS_DIR/foxtrack-bridge"
  chmod +x "$MACOS_DIR/foxtrack-bridge"

  # Ad-hoc sign — free, removes the hard Gatekeeper block on modern macOS
  if command -v codesign &>/dev/null; then
    codesign --deep --force --sign "-" "$APP_DIR"
    echo "    (ad-hoc signed)"
  fi

  # Info.plist — tells macOS this is an executable app
  cat > "$APP_DIR/Contents/Info.plist" << PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key>
  <string>foxtrack-bridge</string>
  <key>CFBundleIdentifier</key>
  <string>studio.foxtrack.bridge</string>
  <key>CFBundleName</key>
  <string>FoxTrack Bridge</string>
  <key>CFBundleDisplayName</key>
  <string>FoxTrack Bridge</string>
  <key>CFBundleVersion</key>
  <string>1.1.6</string>
  <key>CFBundleShortVersionString</key>
  <string>1.1.6</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>LSUIElement</key>
  <true/>
  <key>NSHighResolutionCapable</key>
  <true/>
</dict>
</plist>
PLIST

  # Zip it so it can be downloaded without losing permissions
  cd "$OUT"
  zip -r "${APP_NAME}-macOS-${LABEL}.zip" "${APP_NAME}-${LABEL}.app" --quiet
  rm -rf "${APP_NAME}-${LABEL}.app"
  cd - > /dev/null

  echo "  macOS $LABEL → $OUT/${APP_NAME}-macOS-${LABEL}.zip"
}

make_mac_app arm64 "foxtrack-bridge-mac-arm64"  "Apple-Silicon"
make_mac_app amd64 "foxtrack-bridge-mac-intel"  "Intel"

# ─── Linux — rename with clear filename ───────────────────────────────────────
cp "$OUT/foxtrack-bridge-linux-amd64" "$OUT/${APP_NAME}-Linux-x64"
chmod +x "$OUT/${APP_NAME}-Linux-x64"
echo "  Linux x64   → $OUT/${APP_NAME}-Linux-x64"

# ─── Windows — rename with clear filename ─────────────────────────────────────
cp "$OUT/foxtrack-bridge-windows-amd64.exe" "$OUT/${APP_NAME}-Windows-x64.exe"
echo "  Windows x64 → $OUT/${APP_NAME}-Windows-x64.exe"

cp "$OUT/foxtrack-bridge-windows-arm64.exe" "$OUT/${APP_NAME}-Windows-Arm64.exe"
echo "  Windows Arm → $OUT/${APP_NAME}-Windows-Arm64.exe"

echo ""
echo "=== Done. Distributable files are in $OUT/ ==="
echo ""
echo "Tell Mac users:"
echo "  1. Download the .zip for their chip (Apple Silicon = M1/M2/M3, Intel = older Mac)"
echo "  2. Unzip it — they get 'FoxTrack Bridge.app'"
echo "  3. Double-click to open"
echo "  4. If blocked: System Settings → Privacy & Security → Open Anyway"
echo ""
echo "Tell Windows users:"
echo "  1. Download the Windows x64 or Windows Arm build"
echo "  2. Double-click to run"
echo "  3. If SmartScreen appears: click 'More info' → 'Run anyway'"
