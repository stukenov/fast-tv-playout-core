#!/usr/bin/env bash
set -euo pipefail

API_BASE=${API_BASE:-http://control-api:8081}
EPG_BASE=${EPG_BASE:-http://epg:8082}
ADS_BASE=${ADS_BASE:-http://ads:8083}
ORIGIN_BASE=${ORIGIN_BASE:-http://origin:8080}
SCHED_BASE=${SCHED_BASE:-http://scheduler:8084}
API_KEY=${API_KEY:-dev-key}

wait_for() {
  local url="$1"; local tries=${2:-30}
  for i in $(seq 1 "$tries"); do
    if curl -sSf "$url" >/dev/null; then return 0; fi
    sleep 1
  done
  echo "timeout waiting for $url" >&2; return 1
}

echo "Wait for core services"
wait_for "$API_BASE/healthz" 60
wait_for "$EPG_BASE/healthz" 60
wait_for "$ADS_BASE/healthz" 60
wait_for "$ORIGIN_BASE/hls/" 10 || true

echo "Register channel"
curl -sS -X POST "$API_BASE/v1/channels" -H 'X-API-Key: '"$API_KEY" -H 'content-type: application/json' -d '{"id":"c1","name":"FAST Movies","segment_sec":2,"ladder":["1080p","720p","480p"]}' | jq . || true

echo "Seed schedule via scheduler"
now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
in5=$(date -u -d "+5 minutes" +"%Y-%m-%dT%H:%M:%SZ")
curl -sS -X POST "$SCHED_BASE/v1/schedule/c1" -H 'X-API-Key: '"$API_KEY" -H 'content-type: application/json' \
  -d '{"from":"'"$now"'","to":"'"$in5"'","entries":[{"type":"content","asset_id":"m1","uri":"s3://bucket/content/m1/index.m3u8","start":"'"$now"'","duration":30},{"type":"ad_break","start":"'"$now"'","duration":20,"category":"midroll"}]}' | jq . || true

echo "EPG now-next"
curl -sS "$EPG_BASE/v1/epg/c1/now-next" | jq .

echo "Ads pod"
curl -sS -X POST "$ADS_BASE/v1/ads/pod" -H 'content-type: application/json' -d '{"channel_id":"c1","break":{"start":"'"$now"'","duration":20,"category":"midroll"}}' | jq .

echo "Wait for HLS to be produced..."
for i in $(seq 1 30); do
  if curl -sf "$ORIGIN_BASE/hls/c1/master.m3u8" >/dev/null; then break; fi
  sleep 1
done

echo "Fetch master.m3u8"
curl -sS -D /tmp/headers.txt "$ORIGIN_BASE/hls/c1/master.m3u8" | tee /tmp/master.m3u8

echo "Fetch stream.m3u8"
curl -sS "$ORIGIN_BASE/hls/c1/stream.m3u8" | tee /tmp/stream.m3u8

echo "Basic assertions"
grep -q "#EXTM3U" /tmp/master.m3u8
grep -q "#EXTM3U" /tmp/stream.m3u8
grep -q "#EXTINF:2.000000" /tmp/stream.m3u8 || echo "warn: expected 2s segments"
grep -q "#EXT-X-INDEPENDENT-SEGMENTS" /tmp/master.m3u8
grep -q "#EXT-X-STREAM-INF" /tmp/master.m3u8

echo "Header assertions (origin)"
grep -i "Access-Control-Allow-Origin: *" /tmp/headers.txt || echo "warn: CORS header missing"
grep -i "Cache-Control: max-age=3" /tmp/headers.txt || echo "warn: Cache-Control header missing"

echo "Check metrics thresholds"
METRICS=$(curl -sS http://packager:8086/metrics || true)
AGE=$(echo "$METRICS" | awk '/^playout_playlist_age_seconds\{channel="c1"\}/ {print $2}' | tail -n1)
if [ -n "$AGE" ]; then
  echo "playlist_age_seconds=$AGE"
  awk -v age="$AGE" 'BEGIN { if (age > 12.0) { exit 1 } }' || { echo "FAIL: playlist age too high"; exit 1; }
else
  echo "warn: metrics not available"
fi

echo "OK: smoke tests passed"


