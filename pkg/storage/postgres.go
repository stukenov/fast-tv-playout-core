package storage

import (
    "context"
    "errors"
    "fmt"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgconn"

    "ftvx/pkg/models"
)

type Postgres struct {
    pool *pgxpool.Pool
}

func ConnectPostgres(ctx context.Context, dsn string) (*Postgres, error) {
    pool, err := pgxpool.New(ctx, dsn)
    if err != nil { return nil, err }
    p := &Postgres{pool: pool}
    if err := p.migrate(ctx); err != nil { pool.Close(); return nil, err }
    return p, nil
}

// ConnectPostgresWait retries ConnectPostgres until it succeeds or the wait time is over.
// Containers start in no particular order, so the database is often not ready yet.
func ConnectPostgresWait(ctx context.Context, dsn string, wait time.Duration) (*Postgres, error) {
    if dsn == "" { return nil, errors.New("POSTGRES_DSN is not set") }
    deadline := time.Now().Add(wait)
    for {
        p, err := ConnectPostgres(ctx, dsn)
        if err == nil { return p, nil }
        if time.Now().After(deadline) || ctx.Err() != nil { return nil, err }
        select {
        case <-ctx.Done(): return nil, ctx.Err()
        case <-time.After(time.Second):
        }
    }
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// migrations are applied in order, once each. Never edit an applied entry, append a new one.
var migrations = []string{`
CREATE TABLE IF NOT EXISTS channel (
  id           text PRIMARY KEY,
  name         text NOT NULL,
  segment_sec  int  NOT NULL DEFAULT 2,
  ladder       jsonb NOT NULL DEFAULT '[]'::jsonb,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS asset (
  id           text PRIMARY KEY,
  uri          text NOT NULL,
  duration_s   int  NOT NULL,
  tags         text[] DEFAULT '{}',
  meta         jsonb NOT NULL DEFAULT '{}',
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS timeline_entry (
  channel_id   text REFERENCES channel(id),
  start_at     timestamptz NOT NULL,
  type         text CHECK (type IN ('content','ad_break','bumper','slate')),
  asset_id     text REFERENCES asset(id),
  uri          text,
  duration_s   int NOT NULL,
  category     text,
  scte35       text,
  PRIMARY KEY (channel_id, start_at)
);
CREATE INDEX IF NOT EXISTS idx_timeline_range ON timeline_entry(channel_id, start_at);
CREATE UNIQUE INDEX IF NOT EXISTS ux_asset_uri ON asset(uri);
`, `
-- every segment lasts exactly one second now
ALTER TABLE channel ALTER COLUMN segment_sec SET DEFAULT 1;
UPDATE channel SET segment_sec = 1 WHERE segment_sec <> 1;
`, `
-- a version of the schedule (optimistic locking), where an entry starts in its source, and idempotent requests
ALTER TABLE channel ADD COLUMN IF NOT EXISTS schedule_version bigint NOT NULL DEFAULT 0;
ALTER TABLE timeline_entry ADD COLUMN IF NOT EXISTS offset_s int NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS idempotency (
  channel_id text NOT NULL REFERENCES channel(id) ON DELETE CASCADE,
  key        text NOT NULL,
  body_hash  text NOT NULL,
  version    bigint NOT NULL,
  response   jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (channel_id, key)
);
CREATE INDEX IF NOT EXISTS idx_idempotency_created ON idempotency(created_at);
`}

const migrationLockID = 727274

// migrate is safe to call from several services at once: they queue on an advisory lock.
func (p *Postgres) migrate(ctx context.Context) error {
    conn, err := p.pool.Acquire(ctx)
    if err != nil { return err }
    defer conn.Release()
    if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil { return err }
    defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockID) }()
    if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil { return err }
    for i, sql := range migrations {
        version := i + 1
        var done bool
        if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&done); err != nil { return err }
        if done { continue }
        tx, err := conn.Begin(ctx)
        if err != nil { return err }
        if _, err := tx.Exec(ctx, sql); err != nil { _ = tx.Rollback(ctx); return fmt.Errorf("migration %d: %w", version, err) }
        if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, version); err != nil { _ = tx.Rollback(ctx); return err }
        if err := tx.Commit(ctx); err != nil { return err }
    }
    return nil
}

func (p *Postgres) InsertAsset(ctx context.Context, a models.Asset) error {
    // meta is NOT NULL, and a nil map would be sent as NULL
    if a.Meta == nil { a.Meta = map[string]any{} }
    if a.Tags == nil { a.Tags = []string{} }
    _, err := p.pool.Exec(ctx, `INSERT INTO asset(id, uri, duration_s, tags, meta, created_at) VALUES($1,$2,$3,$4,$5,$6)`,
        a.ID, a.URI, a.DurationS, a.Tags, a.Meta, a.CreatedAt)
    return err
}

