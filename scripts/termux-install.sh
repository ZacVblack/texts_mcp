#!/data/data/com.termux/files/usr/bin/bash
# Installs texts-bridge in Termux, asks for the Worker address and passphrase,
# and makes it start on its own when the phone boots (needs the Termux:Boot app).
# Phone calls also reach Claude when the Termux:API app is installed and allowed
# to read the call log; texts work either way.
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

echo "Updating Termux and installing the tools texts-bridge needs."
echo "The first time, this can take a few minutes."
# Never stop to ask about changed config files; keep the current ones.
export DEBIAN_FRONTEND=noninteractive
APT_OPTS=(-q -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold)
apt-get -q update || true
apt-get "${APT_OPTS[@]}" full-upgrade || true
apt-get "${APT_OPTS[@]}" install curl ca-certificates procps
# termux-api gives texts-bridge the termux-call-log command, for phone calls.
apt-get "${APT_OPTS[@]}" install termux-api || echo "Could not install termux-api; texts still work, calls won't."

echo "Downloading texts-bridge..."
curl -fL --retry 3 -o "$BIN.new" "https://github.com/$REPO/releases/latest/download/$ASSET"
chmod 755 "$BIN.new"
# Only replace a working copy with one that runs on this phone.
if ! "$BIN.new" version >/dev/null 2>&1; then
  rm -f "$BIN.new"
  if [ -x "$BIN" ]; then
    echo "The new texts-bridge doesn't run on this phone, so the current one stays installed."
  else
    echo "The downloaded texts-bridge doesn't run on this phone."
    exit 1
  fi
else
  mv -f "$BIN.new" "$BIN"
fi
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

# Phone calls: texts-bridge reads the call log through the Termux:API app.
# Ask now, while Termux is on screen, so Android can show its permission prompt.
echo
echo "Checking that texts-bridge can read your phone calls..."
read_calls() { timeout 20 termux-call-log -l 1 2>&1 || true; }
CALLS_OUT="no-command"
if command -v termux-call-log >/dev/null 2>&1; then
  CALLS_OUT="$(read_calls)"
  case "$CALLS_OUT" in
    *ermission*)
      echo "Android is asking whether Termux:API may read your call log. Tap Allow."
      if [ -t 0 ]; then
        read -r -p "Press Enter after you tap Allow. " _ || true
        CALLS_OUT="$(read_calls)"
      fi
      ;;
  esac
fi
case "$CALLS_OUT" in
  \[*)
    echo "Calls: working. Missed and recent calls will reach Claude too."
    ;;
  *ermission*)
    echo "Calls: not allowed yet. Open Settings > Apps > Termux:API > Permissions > Call logs > Allow."
    echo "  (If Android says it's a restricted setting, open the Termux:API app page, tap the 3 dots > Allow restricted settings, then try again.)"
    ;;
  no-command)
    echo "Calls: the termux-api package didn't install. Run this installer again."
    ;;
  "")
    echo "Calls: the Termux:API app didn't answer. Install Termux:API from the same place you got Termux"
    echo "  (F-Droid or GitHub; the two must come from the same place), open it once, then run this installer again."
    ;;
  *)
    echo "Calls: termux-call-log said: $(printf '%s' "$CALLS_OUT" | head -c 200)"
    ;;
esac
echo "(Texts work either way. Once calls work, texts-bridge picks them up by itself.)"

# Start it now too, without waiting for a reboot.
# Stop the old copy first and wait for it to exit, or the start script would
# see it still running and leave the old version in place.
pkill -f "texts-bridge run" >/dev/null 2>&1 || true
for _ in $(seq 1 20); do
  pgrep -f "texts-bridge run" >/dev/null 2>&1 || break
  sleep 0.5
done
pkill -9 -f "texts-bridge run" >/dev/null 2>&1 || true
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
