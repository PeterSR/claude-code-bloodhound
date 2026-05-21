#!/bin/sh
# Installs bloodhound-gui into the user's XDG hierarchy: binary into
# $PREFIX/bin, the .desktop entry into $PREFIX/share/applications, and
# the two icons into $PREFIX/share/icons/hicolor. Refreshes the desktop
# / icon caches if the relevant tools are available.
#
# Default prefix is ~/.local (per-user install). Override with PREFIX
# env to drop somewhere else, e.g. PREFIX=/usr/local ./install-gui.sh
# (will need sudo for /usr/local).
#
# Runtime requirements (not installed by this script): GTK3 +
# WebKit2GTK 4.1.
#   Fedora 41+:    sudo dnf install gtk3 webkit2gtk4.1
#   Ubuntu 24.04+: sudo apt install libgtk-3-0 libwebkit2gtk-4.1-0

set -e

PREFIX="${PREFIX:-$HOME/.local}"
HERE="$(cd "$(dirname "$0")" && pwd)"

bindir="$PREFIX/bin"
appdir="$PREFIX/share/applications"
icondir_512="$PREFIX/share/icons/hicolor/512x512/apps"
icondir_svg="$PREFIX/share/icons/hicolor/scalable/apps"

mkdir -p "$bindir" "$appdir" "$icondir_512" "$icondir_svg"

install -m 755 "$HERE/bloodhound-gui" "$bindir/bloodhound-gui"
install -m 644 "$HERE/bloodhound.desktop" "$appdir/bloodhound.desktop"
install -m 644 "$HERE/bloodhound.png" "$icondir_512/bloodhound.png"
install -m 644 "$HERE/bloodhound.svg" "$icondir_svg/bloodhound.svg"

if command -v update-desktop-database >/dev/null 2>&1; then
  update-desktop-database "$appdir" 2>/dev/null || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
  gtk-update-icon-cache -f -t "$PREFIX/share/icons/hicolor" 2>/dev/null || true
fi

echo "installed:"
echo "  $bindir/bloodhound-gui"
echo "  $appdir/bloodhound.desktop"
echo
echo "Launch from your application menu, or run:  bloodhound-gui"
echo
echo "If the menu entry doesn't appear, make sure $bindir is on your PATH"
echo "and $PREFIX/share is searched by your desktop (it is by default on"
echo "GNOME and KDE with XDG_DATA_DIRS set as standard)."
