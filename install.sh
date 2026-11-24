#!/usr/bin/env bash
# sahand-mirror installer
#   online:   bash <(curl -Ls https://raw.githubusercontent.com/sahand363/sahand-mirror/main/install.sh)
#   from hub: bash <(curl -Ls http://HUB:8686/i/sahand-mirror)
#   offline:  bash install.sh --offline sahand-mirror-bundle.tar.gz
set -euo pipefail

REPO="sahand363/sahand-mirror"
BIN=/usr/local/bin/sahand-mirror
CONF=/etc/sahand-mirror/config.json
UNIT=/etc/systemd/system/sahand-mirror.service
OFFLINE=""
PORT="${SAHAND_MIRROR_PORT:-}"
VERSION="${SAHAND_MIRROR_VERSION:-}"

red() { echo -e "\033[31m$*\033[0m"; }
green() { echo -e "\033[32m$*\033[0m"; }
die() { red "error: $*"; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --offline) OFFLINE="${2:-}"; shift 2 ;;
    --port) PORT="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    *) die "unknown option $1" ;;
  esac
done

[ "$(id -u)" = 0 ] || die "run as root"
command -v systemctl >/dev/null || die "systemd is required"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7*|armv8l|armhf) ARCH=armv7 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if [ -n "$OFFLINE" ]; then
  [ -f "$OFFLINE" ] || die "bundle not found: $OFFLINE"
  OFFLINE="$(cd "$(dirname "$OFFLINE")" && pwd)/$(basename "$OFFLINE")"
  echo "extracting hub binary from bundle (no internet needed)..."
  if tar -xzf "$OFFLINE" -C "$TMP" "self/sahand-mirror-linux-$ARCH" 2>/dev/null; then
    install -m 0755 "$TMP/self/sahand-mirror-linux-$ARCH" "$TMP/sahand-mirror"
  else
    # fall back to the mirrored release archive of sahand-mirror inside the bundle
    f="$(tar -tzf "$OFFLINE" | grep -E "^repos/sahand363/sahand-mirror/releases/[^/]+/sahand-mirror-linux-$ARCH\.tar\.gz$" | sort -V | tail -n1 || true)"
    [ -n "$f" ] || die "bundle has no sahand-mirror binary for $ARCH"
    tar -xzf "$OFFLINE" -C "$TMP" "$f"
    tar -xzf "$TMP/$f" -C "$TMP" sahand-mirror
  fi
else
  if [ -n "${MIRROR_URL:-}" ]; then
    BASE="$MIRROR_URL/gh/web/$REPO"   # installing from another hub via /i/sahand-mirror
  else
    BASE="https://github.com/$REPO"
  fi
  if [ -n "$VERSION" ]; then URL="$BASE/releases/download/$VERSION/sahand-mirror-linux-$ARCH.tar.gz"
  else URL="$BASE/releases/latest/download/sahand-mirror-linux-$ARCH.tar.gz"; fi
  echo "downloading $URL"
  curl -fL --retry 3 -o "$TMP/sm.tar.gz" "$URL" || die "download failed"
  if curl -fsL -o "$TMP/sm.tar.gz.sha256" "$URL.sha256"; then
    (cd "$TMP" && echo "$(awk '{print $1}' sm.tar.gz.sha256)  sm.tar.gz" | sha256sum -c -) || die "checksum mismatch"
  fi
  tar -xzf "$TMP/sm.tar.gz" -C "$TMP" sahand-mirror
fi

FRESH=0
[ -f "$CONF" ] || FRESH=1
if [ "$FRESH" = 1 ] && [ -z "$PORT" ]; then
  if [ -t 0 ] || [ -e /dev/tty ]; then
    read -r -p "panel port [8686]: " PORT </dev/tty || true
  fi
fi
PORT="${PORT:-8686}"
[[ "$PORT" =~ ^[0-9]+$ ]] && [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "invalid port $PORT"

systemctl stop sahand-mirror 2>/dev/null || true
install -m 0755 "$TMP/sahand-mirror" "$BIN"

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -n "$IP" ] || IP="$(ip -4 route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')"
INIT_OUT=""
if [ "$FRESH" = 1 ]; then
  INIT_OUT="$("$BIN" init --port "$PORT" --public-url "http://${IP:-127.0.0.1}:$PORT")"
elif [ -n "${SAHAND_MIRROR_PORT:-}" ]; then
  "$BIN" init --port "$PORT" >/dev/null
fi

cat >"$UNIT" <<EOF
[Unit]
Description=sahand-mirror (internal GitHub mirror hub)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$BIN serve
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now sahand-mirror >/dev/null 2>&1 || die "service failed to start (journalctl -u sahand-mirror)"

if [ -n "$OFFLINE" ]; then
  sleep 1
  echo "importing bundle..."
  "$BIN" import "$OFFLINE"
fi

echo
green "sahand-mirror $("$BIN" version) installed."
PANEL="$(grep -o '"public_url": *"[^"]*"' "$CONF" | sed 's/.*"\(http[^"]*\)"/\1/')"
echo "panel:    ${PANEL:-http://$IP:$PORT}"
if [ -n "$INIT_OUT" ]; then
  echo "user:     admin"
  echo "password: $(echo "$INIT_OUT" | awk -F': ' '/admin password/{print $2}')"
  echo "(save it now; reset any time from the menu: sahand-mirror)"
fi
echo
if [ -t 1 ] && [ -e /dev/tty ]; then
  exec "$BIN" menu </dev/tty
fi
