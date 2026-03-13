#!/usr/bin/env bash
set -euo pipefail

APP_NAME="tg-admin-bot"
APP_USER="${SUDO_USER:-$USER}"
APP_GROUP="${APP_USER}"
INSTALL_DIR="/opt/${APP_NAME}"
BIN_PATH="${INSTALL_DIR}/${APP_NAME}"
SERVICE_FILE="/etc/systemd/system/${APP_NAME}.service"
WORKDIR="$(cd "$(dirname "$0")" && pwd)"

echo "==> build"
cd "$WORKDIR"
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "${APP_NAME}" .

echo "==> install dirs"
sudo mkdir -p "${INSTALL_DIR}/data/pages"
sudo cp "${APP_NAME}" "${BIN_PATH}"
sudo cp config.json "${INSTALL_DIR}/config.json"
sudo chown -R "${APP_USER}:${APP_GROUP}" "${INSTALL_DIR}"
sudo chmod +x "${BIN_PATH}"

echo "==> write systemd service"
sudo tee "${SERVICE_FILE}" >/dev/null <<EOF
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

# Безопасность
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
sudo systemctl enable "${APP_NAME}"
sudo systemctl restart "${APP_NAME}"

echo "==> status"
sudo systemctl --no-pager --full status "${APP_NAME}" || true

echo "Done."
