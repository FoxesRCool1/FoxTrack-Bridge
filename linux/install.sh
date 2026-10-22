#!/bin/sh
# Single-command per-user installer for FoxTrack Bridge on Linux. No sudo,
# no system-wide writes - everything goes under $HOME. When run from a
# checkout of this repository (e.g. `./linux/install.sh` after cloning),
# the icon, .desktop entry, and systemd unit are read from the checkout -
# useful for testing unreleased changes. Otherwise (e.g. a bare
# `curl -fsSL <raw-url>/install.sh | sh`), those same three files are
# downloaded from the matching GitHub release's source tarball instead, so
# they always match the binary being installed. Only the platform binary
# and checksums.txt ever come from release assets in either mode.
set -eu

REPO="FoxesRCool1/FoxTrack-Bridge"
RELEASE_BASE_URL="https://github.com/${REPO}/releases/latest/download"
API_LATEST_URL="https://api.github.com/repos/${REPO}/releases/latest"
ARCHIVE_BASE_URL="https://github.com/${REPO}/archive/refs/tags"

BIN_DIR="$HOME/.local/bin"
BIN_PATH="$BIN_DIR/foxtrack-bridge"
ICON_DIR="$HOME/.local/share/icons/hicolor/512x512/apps"
ICON_PATH="$ICON_DIR/foxtrack-bridge.png"
DESKTOP_DIR="$HOME/.local/share/applications"
DESKTOP_PATH="$DESKTOP_DIR/foxtrack-bridge.desktop"
SERVICE_DIR="$HOME/.config/systemd/user"
SERVICE_PATH="$SERVICE_DIR/foxtrack-bridge.service"
CONFIG_DIR="$HOME/.config/FoxTrack-Bridge"

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
SRC_ICON="$REPO_ROOT/assets/logo.png"
SRC_DESKTOP="$REPO_ROOT/linux/foxtrack-bridge.desktop"
SRC_SERVICE="$REPO_ROOT/linux/foxtrack-bridge-user.service"

info() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
err()  { printf 'error: %s\n' "$*" >&2; }

have_cmd() { command -v "$1" >/dev/null 2>&1; }

# ask_yes_no <prompt>: default yes. Reads from the terminal even when the
# script itself is piped in (curl | sh). Returns 1 when nobody can answer.
ask_yes_no() {
	if [ -t 0 ]; then
		printf '%s [Y/n] ' "$1"
		read -r answer || return 1
	elif (: </dev/tty) 2>/dev/null; then
		printf '%s [Y/n] ' "$1" >/dev/tty
		read -r answer </dev/tty || return 1
	else
		return 1
	fi
	case "$answer" in
	"" | [Yy]*) return 0 ;;
	*) return 1 ;;
	esac
}

# start_service: enable and start the user unit, then show the dashboard URL.
# The unit runs on the default port; it does not see this shell's environment.
start_service() {
	port=8080
	if ! systemctl --user daemon-reload || ! systemctl --user enable --now foxtrack-bridge; then
		warn "could not start the service; run the commands below yourself"
		return 1
	fi
	info "Dashboard: http://localhost:$port"
	if { [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ]; } && have_cmd xdg-open; then
		(xdg-open "http://localhost:$port" >/dev/null 2>&1 &) || true
	fi
}

usage() {
	cat <<EOF
Usage: linux/install.sh [--yes] [--uninstall]

Installs FoxTrack Bridge for the current user only, under \$HOME/.local and
\$HOME/.config. No sudo is used or required.

  (no arguments)  Install the binary, icon, desktop entry, and systemd
                  user unit, then offer to start it now and at login.
  --yes           Start the service without asking (when systemd user
                  services are available).
  --uninstall     Remove everything the installer put in place. Your
                  configuration and print history in
                  \$HOME/.config/FoxTrack-Bridge are never touched.
  -h, --help      Show this help.
EOF
}

