#!/data/data/com.termux/files/usr/bin/bash
# Installs texts-bridge in Termux, asks for the Worker address and passphrase,
# and makes it start on its own when the phone boots (needs the Termux:Boot app).
#
# Run inside Termux:
#   curl -fsSLo install.sh https://raw.githubusercontent.com/ZacVblack/texts_mcp/main/scripts/termux-install.sh && bash install.sh
set -euo pipefail

REPO="ZacVblack/texts_mcp"
BIN="$PREFIX/bin/texts-bridge"

case "$(uname -m)" in
  aarch64 | arm64) ASSET="texts-bridge-android-arm64" ;;
  armv7l | armv8l | arm) ASSET="texts-bridge-linux-armv7" ;;
  x86_64) ASSET="texts-bridge-linux-amd64" ;;
  *) echo "This phone's processor ($(uname -m)) is not supported." >&2; exit 1 ;;
esac

echo "Installing the tools texts-bridge needs..."
pkg update -y >/dev/null 2>&1 || true
pkg install -y curl ca-certificates procps >/dev/null

echo "Downloading texts-bridge..."
curl -fL --retry 3 -o "$BIN.new" "https://github.com/$REPO/releases/latest/download/$ASSET"
chmod 755 "$BIN.new"
mv -f "$BIN.new" "$BIN"
echo "Installed $("$BIN" version)."
echo

if ! "$BIN" check >/dev/null 2>&1; then
  "$BIN" setup
  echo
fi

mkdir -p "$HOME/.termux/boot" "$HOME/.texts-mcp"
cat > "$HOME/.termux/boot/start-texts-bridge" <<'BOOT'
#!/data/data/com.termux/files/usr/bin/sh
# Keeps texts-bridge running. Started by Termux:Boot when the phone boots.
termux-wake-lock
if pgrep -f "texts-bridge run" >/dev/null 2>&1; then
  exit 0
fi
LOG="$HOME/.texts-mcp/bridge.log"
nohup sh -c '
  while true; do
    if [ -f "$0" ] && [ "$(wc -c < "$0")" -gt 5000000 ]; then mv -f "$0" "$0.old"; fi
    texts-bridge run >> "$0" 2>&1
    sleep 15
  done
' "$LOG" >/dev/null 2>&1 &
BOOT
chmod 755 "$HOME/.termux/boot/start-texts-bridge"

# Also start it whenever Termux is opened, so a phone without Termux:Boot
# only needs Termux opened once after a restart.
touch "$HOME/.bashrc"
if ! grep -q "start-texts-bridge" "$HOME/.bashrc"; then
  cat >> "$HOME/.bashrc" <<'RC'

# texts-bridge: make sure it is running whenever Termux opens
[ -x "$HOME/.termux/boot/start-texts-bridge" ] && "$HOME/.termux/boot/start-texts-bridge"
RC
fi

# Start it now too, without waiting for a reboot.
pkill -f "texts-bridge run" >/dev/null 2>&1 || true
"$HOME/.termux/boot/start-texts-bridge"

echo
echo "texts-bridge is running in the background."
echo "Its log is in ~/.texts-mcp/bridge.log (view it with: tail -f ~/.texts-mcp/bridge.log)."
echo
echo "Two phone settings keep it running:"
echo "  1. Settings > Apps > Termux > Battery: Unrestricted (or 'Don't optimize')."
echo "  2. Open the Termux:Boot app once, so Android lets it start at boot."
echo "     No Termux:Boot? Then just open Termux once after the phone restarts."
echo
echo "To update later, run this installer again."
