#!/usr/bin/env bash
# Builds this fork for the home hub, smoke-tests it on a spare port with no credentials,
# then swaps it in under a detached guard that rolls back on its own if the service fails.
#
#   hub/deploy.sh            build, smoke test, deploy
#   hub/deploy.sh --build    build and smoke test only
#
# The guard runs as its own systemd user unit, so a broken proxy cannot strand the deploy
# even when the shell that started it talks to the model through this same proxy.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
INSTALL_DIR=${INSTALL_DIR:-$HOME/cliproxyapi}
SERVICE=${SERVICE:-cliproxyapi}
GO=${GO:-$HOME/.local/go/bin/go}
KEYS_FILE=${KEYS_FILE:-$HOME/.config/cliproxyapi-keys}
STRATEGY=${STRATEGY:-quota-pacing}
SMOKE_PORT=${SMOKE_PORT:-18317}

say() { printf '\n==> %s\n' "$*"; }

cd "$REPO"
VERSION="$(git describe --tags --always --dirty)-fork"
COMMIT=$(git rev-parse --short HEAD)
BUILD_DIR="$INSTALL_DIR/fork-builds/$VERSION"
BINARY="$BUILD_DIR/cli-proxy-api"

say "Building $VERSION"
mkdir -p "$BUILD_DIR"
# No CGO: the hub has no C toolchain and runs no plugins. GOAMD64=v1 runs on any x86-64,
# including the hub's Sandy Bridge CPU, which lacks AVX2.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 "$GO" build -trimpath -buildvcs=false \
	-ldflags="-s -w -X main.Version=$VERSION -X main.Commit=$COMMIT -X main.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	-o "$BINARY" ./cmd/server

ACCESS_KEY=$(grep '^CLIPROXY_ACCESS_KEY=' "$KEYS_FILE" | cut -d= -f2- | tr -d "\"'")

say "Smoke test on port $SMOKE_PORT with an empty credential directory"
SMOKE_DIR=$(mktemp -d)
trap 'rm -rf "$SMOKE_DIR"' EXIT
python3 -I - "$INSTALL_DIR/config.yaml" "$SMOKE_DIR" "$SMOKE_PORT" "$STRATEGY" <<'EOF'
import sys, yaml
src, smoke_dir, port, strategy = sys.argv[1:]
cfg = yaml.safe_load(open(src))
cfg.setdefault("server", {})["port"] = int(port)
cfg.setdefault("oauth", {})["auth-dir"] = smoke_dir + "/auths"
cfg.setdefault("plugins", {})["enabled"] = False
cfg.setdefault("routing", {})["strategy"] = strategy
yaml.safe_dump(cfg, open(smoke_dir + "/config.yaml", "w"))
EOF
mkdir -p "$SMOKE_DIR/auths"
(cd "$SMOKE_DIR" && exec "$BINARY" -config "$SMOKE_DIR/config.yaml" >"$SMOKE_DIR/smoke.log" 2>&1) &
SMOKE_PID=$!
ok=0
for _ in $(seq 1 30); do
	sleep 1
	kill -0 "$SMOKE_PID" 2>/dev/null || break
	code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ACCESS_KEY" "http://127.0.0.1:$SMOKE_PORT/v1/models" || true)
	if [ "$code" = 200 ]; then ok=1; break; fi
done
sleep 3
kill -0 "$SMOKE_PID" 2>/dev/null || ok=0
kill "$SMOKE_PID" 2>/dev/null || true
wait "$SMOKE_PID" 2>/dev/null || true
if [ "$ok" != 1 ] || grep -q -E 'panic:|SIGILL|fatal error' "$SMOKE_DIR/smoke.log"; then
	tail -40 "$SMOKE_DIR/smoke.log"
	echo "Smoke test failed; the live service was not touched."
	exit 1
fi
echo "Smoke test passed."

if [ "${1:-}" = "--build" ]; then
	echo "Built $BINARY"
	exit 0
fi

say "Deploying under a detached guard"
systemd-run --user --unit="cpa-deploy-guard-$(date +%s)" --collect --quiet \
	--setenv=INSTALL_DIR="$INSTALL_DIR" --setenv=SERVICE="$SERVICE" --setenv=KEYS_FILE="$KEYS_FILE" \
	--setenv=STRATEGY="$STRATEGY" --setenv=NEW_BINARY="$BINARY" \
	bash "$REPO/hub/guard.sh"
echo "Guard started. Follow it with: tail -f $INSTALL_DIR/fork-builds/deploy.log"