fetch() {
	# fetch <url> <dest>
	if have_cmd curl; then
		curl -fsSL -o "$2" "$1"
	elif have_cmd wget; then
		wget -q -O "$2" "$1"
	else
		err "need curl or wget to download files"
		exit 1
	fi
}

refresh_caches() {
	if have_cmd gtk-update-icon-cache; then
		gtk-update-icon-cache -f -t "$HOME/.local/share/icons/hicolor" >/dev/null 2>&1 || true
	fi
	if have_cmd update-desktop-database; then
		update-desktop-database "$DESKTOP_DIR" >/dev/null 2>&1 || true
	fi
}

detect_asset() {
	arch=$(uname -m)
	case "$arch" in
	x86_64)
		echo "FoxTrack-Bridge-Linux"
		;;
	aarch64)
		echo "FoxTrack-Bridge-Linux-ARM64"
		;;
	armv7l)
		echo "FoxTrack-Bridge-Linux-ARM32"
		;;
	armv6l)
		err "ARMv6 is not supported - the ARM32 release build targets ARMv7 and later"
		exit 1
		;;
	*)
		err "unrecognized architecture '$arch' - no FoxTrack Bridge build is available for it"
		exit 1
		;;
	esac
}

resolve_latest_tag() {
	release_json="$tmp_dir/release.json"
	fetch "$API_LATEST_URL" "$release_json"
	tag=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$release_json" | head -n 1)
	if [ -z "$tag" ]; then
		err "could not determine the latest release tag from the GitHub API"
		exit 1
	fi
	printf '%s\n' "$tag"
}

fetch_release_assets() {
	if ! have_cmd tar; then
		err "tar is required to unpack the release source archive"
		exit 1
	fi

	tag=$(resolve_latest_tag)
	info "No local checkout detected; using packaging assets from release $tag."

	src_tarball="$tmp_dir/source.tar.gz"
	fetch "$ARCHIVE_BASE_URL/$tag.tar.gz" "$src_tarball"

	src_dir="$tmp_dir/src"
	mkdir -p "$src_dir"
	if ! tar -xzf "$src_tarball" -C "$src_dir" --strip-components=1; then
		err "failed to extract the release $tag source archive"
		exit 1
	fi

	ICON_SRC_PATH="$src_dir/assets/logo.png"
	DESKTOP_SRC_PATH="$src_dir/linux/foxtrack-bridge.desktop"
	SERVICE_SRC_PATH="$src_dir/linux/foxtrack-bridge-user.service"

	if [ ! -f "$ICON_SRC_PATH" ] || [ ! -f "$DESKTOP_SRC_PATH" ] || [ ! -f "$SERVICE_SRC_PATH" ]; then
		err "release $tag source archive is missing expected packaging files (assets/logo.png, linux/foxtrack-bridge.desktop, or linux/foxtrack-bridge-user.service) - aborting, nothing installed"
		err "this release predates the installer; install manually per the README: https://github.com/${REPO}#manual-install-recommended"
		exit 1
	fi

	RESOLVED_TAG="$tag"
}

