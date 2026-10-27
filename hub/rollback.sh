#!/usr/bin/env bash
# Puts the stock upstream binary back and switches routing to round-robin.
set -euo pipefail

INSTALL_DIR=${INSTALL_DIR:-$HOME/cliproxyapi}
SERVICE=${SERVICE:-cliproxyapi}
STOCK="$INSTALL_DIR/fork-builds/cli-proxy-api.stock"
LIVE="$INSTALL_DIR/cli-proxy-api"
CONFIG="$INSTALL_DIR/config.yaml"

[ -x "$STOCK" ] || { echo "No stock binary at $STOCK"; exit 1; }
cp -p "$CONFIG" "$CONFIG.bak-rollback-$(date +%Y%m%d-%H%M%S)"
systemctl --user stop "$SERVICE"
cp "$STOCK" "$LIVE.new" && chmod 755 "$LIVE.new" && mv -f "$LIVE.new" "$LIVE"
sed -i -E '0,/^(\s*strategy:\s*)"[^"]*"/s//\1"round-robin"/' "$CONFIG"
systemctl --user start "$SERVICE"
sleep 5
systemctl --user is-active "$SERVICE"
"$LIVE" -h 2>&1 | grep -m1 'CLIProxyAPI Version' || true
