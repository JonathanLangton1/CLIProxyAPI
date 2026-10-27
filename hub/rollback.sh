#!/usr/bin/env bash
# Puts the stock upstream binary back and switches routing to round-robin.
# Both replacements are staged before the service stops, and the service is always started
# again, even if a step fails.
set -euo pipefail

INSTALL_DIR=${INSTALL_DIR:-$HOME/cliproxyapi}
SERVICE=${SERVICE:-cliproxyapi}
STOCK="$INSTALL_DIR/fork-builds/cli-proxy-api.stock"
LIVE="$INSTALL_DIR/cli-proxy-api"
CONFIG="$INSTALL_DIR/config.yaml"
BACKUP="$CONFIG.bak-rollback-$(date +%Y%m%d-%H%M%S)"

[ -x "$STOCK" ] || { echo "No stock binary at $STOCK"; exit 1; }
cp -p "$CONFIG" "$BACKUP" && cmp -s "$CONFIG" "$BACKUP"
cp "$STOCK" "$LIVE.new" && chmod 755 "$LIVE.new" && cmp -s "$STOCK" "$LIVE.new"
cp -p "$CONFIG" "$CONFIG.rollback-new"
sed -i -E '0,/^(\s*strategy:\s*)"[^"]*"/s//\1"round-robin"/' "$CONFIG.rollback-new"
grep -q -E '^\s*strategy:\s*"round-robin"' "$CONFIG.rollback-new"

trap 'systemctl --user start "$SERVICE"' EXIT
systemctl --user stop "$SERVICE"
mv -f "$LIVE.new" "$LIVE"
mv -f "$CONFIG.rollback-new" "$CONFIG"
trap - EXIT
systemctl --user start "$SERVICE"
sleep 5
systemctl --user is-active "$SERVICE"
"$LIVE" -h 2>&1 | grep -m1 'CLIProxyAPI Version' || true