func (p *Postgres) GetAsset(ctx context.Context, id string) (models.Asset, error) {
    var a models.Asset
    err := p.pool.QueryRow(ctx, `SELECT id, uri, duration_s, COALESCE(tags,'{}'), COALESCE(meta,'{}'), created_at FROM asset WHERE id=$1`, id).
        Scan(&a.ID, &a.URI, &a.DurationS, &a.Tags, &a.Meta, &a.CreatedAt)
    return a, err
}

func (p *Postgres) GetAssetByURI(ctx context.Context, uri string) (models.Asset, error) {
    var a models.Asset
    err := p.pool.QueryRow(ctx, `SELECT id, uri, duration_s, COALESCE(tags,'{}'), COALESCE(meta,'{}'), created_at FROM asset WHERE uri=$1`, uri).
        Scan(&a.ID, &a.URI, &a.DurationS, &a.Tags, &a.Meta, &a.CreatedAt)
    return a, err
}

func (p *Postgres) InsertChannel(ctx context.Context, c models.Channel) error {
    if c.Ladder == nil { c.Ladder = []string{} }
    _, err := p.pool.Exec(ctx, `INSERT INTO channel(id, name, segment_sec, ladder, created_at) VALUES($1,$2,$3,$4,$5)`,
        c.ID, c.Name, c.SegmentSec, c.Ladder, c.CreatedAt)
    return err
}

func (p *Postgres) GetChannel(ctx context.Context, id string) (models.Channel, error) {
    var c models.Channel
    err := p.pool.QueryRow(ctx, `SELECT id, name, segment_sec, COALESCE(ladder,'[]'), created_at, schedule_version FROM channel WHERE id=$1`, id).
        Scan(&c.ID, &c.Name, &c.SegmentSec, &c.Ladder, &c.CreatedAt, &c.ScheduleVersion)
    return c, err
}

func (p *Postgres) ListChannels(ctx context.Context, limit int) ([]models.Channel, error) {
    rows, err := p.pool.Query(ctx, `SELECT id, name, segment_sec, COALESCE(ladder,'[]'), created_at, schedule_version FROM channel ORDER BY id LIMIT $1`, limit)
    if err != nil { return nil, err }
    defer rows.Close()
    out := []models.Channel{}
    for rows.Next() {
        var c models.Channel
        if err := rows.Scan(&c.ID, &c.Name, &c.SegmentSec, &c.Ladder, &c.CreatedAt, &c.ScheduleVersion); err != nil { return nil, err }
        out = append(out, c)
    }
    return out, rows.Err()
}

func (p *Postgres) UpsertTimeline(ctx context.Context, channelID string, entries []models.TimelineEntry) error {
    if channelID == "" { return errors.New("channel_id required") }
    batch := &pgx.Batch{}
    for _, e := range entries {
        batch.Queue(`INSERT INTO timeline_entry(channel_id,start_at,type,asset_id,uri,duration_s,category,scte35,offset_s) VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,NULLIF($7,''),NULLIF($8,''),$9)
          ON CONFLICT(channel_id,start_at) DO UPDATE SET type=EXCLUDED.type, asset_id=EXCLUDED.asset_id, uri=EXCLUDED.uri, duration_s=EXCLUDED.duration_s, category=EXCLUDED.category, scte35=EXCLUDED.scte35, offset_s=EXCLUDED.offset_s`,
            channelID, e.Start, e.Type, e.AssetID, e.URI, e.DurationS, e.Category, e.SCTE35, e.OffsetS)
    }
    br := p.pool.SendBatch(ctx, batch)
    defer br.Close()
    for range entries {
        if _, err := br.Exec(); err != nil { return err }
    }
    return nil
}

// GetTimelineRange returns entries that overlap [from,to): an entry that started before from
// but is still running (a long movie) is included. The URI falls back to the registered asset URI.
func (p *Postgres) GetTimelineRange(ctx context.Context, channelID string, from time.Time, to time.Time) ([]models.TimelineEntry, error) {
    return queryRange(ctx, p.pool, channelID, from, to)
}

