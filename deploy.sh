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

echo "==> build"
cd "$WORKDIR"
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BUILD_BIN" .

echo "==> prepare dirs"
sudo mkdir -p "${INSTALL_DIR}/data/pages"
sudo chown -R "${APP_USER}:${APP_GROUP}" "${INSTALL_DIR}"

if [ ! -f "$WORKDIR/config.json" ]; then
  echo "config.json not found in $WORKDIR"
  exit 1
fi

echo "==> install config if missing"
if [ ! -f "$CONFIG_PATH" ]; then
  sudo cp "$WORKDIR/config.json" "$CONFIG_PATH"
  sudo chown "${APP_USER}:${APP_GROUP}" "$CONFIG_PATH"
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

echo "Done."