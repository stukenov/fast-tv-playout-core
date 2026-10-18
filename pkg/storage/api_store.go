package storage

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgconn"

    "ftvx/pkg/models"
)

var (
    ErrNotFound            = errors.New("not found")
    ErrConflict            = errors.New("already exists")
    ErrUnknownReference    = errors.New("refers to something that does not exist")
    ErrAssetInUse          = errors.New("the asset is used by the schedule")
    ErrIdempotencyConflict = errors.New("this Idempotency-Key was used with a different request")
)

// VersionMismatch is returned when If-Match does not match the version of the schedule.
type VersionMismatch struct{ Current int64 }

func (e VersionMismatch) Error() string { return fmt.Sprintf("the schedule is at version %d", e.Current) }

// mapErr turns the errors of PostgreSQL that callers care about into ours.
func mapErr(err error) error {
    if errors.Is(err, pgx.ErrNoRows) { return ErrNotFound }
    var pe *pgconn.PgError
    if errors.As(err, &pe) {
        switch pe.Code {
        case "23505": return ErrConflict
        case "23503": return ErrUnknownReference
        }
    }
    return err
}

// ListChannelsPage returns channels with an id greater than after, in id order.
func (p *Postgres) ListChannelsPage(ctx context.Context, after string, limit int) ([]models.Channel, error) {
    rows, err := p.pool.Query(ctx, `SELECT id, name, segment_sec, COALESCE(ladder,'[]'), created_at, schedule_version FROM channel WHERE id > $1 ORDER BY id LIMIT $2`, after, limit)
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

// UpdateChannel changes the name and/or the ladder of a channel.
func (p *Postgres) UpdateChannel(ctx context.Context, id string, name *string, ladder []string) (models.Channel, error) {
    var c models.Channel
    err := p.pool.QueryRow(ctx, `UPDATE channel SET name = COALESCE($2, name), ladder = COALESCE($3::jsonb, ladder) WHERE id=$1
        RETURNING id, name, segment_sec, COALESCE(ladder,'[]'), created_at, schedule_version`, id, name, ladderJSON(ladder)).
        Scan(&c.ID, &c.Name, &c.SegmentSec, &c.Ladder, &c.CreatedAt, &c.ScheduleVersion)
    return c, mapErr(err)
}

func ladderJSON(l []string) any {
    if l == nil { return nil }
    b, _ := json.Marshal(l)
    return string(b)
}

// DeleteChannel removes a channel with its schedule.
func (p *Postgres) DeleteChannel(ctx context.Context, id string) error {
    tx, err := p.pool.Begin(ctx)
    if err != nil { return err }
    defer func() { _ = tx.Rollback(ctx) }()
    if _, err := tx.Exec(ctx, `DELETE FROM timeline_entry WHERE channel_id=$1`, id); err != nil { return err }
    tag, err := tx.Exec(ctx, `DELETE FROM channel WHERE id=$1`, id)
    if err != nil { return err }
    if tag.RowsAffected() == 0 { return ErrNotFound }
    return tx.Commit(ctx)
}

// ListAssets returns assets with an id greater than after, optionally with a tag.
func (p *Postgres) ListAssets(ctx context.Context, tag, after string, limit int) ([]models.Asset, error) {
    rows, err := p.pool.Query(ctx, `SELECT id, uri, duration_s, COALESCE(tags,'{}'), COALESCE(meta,'{}'), created_at FROM asset
        WHERE id > $1 AND ($2 = '' OR $2 = ANY(tags)) ORDER BY id LIMIT $3`, after, tag, limit)
    if err != nil { return nil, err }
    defer rows.Close()
    out := []models.Asset{}
    for rows.Next() {
        var a models.Asset
        if err := rows.Scan(&a.ID, &a.URI, &a.DurationS, &a.Tags, &a.Meta, &a.CreatedAt); err != nil { return nil, err }
        out = append(out, a)
    }
    return out, rows.Err()
}

// DeleteAsset removes an asset that no schedule entry uses.
func (p *Postgres) DeleteAsset(ctx context.Context, id string) error {
    var used bool
    if err := p.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM timeline_entry WHERE asset_id=$1)`, id).Scan(&used); err != nil { return err }
    if used { return ErrAssetInUse }
    tag, err := p.pool.Exec(ctx, `DELETE FROM asset WHERE id=$1`, id)
    if err != nil { return err }
    if tag.RowsAffected() == 0 { return ErrNotFound }
    return nil
}

// ApplyRequest replaces a window of the schedule under conditions.
type ApplyRequest struct {
    ChannelID string
    From, To  time.Time
    Entries   []models.TimelineEntry
    IfMatch   *int64 // apply only if the schedule is at this version
    IdemKey   string // a repeated request with the same key and the same body is answered with the stored answer
    BodyHash  string // hash of the request body, to tell a repeat from another request that reuses the key
    // Response builds the answer to store, knowing the new version.
    Response func(version int64) []byte
}

// ApplyResult tells what happened.
type ApplyResult struct {
    Version  int64
    Replayed bool   // this was a repeat of an earlier request: nothing was changed
    Response []byte // the stored answer of the original request, when Replayed
}

// Reader lets an edit look at the schedule inside the transaction that will change it.
type Reader struct {
    tx        pgx.Tx
    ctx       context.Context
    channelID string
    Version   int64 // the schedule version when the edit began
}

// Range returns the entries that overlap [from,to).
func (r Reader) Range(from, to time.Time) ([]models.TimelineEntry, error) { return queryRange(r.ctx, r.tx, r.channelID, from, to) }

// Horizon is the end of the last entry (zero time when there are none).
func (r Reader) Horizon() (time.Time, error) {
    var t time.Time
    err := r.tx.QueryRow(r.ctx, `SELECT COALESCE(MAX(start_at + make_interval(secs => duration_s)), to_timestamp(0)) FROM timeline_entry WHERE channel_id=$1`, r.channelID).Scan(&t)
    return t, err
}

// Change is what an edit decides: replace the entries that start in [From,To) with Entries.
type Change struct {
    From, To time.Time
    Entries  []models.TimelineEntry
    Response func(version int64) []byte // the answer to keep for an idempotent repeat
}

// EditSchedule reads, decides and writes under one lock of the channel row: edits of one channel run one after the other,
// so a command never works on a schedule that another one is changing, and a version check cannot be raced.
// A repeat of an earlier request (same IdemKey and BodyHash) is answered with the stored answer and changes nothing.
func (p *Postgres) EditSchedule(ctx context.Context, channelID string, ifMatch *int64, idemKey, bodyHash string, decide func(Reader) (Change, error)) (ApplyResult, error) {
    tx, err := p.pool.Begin(ctx)
    if err != nil { return ApplyResult{}, err }
    defer func() { _ = tx.Rollback(ctx) }()
    var version int64
    if err := tx.QueryRow(ctx, `SELECT schedule_version FROM channel WHERE id=$1 FOR UPDATE`, channelID).Scan(&version); err != nil { return ApplyResult{}, mapErr(err) }
    if idemKey != "" {
        var hash string
        var v int64
        var resp []byte
        err := tx.QueryRow(ctx, `SELECT body_hash, version, response FROM idempotency WHERE channel_id=$1 AND key=$2`, channelID, idemKey).Scan(&hash, &v, &resp)
        if err == nil {
            if hash != bodyHash { return ApplyResult{}, ErrIdempotencyConflict }
            return ApplyResult{Version: v, Replayed: true, Response: resp}, nil
        }
        if !errors.Is(err, pgx.ErrNoRows) { return ApplyResult{}, err }
    }
    if ifMatch != nil && *ifMatch != version { return ApplyResult{}, VersionMismatch{Current: version} }
    ch, err := decide(Reader{tx: tx, ctx: ctx, channelID: channelID, Version: version})
    if err != nil { return ApplyResult{}, err }
    newVersion, err := replaceInTx(ctx, tx, channelID, ch.From, ch.To, ch.Entries)
    if err != nil { return ApplyResult{}, mapErr(err) }
    if idemKey != "" {
        resp := []byte("{}")
        if ch.Response != nil { resp = ch.Response(newVersion) }
        if _, err := tx.Exec(ctx, `INSERT INTO idempotency(channel_id,key,body_hash,version,response) VALUES($1,$2,$3,$4,$5)`, channelID, idemKey, bodyHash, newVersion, resp); err != nil { return ApplyResult{}, err }
        _, _ = tx.Exec(ctx, `DELETE FROM idempotency WHERE created_at < now() - interval '24 hours'`)
    }
    if err := tx.Commit(ctx); err != nil { return ApplyResult{}, err }
    return ApplyResult{Version: newVersion}, nil
}

// ApplySchedule replaces a window of the schedule under conditions.
func (p *Postgres) ApplySchedule(ctx context.Context, r ApplyRequest) (ApplyResult, error) {
    return p.EditSchedule(ctx, r.ChannelID, r.IfMatch, r.IdemKey, r.BodyHash, func(Reader) (Change, error) {
        return Change{From: r.From, To: r.To, Entries: r.Entries, Response: r.Response}, nil
    })
}

// InsertAssetChecked is InsertAsset with the errors of PostgreSQL mapped.
func (p *Postgres) InsertAssetChecked(ctx context.Context, a models.Asset) error { return mapErr(p.InsertAsset(ctx, a)) }

// InsertChannelChecked is InsertChannel with the errors of PostgreSQL mapped.
func (p *Postgres) InsertChannelChecked(ctx context.Context, c models.Channel) error { return mapErr(p.InsertChannel(ctx, c)) }

// GetAssetChecked and GetChannelChecked return ErrNotFound for a missing row.
func (p *Postgres) GetAssetChecked(ctx context.Context, id string) (models.Asset, error) {
    a, err := p.GetAsset(ctx, id)
    return a, mapErr(err)
}

func (p *Postgres) GetChannelChecked(ctx context.Context, id string) (models.Channel, error) {
    c, err := p.GetChannel(ctx, id)
    return c, mapErr(err)
}

// MissingAssets returns those of the ids that are not registered assets.
func (p *Postgres) MissingAssets(ctx context.Context, ids []string) ([]string, error) {
    if len(ids) == 0 { return nil, nil }
    rows, err := p.pool.Query(ctx, `SELECT x FROM unnest($1::text[]) AS x WHERE NOT EXISTS (SELECT 1 FROM asset WHERE id = x) ORDER BY x`, ids)
    if err != nil { return nil, err }
    defer rows.Close()
    var out []string
    for rows.Next() {
        var id string
        if err := rows.Scan(&id); err != nil { return nil, err }
        out = append(out, id)
    }
    return out, rows.Err()
}