do_install() {
	info "FoxTrack Bridge installer"
	info "This will install to:"
	info "  binary:  $BIN_PATH"
	info "  icon:    $ICON_PATH"
	info "  desktop: $DESKTOP_PATH"
	info "  service: $SERVICE_PATH (as a systemd --user unit, not enabled)"
	info ""

	tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/foxtrack-install.XXXXXX")
	trap 'rm -rf "$tmp_dir"' EXIT INT TERM HUP

	RESOLVED_TAG=""
	if [ -f "$SRC_ICON" ] && [ -f "$SRC_DESKTOP" ] && [ -f "$SRC_SERVICE" ]; then
		info "Detected local checkout at $REPO_ROOT; using packaging assets from disk (dev/test mode)."
		ICON_SRC_PATH="$SRC_ICON"
		DESKTOP_SRC_PATH="$SRC_DESKTOP"
		SERVICE_SRC_PATH="$SRC_SERVICE"
	else
		fetch_release_assets
	fi

	asset=$(detect_asset)
	info "Detected architecture $(uname -m) -> release asset $asset"

	if [ -n "$RESOLVED_TAG" ]; then
		asset_base_url="https://github.com/${REPO}/releases/download/${RESOLVED_TAG}"
	else
		asset_base_url="$RELEASE_BASE_URL"
	fi

	info "Downloading $asset and checksums.txt..."
	fetch "$asset_base_url/$asset" "$tmp_dir/$asset"
	fetch "$asset_base_url/checksums.txt" "$tmp_dir/checksums.txt"

	info "Verifying checksum..."
	checksum_line="$tmp_dir/checksum_line.txt"
	if ! awk -v f="$asset" '$2 == f { print; found=1 } END { exit !found }' "$tmp_dir/checksums.txt" >"$checksum_line"; then
		err "no checksum entry for $asset in checksums.txt - aborting, nothing installed"
		exit 1
	fi
	if ! (cd "$tmp_dir" && sha256sum -c checksum_line.txt); then
		err "checksum verification failed for $asset - aborting, nothing installed"
		exit 1
	fi

	info "Installing binary..."
	mkdir -p "$BIN_DIR"
	cp "$tmp_dir/$asset" "$BIN_PATH.new"
	chmod 755 "$BIN_PATH.new"
	mv "$BIN_PATH.new" "$BIN_PATH"

	case ":$PATH:" in
	*":$BIN_DIR:"*) ;;
	*)
		warn "$BIN_DIR is not on your PATH. Add it in your shell profile, e.g.:"
		warn "  export PATH=\"\$HOME/.local/bin:\$PATH\""
		;;
	esac

	info "Installing icon and desktop entry..."
	mkdir -p "$ICON_DIR"
	cp "$ICON_SRC_PATH" "$ICON_PATH"
	mkdir -p "$DESKTOP_DIR"
	cp "$DESKTOP_SRC_PATH" "$DESKTOP_PATH"
	refresh_caches

	info "Installing systemd user unit..."
	mkdir -p "$SERVICE_DIR"
	cp "$SERVICE_SRC_PATH" "$SERVICE_PATH"
	info "  installed as foxtrack-bridge.service (renamed from foxtrack-bridge-user.service"
	info "  to avoid clashing with the system template unit foxtrack-bridge@.service)"

	info ""
	info "Install complete."

	if have_cmd systemctl && systemctl --user show-environment >/dev/null 2>&1 &&
		{ [ "$ASSUME_YES" = 1 ] || ask_yes_no "Start FoxTrack Bridge now and at login?"; } &&
		start_service; then
		info ""
		info "On a headless machine, keep it running after you log out with:"
		info "  loginctl enable-linger $(id -un)"
		return
	fi

	info "Dashboard: http://localhost:8080"
	info ""
	info "The service was installed but not enabled. To run it on login, run:"
	info "  systemctl --user enable --now foxtrack-bridge"
	info "  loginctl enable-linger $(id -un)"
}

do_uninstall() {
	info "Uninstalling FoxTrack Bridge..."

	if have_cmd systemctl; then
		systemctl --user stop foxtrack-bridge >/dev/null 2>&1 || true
		systemctl --user disable foxtrack-bridge >/dev/null 2>&1 || true
	fi

	rm -f "$BIN_PATH"
	rm -f "$ICON_PATH"
	rm -f "$DESKTOP_PATH"
	rm -f "$SERVICE_PATH"
	refresh_caches

	info "Removed the binary, icon, desktop entry, and systemd unit."
	info "Your configuration and print history were left untouched at:"
	info "  $CONFIG_DIR"
}

ASSUME_YES=0
ACTION=install
for arg in "$@"; do
	case "$arg" in
	--yes)
		ASSUME_YES=1
		;;
	--uninstall)
		ACTION=uninstall
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		exit 1
		;;
	esac
done

case "$ACTION" in
install)
	do_install
	;;
uninstall)
	do_uninstall
	;;
esac
