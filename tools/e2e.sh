#!/usr/bin/env bash
# End-to-end test on a machine with Docker. Takes about two minutes. Results are written to ./e2e-out:
#   contact.png      frames of the produced stream (open it and look)
#   v720.m3u8        the live playlist as the player saw it
#   report.txt       what was checked
#   logs.txt         logs of every service
set -uo pipefail
cd "$(dirname "$0")/.."
# E2E_PROFILE=tight: the playlist holds only the segment on the air and the next one
if [ "${E2E_PROFILE:-default}" = tight ]; then
  export PLAYLIST_WINDOW_MAX=2 LIVE_LEAD_S=1 TICK_MS=100 SEGMENT_RETAIN_S=10 CUE_LEAD_S=2
else
  # a command must air after the lead time of the engine, or the part of the schedule it changes is already released
  export CUE_LEAD_S=8
fi
export API_KEY=${API_KEY:-dev-key} POSTGRES_PASSWORD=${POSTGRES_PASSWORD:-dev}
COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.e2e.yml)
mkdir -p e2e-out

# One image at a time: compose builds everything in parallel, and eight Go compilers do not fit into a small runner.
for svc in control-api engine e2e assets; do
  "${COMPOSE[@]}" build --quiet "$svc" || exit 2
done
# a broken nginx config should fail here, not as a "no such host" two minutes later
docker run --rm -v "$PWD/deploy/nginx.conf:/etc/nginx/conf.d/default.conf:ro" nginx:1.27-alpine nginx -t >/dev/null 2>&1 || { echo "deploy/nginx.conf is invalid"; exit 2; }
trap '"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1' EXIT
"${COMPOSE[@]}" up -d --no-build postgres assets origin control-api engine || exit 2
cleanup() {
  "${COMPOSE[@]}" logs --no-color --timestamps > e2e-out/logs.txt 2>&1
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1
}
trap cleanup EXIT

timeout 600 "${COMPOSE[@]}" run --rm --no-deps --use-aliases e2e
status=$?
echo "e2e exit code: $status (results in e2e-out/)"
exit $status
