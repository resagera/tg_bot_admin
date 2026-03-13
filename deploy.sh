#!/usr/bin/env bash
set -euo pipefail

APP_NAME="tg-admin-bot"
APP_USER="${SUDO_USER:-$USER}"
APP_GROUP="$APP_USER"

INSTALL_DIR="/opt/${APP_NAME}"
BIN_PATH="${INSTALL_DIR}/${APP_NAME}"
CONFIG_PATH="${INSTALL_DIR}/config.json"
SERVICE_FILE="/etc/systemd/system/${APP_NAME}.service"

WORKDIR="$(cd "$(dirname "$0")" && pwd)"
BUILD_BIN="${WORKDIR}/${APP_NAME}"
TMP_BIN="${INSTALL_DIR}/${APP_NAME}.new"

UPDATE_CONFIG=0
SHOW_LOGS=0
UPDATE_REPO=0

for arg in "$@"; do
  case "$arg" in
    --config)
      UPDATE_CONFIG=1
      ;;
    --logs)
      SHOW_LOGS=1
      ;;
    --rep)
      UPDATE_REPO=1
      ;;
    *)
      echo "Unknown option: $arg"
      echo "Usage: ./deploy.sh [--config] [--logs] [--rep]"
      exit 1
      ;;
  esac
done

require_file() {
  local path="$1"
  if [ ! -f "$path" ]; then
    echo "Required file not found: $path"
    exit 1
  fi
}

echo "==> workdir: $WORKDIR"

if [ "$UPDATE_REPO" -eq 1 ]; then
  echo "==> update repository from git (hard reset)"
  if [ ! -d "$WORKDIR/.git" ]; then
    echo "Current folder is not a git repository: $WORKDIR"
    exit 1
  fi

  echo "==> fetch repository"
  git -C "$WORKDIR" fetch origin

  DEFAULT_BRANCH=$(git -C "$WORKDIR" remote show origin | grep "HEAD branch" | awk '{print $NF}')

  if [ -z "$DEFAULT_BRANCH" ]; then
    echo "Cannot detect default branch"
    exit 1
  fi

  echo "==> repository default branch: $DEFAULT_BRANCH"

  git -C "$WORKDIR" checkout -B "$DEFAULT_BRANCH"
  git -C "$WORKDIR" reset --hard "origin/$DEFAULT_BRANCH"
  git -C "$WORKDIR" clean -fd
fi

echo "==> build"
cd "$WORKDIR"
require_file "$WORKDIR/config.json"
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BUILD_BIN" .

echo "==> prepare dirs"
sudo mkdir -p "${INSTALL_DIR}/data/pages"
sudo chown -R "${APP_USER}:${APP_GROUP}" "${INSTALL_DIR}"

echo "==> install config"
if [ ! -f "$CONFIG_PATH" ]; then
  echo "==> config.json not found in install dir, copying initial config"
  sudo cp "$WORKDIR/config.json" "$CONFIG_PATH"
  sudo chown "${APP_USER}:${APP_GROUP}" "$CONFIG_PATH"
elif [ "$UPDATE_CONFIG" -eq 1 ]; then
  echo "==> updating config.json"
  sudo cp "$WORKDIR/config.json" "$CONFIG_PATH"
  sudo chown "${APP_USER}:${APP_GROUP}" "$CONFIG_PATH"
else
  echo "==> keeping existing config.json"
fi

echo "==> write new binary to temp path"
sudo cp "$BUILD_BIN" "$TMP_BIN"
sudo chown "${APP_USER}:${APP_GROUP}" "$TMP_BIN"
sudo chmod +x "$TMP_BIN"

echo "==> write systemd service"
sudo tee "$SERVICE_FILE" >/dev/null <<EOF
[Unit]
Description=Telegram Admin Bot
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${APP_USER}
Group=${APP_GROUP}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${BIN_PATH}
Restart=always
RestartSec=3

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=false
ReadWritePaths=${INSTALL_DIR}

[Install]
WantedBy=multi-user.target
EOF

echo "==> reload systemd"
sudo systemctl daemon-reload
sudo systemctl enable "${APP_NAME}" >/dev/null 2>&1 || true

echo "==> stop service if running"
if sudo systemctl is-active --quiet "${APP_NAME}"; then
  sudo systemctl stop "${APP_NAME}"
fi

echo "==> replace binary atomically"
sudo mv -f "$TMP_BIN" "$BIN_PATH"
sudo chown "${APP_USER}:${APP_GROUP}" "$BIN_PATH"
sudo chmod +x "$BIN_PATH"

echo "==> start service"
sudo systemctl restart "${APP_NAME}"

echo "==> status"
sudo systemctl --no-pager --full status "${APP_NAME}" || true

if [ "$SHOW_LOGS" -eq 1 ]; then
  echo "==> logs"
  sudo journalctl -u "${APP_NAME}" -n 100 --no-pager
fi

echo "Done."