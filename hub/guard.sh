#!/usr/bin/env bash
# Swaps in NEW_BINARY, sets the routing strategy, restarts the service and watches it.
# If the service does not come up healthy and stay up, restores the previous binary and
# config. Started by hub/deploy.sh as a detached systemd user unit.
set -uo pipefail

: "${INSTALL_DIR:?}" "${SERVICE:?}" "${KEYS_FILE:?}" "${STRATEGY:?}" "${NEW_BINARY:?}"
LOG="$INSTALL_DIR/fork-builds/deploy.log"
LIVE="$INSTALL_DIR/cli-proxy-api"
CONFIG="$INSTALL_DIR/config.yaml"
STAMP=$(date +%Y%m%d-%H%M%S)
PREV_BINARY="$INSTALL_DIR/fork-builds/cli-proxy-api.prev-$STAMP"
PREV_CONFIG="$INSTALL_DIR/config.yaml.bak-deploy-$STAMP"

exec >>"$LOG" 2>&1
log() { printf '%s %s\n' "$(date -Is)" "$*"; }

ACCESS_KEY=$(grep '^CLIPROXY_ACCESS_KEY=' "$KEYS_FILE" | cut -d= -f2- | tr -d "\"'")
PORT=$(python3 -I -c 'import sys,yaml; print((yaml.safe_load(open(sys.argv[1])).get("server") or {}).get("port") or 8317)' "$CONFIG")

healthy() {
	local code
	systemctl --user is-active --quiet "$SERVICE" || return 1
	code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -H "Authorization: Bearer $ACCESS_KEY" "http://127.0.0.1:$PORT/v1/models" || true)
	[ "$code" = 200 ]
}

restarts() { systemctl --user show "$SERVICE" -p NRestarts --value; }

# Comes up within 60s, then no restart and still answering for another 60s.
watch_service() {
	local up=0 before
	for _ in $(seq 1 60); do
		sleep 1
		if healthy; then up=1; break; fi
	done
	[ "$up" = 1 ] || return 1
	before=$(restarts)
	for _ in $(seq 1 12); do
		sleep 5
		[ "$(restarts)" = "$before" ] || return 1
		healthy || return 1
	done
	return 0
}

restore() {
	log "restoring $PREV_BINARY and $PREV_CONFIG"
	systemctl --user stop "$SERVICE"
	cp "$PREV_BINARY" "$LIVE.new" && chmod 755 "$LIVE.new" && mv -f "$LIVE.new" "$LIVE" || log "restore: binary copy failed"
	cp -p "$PREV_CONFIG" "$CONFIG" || log "restore: config copy failed"
	systemctl --user start "$SERVICE"
	if watch_service; then
		log "rollback ok"
	else
		log "rollback FAILED: service still unhealthy"
	fi
}

log "deploy start: $NEW_BINARY (strategy $STRATEGY)"
# Verified backups first: nothing is changed unless both copies are byte-identical.
if ! { cp -p "$LIVE" "$PREV_BINARY" && cmp -s "$LIVE" "$PREV_BINARY" &&
	cp -p "$CONFIG" "$PREV_CONFIG" && cmp -s "$CONFIG" "$PREV_CONFIG"; }; then
	log "deploy aborted: could not back up the live binary and config; nothing changed"
	exit 1
fi
if [ ! -e "$INSTALL_DIR/fork-builds/cli-proxy-api.stock" ]; then
	cp -p "$LIVE" "$INSTALL_DIR/fork-builds/cli-proxy-api.stock" || log "warning: could not keep a stock copy"
fi

# Stage both replacements before touching the live files.
NEW_CONFIG="$CONFIG.deploy-new"
if ! { cp "$NEW_BINARY" "$LIVE.new" && chmod 755 "$LIVE.new" && cmp -s "$NEW_BINARY" "$LIVE.new" &&
	cp -p "$CONFIG" "$NEW_CONFIG" &&
	sed -i -E "0,/^(\s*strategy:\s*)\"[^\"]*\"/s//\1\"$STRATEGY\"/" "$NEW_CONFIG" &&
	grep -q -E "^\s*strategy:\s*\"$STRATEGY\"" "$NEW_CONFIG"; }; then
	rm -f "$LIVE.new" "$NEW_CONFIG"
	log "deploy aborted: could not stage the new binary or config; nothing changed"
	exit 1
fi

if ! { mv -f "$LIVE.new" "$LIVE" && mv -f "$NEW_CONFIG" "$CONFIG"; }; then
	log "deploy failed while installing files"
	restore
	exit 1
fi
log "routing line now: $(grep -m1 -E '^\s*strategy:' "$CONFIG" | sed 's/#.*//')"
systemctl --user restart "$SERVICE"

if watch_service; then
	log "deploy ok: $("$LIVE" -h 2>&1 | grep -m1 'CLIProxyAPI Version' || true)"
	exit 0
fi

log "deploy FAILED; rolling back"
journalctl --user -u "$SERVICE" --since "-3 min" --no-pager | grep -v -E 'gin_logger|session-affinity' | tail -30
restore
exit 1
