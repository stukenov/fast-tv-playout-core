# Runbook

What to do when an alert fires. Services are `control-api` and the `engine` that plays the channel;
state lives in PostgreSQL (channels, assets, schedule).
The live playlist is a file written by the engine into the `hls` volume.

Quick look at everything:

```bash
docker compose ps
docker compose logs --since 10m engine
curl -s localhost:8085/v1/channels/c1/status     # engine: current sequence, clips planned, gaps, segments in the playlist, last write
curl -s localhost:8085/readyz                    # engine: 200 after the first playlist was written and while PostgreSQL answers
curl -s localhost:8080/hls/c1/v720.m3u8 | tail   # what players get
```

## Playlist not updating
Alert `PlaylistNotUpdating`, viewers see a frozen picture. One process writes the playlist: the engine.

1. `docker compose ps`: is `engine` up? `docker compose logs --since 5m engine`.
2. `curl localhost:8085/readyz`. 503 "postgres unavailable": the schedule store is down. The engine keeps airing the last schedule it read,
   so the stream goes on until that schedule ends; restore PostgreSQL.
3. `curl localhost:8085/v1/channels/c1/status`: `last_playlist_at` is when the playlist was written last; `window_len` 0 means the engine plans nothing
   (an empty schedule plans the slate, so check the slate: see "Clips dropped").
4. `last_playlist_at` is old but the engine is up: `docker compose logs engine | grep -i "write"` (a full or read-only `hls` volume), or a source that
   does not answer (the log says which; the slate takes its place).
5. Restarting `engine` is safe: it restores the window from the playlist file and continues without duplicates; a discontinuity is written at the restart point.

## Clips dropped
Alert `ClipsDropped`: a clip could not be turned into a segment, not even the slate. The stream has a hole (a few seconds).
The slate itself is unreachable or invalid: `docker compose logs engine | grep slate`. Check `SLATE_URI`, that
the slate playlist exists under `SOURCE_FILE_ROOT` and that its segments are exactly 1 second long.

## Source problems
Alert `SourceProblems`: the slate aired instead of a movie or ad. `reason` tells why:

| reason | meaning | fix |
|---|---|---|
| `fetch` | a source playlist, init section or segment cannot be downloaded or parsed | check the URI of the asset; is it a media playlist (not a master)? is it reachable from the engine container (a `file://` source must be under `SOURCE_FILE_ROOT`, an `http` one on a host of `SOURCE_ALLOW_HOSTS`)? |
| `offset` | the schedule is longer than the source | `duration_s` of the asset or the entry is larger than the real file |
| `duration` | a segment is not exactly 1 s (an incomplete last second, 29.97 fps, a wrong `-hls_time`) | re-run the file through `tools/normalize.sh`, it fades out and fills the last second |
| `timeline` | the decode times of the file are not where its segments say (the file was cut or joined, or the frame rate is 29.97) | re-run the file through `tools/normalize.sh` |
| `format` | the source has no `EXT-X-MAP` (MPEG-TS) | only fMP4/CMAF is supported, convert with `tools/normalize.sh` |
| `retime` | the segment cannot be re-timed, usually `tfdt` version 0 | convert with `tools/normalize.sh` |

## Sources encoded differently
Alert `SourcesEncodedDifferently`: the init section of a source differs from the one before it (log: "the init section differs from the previous source").
Players that re-initialize at a discontinuity (hls.js, Safari) may still play it, but ffmpeg and many TVs keep the codec parameters of the first
init section and show broken pictures after the change. Find the file (the log names it) and re-run it through `tools/normalize.sh`, like all the others,
with the same fps and size.

## Schedule store down
Alert `ScheduleStale`: the engine could not read the schedule for 30 s (event `schedule.stale`). It keeps airing the last schedule it read,
then the slate. Check PostgreSQL (`docker compose ps postgres`, `docker compose logs postgres`) and restore it; the engine recovers by itself.

## Ad breaks air as slate
`ads.fallback` events and `playout_ad_errors_total` grow. With `ADS_URL`: the ad server is down, slow (more than about 2 s) or returns a pod that does not fit the break.
With `ADS_REGISTRY_FILE`: the file is missing or invalid, or no creative fits (a house ad of 1 s lets any length be filled exactly). `docker compose logs engine | grep -i ads`.

## Webhooks not arriving
The engine retries with backoff and keeps 512 events per receiver; beyond that the oldest are dropped. Check the receiver answers 2xx and that it checks
`X-FTVX-Signature` against the same secret as in `WEBHOOK_URLS`. SSE (`/v1/channels/c1/events`) with `Last-Event-ID` gives the missed events if they are still in the last 1000.

## Backups
Everything that is not a file in `MEDIA_DIR` is in PostgreSQL:

```bash
docker compose exec -T postgres pg_dump -U ftvx ftvx | gzip > ftvx-$(date +%F).sql.gz
gunzip -c ftvx-2025-01-01.sql.gz | docker compose exec -T postgres psql -U ftvx ftvx   # restore into an empty database
```

The playlist is rebuilt by the engine from the schedule and the clock within seconds.
Keep `MEDIA_DIR` backed up separately: it is your content.

## Upgrade
```bash
git pull && docker compose build && docker compose up -d
```
Database migrations run on start under an advisory lock, so services can start together. The playlist keeps its sequence
numbers across an engine restart, players do not reload. A discontinuity is written at the restart point.
