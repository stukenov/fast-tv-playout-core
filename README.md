# FAST TV Playout Core

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![CI](https://github.com/stukenov/fast-tv-playout-core/actions/workflows/ci.yml/badge.svg)](https://github.com/stukenov/fast-tv-playout-core/actions/workflows/ci.yml)

The core of a live TV channel. It takes a schedule and sources that are already HLS/CMAF and produces one continuous
live HLS stream that any player (and ffmpeg) can open. Nothing is re-encoded: the engine takes the 1-second segments of your sources
as they are and rewrites only their decode times (a few bytes per segment), so that movies, ads and the slate form one timeline.
One channel needs a few megabytes of memory and no ffmpeg process.

It is a core, not a product: no guide, no scheduler, no ad server, no UI. It has a small API to control the air and to learn
what is on it, so that you can embed it in whatever you build.

```
 your system ──► control-api ──► PostgreSQL (channels, assets, schedule)
   │  REST, API keys                  │
   │                                  ▼ every tick: what plays in the next seconds?
   │  SSE, webhooks                engine ──► ad server (webhook, optional)
   └◄──────────────────────────────   │     reads the sources (files or http), writes segments + playlists
                                      ▼  files in a volume
                                   origin (nginx) ──► CDN ──► players
```

* **engine** (8085): plays one channel. Every tick it plans the next seconds from the schedule, releases the segments that are due
  (`LIVE_LEAD_S` before they air), writes `master.m3u8` and `v720.m3u8` and tells the outside world what happens (events, SCTE-35 markers).
  It never waits for the network inside its loop: ad pods and source segments are fetched ahead, every wait has a budget, and the slate
  takes the place of anything that does not answer in time.
* **control-api** (8081): channels, assets, schedule and commands (REST, `api/openapi.yaml`).
* **PostgreSQL**: the only state. **origin** (nginx): serves `/hls/` (and, if you want, your media under `/vod/`).

## Requirements for the sources

**Every segment of the stream lasts exactly one second.** A source must be:

* a **media** playlist (not a master) of **fMP4 / CMAF** with `#EXT-X-MAP`, VOD (`#EXT-X-ENDLIST`); MPEG-TS is not supported;
* segments of **exactly 1 second** (`#EXTINF:1.000000`), a keyframe at every segment start, a whole frame rate (24, 25, 30, 50, 60; 29.97 is refused), 64-bit decode times (`tfdt` version 1);
* **encoded with identical settings as every other source** (codec, profile, frame rate, resolution, encoder options) and matching `VARIANT_*` in `.env`.
  Strict decoders keep the codec parameters of the first init section. The engine counts a change in `packager_init_changes_total`.

`tools/normalize.sh` makes any video file such a source:

```bash
tools/normalize.sh film.mov media/movies/film1       # writes media/movies/film1/index.m3u8 and the segments
```

A file is rarely a whole number of seconds long. `normalize.sh` fades the picture and the sound out over the last half second and fills the rest
of that second with black and silence, so the last segment is as long as the others. This is done when the file is prepared: a fade needs an encoder.

Where the engine reads sources from:

* `file:///media/...`: files under `SOURCE_FILE_ROOT` (the compose file mounts `MEDIA_DIR` there, read-only). Links and `..` out of the folder are refused.
* `http(s)://...`: any host, or only the hosts in `SOURCE_ALLOW_HOSTS` (also checked on redirects). Other schemes are refused.

## Quick start

You need Docker with the compose plugin.

```bash
git clone https://github.com/stukenov/fast-tv-playout-core.git && cd fast-tv-playout-core
cp .env.example .env            # set API_KEY and POSTGRES_PASSWORD
mkdir -p media/movies media/ads media/slate
tools/normalize.sh film.mov media/movies/film1
tools/normalize.sh slate.mov media/slate               # any 10 s clip; airs when nothing is scheduled
tools/normalize.sh ad.mov media/ads/ad1                # a 10 s ad
cat > media/ads/_registry.json <<'JSON'
{"version":1,"creatives":[
 {"id":"ad1","type":"partner","category":["any"],"duration":10,"uri":"file:///media/ads/ad1/index.m3u8","active":true}
]}
JSON
docker compose up -d --build
```

Register a channel and a movie, then schedule it. Times are UTC, starts and durations are whole seconds.

```bash
H='X-API-Key: <your API_KEY>'
curl -s localhost:8081/v1/channels -H "$H" -d '{"id":"c1","name":"My channel","segment_sec":1}'
curl -s localhost:8081/v1/assets   -H "$H" -d '{"id":"film1","uri":"file:///media/movies/film1/index.m3u8","duration_s":5400}'

start=$(date -u -d '+2 minutes' +%Y-%m-%dT%H:%M:00Z)       # macOS: date -u -v+2M +%Y-%m-%dT%H:%M:00Z
end=$(date -u -d '+2 minutes 4 hours' +%Y-%m-%dT%H:%M:00Z)
curl -s localhost:8081/v1/channels/c1/schedule:replace -H "$H" -d '{
  "window": {"from": "'$start'", "to": "'$end'"},
  "entries": [{"type": "content", "asset_id": "film1", "start": "'$start'", "duration": 5400}]}'

open http://localhost:8080/hls/c1/master.m3u8     # or: ffplay / VLC / hls.js
```

Entry types: `content` and `bumper` (a registered `asset_id`), `slate`, `ad_break` (`duration` in seconds, `category`). Without a schedule the channel airs the slate.

## Embedding: the API

The full contract is [api/openapi.yaml](api/openapi.yaml) (a test checks that it matches the routes). In short:

| What | Call |
|---|---|
| Replace a part of the schedule | `POST /v1/channels/{id}/schedule:replace` (`?dry_run=true` only validates) |
| Read the schedule, what is on now and next | `GET /v1/channels/{id}/schedule`, `GET /v1/channels/{id}/now-next` |
| Act on the air now | `POST /v1/channels/{id}/commands` |
| Ad break from a SCTE-35 message | `POST /v1/channels/{id}/scte35` |
| Channels, assets | `/v1/channels`, `/v1/assets` (create, read, list, delete) |
| What is happening | engine `GET /v1/channels/{id}/events` (SSE), webhooks, `GET /v1/channels/{id}/status` |

**Keys and rights.** `API_KEY` is an administrator. `API_KEYS="name=secret:scopes:channels;..."` adds keys with limited rights: scopes `read`, `schedule`,
`command`, `admin` (admin includes the rest), channels joined by `+` or `*`. A secret may be given as `sha256:<hex>`. No key: 401, not allowed: 403.

**Schedule changes are safe to repeat and to race.** `Idempotency-Key` makes a repeated call return the first answer and change nothing.
`If-Match: "<version>"` (the `ETag` of the last answer) refuses a change made on top of an outdated schedule with 412. Every change is one transaction.

**Commands** (`POST .../commands`) edit the schedule so that the change reaches the stream as soon as the engine allows, and answer with `effective_at`:

```bash
curl -s localhost:8081/v1/channels/c1/commands -H "$H" -d '{"type":"ad_break","duration":30}'        # cut an ad break into what is on
curl -s localhost:8081/v1/channels/c1/commands -H "$H" -d '{"type":"insert","asset_id":"promo1"}'    # play an asset now
curl -s localhost:8081/v1/channels/c1/commands -H "$H" -d '{"type":"skip"}'                          # end the current entry or break
curl -s localhost:8081/v1/channels/c1/commands -H "$H" -d '{"type":"slate","duration":60}'           # slate for a minute
```

The entry that was cut resumes where it stopped and later entries move by the inserted time. A command airs `CUE_LEAD_S` (default 3) seconds after it was sent,
and an ad break needs at least that much plus `LIVE_LEAD_S` until it is in the playlist. With the tight profile below it is about 3 s from the call to the air.

**Events.** The engine publishes `engine.started`, `program.started`, `break.started`, `break.ended`, `slate.started`, `slate.ended`, `source.error`,
`schedule.stale` and `ads.fallback`, announced when they go on the air. Over SSE (`/v1/channels/{id}/events`, resumes with `Last-Event-ID`, `?from=now` for new events only)
and as webhooks: `WEBHOOK_URLS="https://a.example/hook|secret;https://b.example/hook"`. A webhook is a POST of the event as JSON with
`X-FTVX-Signature: sha256=<HMAC-SHA256 of the body>`, retried with backoff; a receiver that is down loses only the oldest of 512 queued events.

**SCTE-35.** Ad breaks are marked in the playlist as `EXT-X-DATERANGE` with `SCTE35-OUT`/`SCTE35-IN` (a real splice_insert with CRC) and/or `EXT-X-CUE-OUT`/`EXT-X-CUE-IN`:
`SCTE35_TAGS=daterange|cue|both|off`. The other way round, `POST .../scte35` takes a hex or base64 splice_info_section from an upstream system: "out" starts an ad break of the given length,
"in" ends the one that is on the air.

**Ads.** Breaks are filled by one of two means.

* `ADS_REGISTRY_FILE`: a JSON file of creatives (`id`, `type` partner or house, `category`, `duration`, `uri`, `active`). The engine picks creatives that fill the break exactly (house ads pad the rest) and does not repeat one back to back.
* `ADS_URL`: your ad server. The engine POSTs the break (`AdPodRequest`) a few seconds ahead and expects the pod (`AdPodResponse`) within about two seconds; see `x-ads-webhook` in the OpenAPI file.

If neither answers in time the break is filled with the slate and `ads.fallback` is published. The engine never waits for the ad server.

## Configuration

Environment variables, see `.env.example` and `docker-compose.yml`.

| Variable | Default | Meaning |
|---|---|---|
| `API_KEY` | required | administrator key of both APIs |
| `API_KEYS` | empty | keys with limited rights |
| `POSTGRES_PASSWORD` | required | password of the bundled PostgreSQL |
| `MEDIA_DIR` | `./media` | your sources, mounted at `/media` |
| `SOURCE_FILE_ROOT` | `/media` | where `file://` sources may be |
| `SOURCE_ALLOW_HOSTS` | empty (any) | hosts that `http(s)` sources may be on |
| `SLATE_URI` | `file:///media/slate/index.m3u8` | what airs when nothing is scheduled |
| `ADS_URL`, `ADS_API_KEY`, `ADS_REGISTRY_FILE` | | see Ads |
| `WEBHOOK_URLS` | empty | see Events |
| `SCTE35_TAGS` | `daterange` | markers in the playlist |
| `CUE_LEAD_S` | 3 | control-api: how far ahead a command airs |
| `LIVE_LEAD_S` | 6 | how long before its air time a segment goes into the playlist; also how late a schedule change can still be applied |
| `PUBLISH_AHEAD_S` | 30 | how far ahead the engine fetches |
| `TICK_MS` | 500 | how often the engine steps |
| `SEGMENT_RETAIN_S` | 30 | how long a segment file stays after it left the playlist |
| `PLAYLIST_WINDOW_MAX` | 180 | segments in the playlist; 2 is the minimum |
| `VARIANT_*` | 720p25 | numbers written to `master.m3u8` |

## Operations

* Metrics at `/metrics` of both services; `docker compose --profile monitoring up -d` starts Prometheus with `deploy/alerts.yml`.
* What to do when an alert fires, backups and upgrades: [docs/runbook.md](docs/runbook.md).
* Services listen on `127.0.0.1` except the origin (8080). Put TLS and a CDN in front of the origin; keep the APIs behind a reverse proxy or a VPN.
* One engine process plays one channel (`CHANNEL_ID`, `VARIANT_ID`); run more instances for more channels.

## The tight profile: two segments in the playlist

```
PLAYLIST_WINDOW_MAX=2 LIVE_LEAD_S=1 TICK_MS=100 SEGMENT_RETAIN_S=10 CUE_LEAD_S=2
```

The playlist then holds the segment on the air and the next one. A change of the schedule or a command reaches the air about two seconds after it was made:
in the end-to-end test an ad break sent through the API aired 3.0 s later and was in the playlist 2.1 s after the command. What it costs:

* **No margin.** A player had the next segment at least 0.8 s before its air time (the default profile gives 5.7 s). A player or a network that hesitates for a second stalls.
* **No rewind.** Many players start three segments from the end; with two they start at the beginning. ffmpeg followed it for 43 s without an error; try your own players.
* **Do not cache the playlist.** `deploy/nginx.conf` says `max-age=1`; use `max-age=0` or exclude `*.m3u8` in a CDN.

## What it does not do

* No transcoding at runtime, no ABR ladder (one variant per channel), no DRM, no LL-HLS, no DASH, no live input.
* VAST responses are not parsed; creatives come from the registry or from your ad server.
* `EXT-X-DISCONTINUITY` is written at every change of source. Decode times are continuous across it, so strict players still play it.
* A schedule change is applied if it is made more than `LIVE_LEAD_S` seconds before the changed second airs; what is in the playlist never changes.

## Tests

```bash
go test -race ./...                                                  # unit tests on fake time, real HTTP servers for the sources
POSTGRES_DSN=postgres://... go test -tags integration ./pkg/storage/ ./services/control-api/   # real PostgreSQL
tools/e2e.sh                                                         # the whole stack in Docker, about 4 minutes
E2E_PROFILE=tight tools/e2e.sh                                       # the same with a playlist of two segments
```

`tools/e2e.sh` generates real sources, starts the stack and plays "movie, ad break, movie, a faded tail", then cuts an ad break into a movie
with the command. It checks that every 1-second slot carries the picture of the right source segment, discontinuities are only where sources change,
the decode times follow each other, ffmpeg decodes the whole stream with exactly 25 pictures per segment, the picture of every segment has the colour of its source,
SCTE-35 markers decode to the right breaks, SSE and webhook carry the same events, and the API refuses and accepts what it should.
It writes `e2e-out/contact.png` (frames of the stream) so that you can look at the result. CI runs all of it on every pull request.

## License

MIT, see [LICENSE](LICENSE).