// querier is what a pool and a transaction have in common.
type querier interface {
    Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func queryRange(ctx context.Context, q querier, channelID string, from, to time.Time) ([]models.TimelineEntry, error) {
    rows, err := q.Query(ctx, `SELECT t.type, COALESCE(t.asset_id,''), COALESCE(NULLIF(t.uri,''), a.uri, ''), t.start_at, t.duration_s, COALESCE(t.category,''), COALESCE(t.scte35,''), t.offset_s
FROM timeline_entry t LEFT JOIN asset a ON a.id = t.asset_id
WHERE t.channel_id=$1 AND t.start_at < $3 AND t.start_at + make_interval(secs => t.duration_s) > $2
ORDER BY t.start_at`, channelID, from, to)
    if err != nil { return nil, err }
    defer rows.Close()
    out := []models.TimelineEntry{}
    for rows.Next() {
        var e models.TimelineEntry
        if err := rows.Scan(&e.Type, &e.AssetID, &e.URI, &e.Start, &e.DurationS, &e.Category, &e.SCTE35, &e.OffsetS); err != nil { return nil, err }
        out = append(out, e)
    }
    return out, rows.Err()
}

// ReplaceTimelineRange deletes all entries that start in [from,to) for the channel and inserts the given ones in a transaction.
// It raises the schedule version of the channel.
func (p *Postgres) ReplaceTimelineRange(ctx context.Context, channelID string, from time.Time, to time.Time, entries []models.TimelineEntry) error {
    tx, err := p.pool.Begin(ctx)
    if err != nil { return err }
    defer func() { _ = tx.Rollback(ctx) }()
    if _, err := replaceInTx(ctx, tx, channelID, from, to, entries); err != nil { return err }
    return tx.Commit(ctx)
}

// replaceInTx replaces the range and raises the schedule version; it returns the new version.
func replaceInTx(ctx context.Context, tx pgx.Tx, channelID string, from, to time.Time, entries []models.TimelineEntry) (int64, error) {
    if _, err := tx.Exec(ctx, `DELETE FROM timeline_entry WHERE channel_id=$1 AND start_at >= $2 AND start_at < $3`, channelID, from, to); err != nil { return 0, err }
    batch := &pgx.Batch{}
    for _, e := range entries {
        batch.Queue(`INSERT INTO timeline_entry(channel_id,start_at,type,asset_id,uri,duration_s,category,scte35,offset_s) VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,NULLIF($7,''),NULLIF($8,''),$9)`,
            channelID, e.Start, e.Type, e.AssetID, e.URI, e.DurationS, e.Category, e.SCTE35, e.OffsetS)
    }
    br := tx.SendBatch(ctx, batch)
    for range entries {
        if _, err := br.Exec(); err != nil { br.Close(); return 0, err }
    }
    if err := br.Close(); err != nil { return 0, err }
    var version int64
    if err := tx.QueryRow(ctx, `UPDATE channel SET schedule_version = schedule_version + 1 WHERE id=$1 RETURNING schedule_version`, channelID).Scan(&version); err != nil { return 0, err }
    return version, nil
}

// HorizonCoveredUntil returns the maximal end time of entries for a channel (MAX(start_at + duration)). If none, returns zero.
func (p *Postgres) HorizonCoveredUntil(ctx context.Context, channelID string) (time.Time, error) {
    var t time.Time
    // start_at + duration_s seconds
    err := p.pool.QueryRow(ctx, `SELECT COALESCE(MAX(start_at + make_interval(secs => duration_s)), to_timestamp(0)) FROM timeline_entry WHERE channel_id=$1`, channelID).Scan(&t)
    return t, err
}

// TrimTimelinePast deletes entries older than the provided cutoff for a channel. If channelID is empty, all channels.
func (p *Postgres) TrimTimelinePast(ctx context.Context, channelID string, cutoff time.Time) (int64, error) {
    var cmd string
    var tag pgconn.CommandTag
    var err error
    if channelID == "" {
        cmd = `DELETE FROM timeline_entry WHERE start_at < $1`
        tag, err = p.pool.Exec(ctx, cmd, cutoff)
    } else {
        cmd = `DELETE FROM timeline_entry WHERE channel_id=$1 AND start_at < $2`
        tag, err = p.pool.Exec(ctx, cmd, channelID, cutoff)
    }
    if err != nil { return 0, err }
    return tag.RowsAffected(), nil
}

// SelectAssetsByTag returns assets matching a tag/library. Minimal fields only.
func (p *Postgres) SelectAssetsByTag(ctx context.Context, tag string, minDuration int) ([]models.Asset, error) {
    rows, err := p.pool.Query(ctx, `SELECT id, uri, duration_s FROM asset WHERE $1 = ANY(tags) AND duration_s >= $2 ORDER BY id`, tag, minDuration)
    if err != nil { return nil, err }
    defer rows.Close()
    out := []models.Asset{}
    for rows.Next() {
        var a models.Asset
        if err := rows.Scan(&a.ID, &a.URI, &a.DurationS); err != nil { return nil, err }
        out = append(out, a)
    }
    return out, rows.Err()
}


