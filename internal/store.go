package internal

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

//go:embed migrations/*.sql
var _migrations embed.FS

// _migrationLock is the advisory lock ID serializing migrations.
const _migrationLock = 7240501

// Store is the permanent, normalized copy of everything we've fetched. Sync
// only inserts and updates; nothing here expires. See docs/design.md.
type Store struct {
	db *pgxpool.Pool
}

// NewStore connects to Postgres and applies pending migrations.
func NewStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := newDB(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating db: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("migrating: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate applies embedded migrations in filename order, each in its own
// transaction.
func migrate(ctx context.Context, db *pgxpool.Pool) error {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, _migrationLock); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, _migrationLock) }()

	_, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(_migrations, "migrations")
	if err != nil {
		return err
	}
	for _, e := range entries {
		var applied bool
		err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, e.Name()).Scan(&applied)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := fs.ReadFile(_migrations, "migrations/"+e.Name())
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// No arguments, so pgx uses the simple protocol, which allows
			// several statements.
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, e.Name())
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		Log(ctx).Info("applied migration", "version", e.Name())
	}
	return nil
}

// Ingest records a cache write in the store. The cache write has already
// happened, so errors are only logged.
func (s *Store) Ingest(ctx context.Context, key string, val []byte) {
	if err := s.ingest(ctx, key, val, false); err != nil {
		Log(ctx).Warn("problem writing to store", "key", key, "err", err)
	}
}

// ingest upserts the rows for one cache entry. Keys other than authors (a),
// works (w), books (b) and series (s) are ignored. full also stores the works
// embedded in an author, which the backfill needs but live writes don't (they
// arrive as their own work entries).
func (s *Store) ingest(ctx context.Context, key string, val []byte, full bool) error {
	if len(key) < 2 || len(val) == 0 || slices.Equal(val, _missing) {
		return nil
	}
	if _, err := strconv.ParseInt(key[1:], 10, 64); err != nil {
		return nil // Not an entity key, e.g. "ra123" or "/search?...".
	}

	b := &pgx.Batch{}
	switch key[0] {
	case 'a':
		var a AuthorResource
		if err := json.Unmarshal(val, &a); err != nil {
			return err
		}
		queueAuthor(b, a, full)
	case 'w', 'b':
		var w workResource
		if err := json.Unmarshal(val, &w); err != nil {
			return err
		}
		src := fromWork
		if key[0] == 'b' {
			src = fromBook
		}
		queueWork(b, w, src)
	case 's':
		var sr SeriesResource
		if err := json.Unmarshal(val, &sr); err != nil {
			return err
		}
		queueSeries(b, sr, true)
	default:
		return nil
	}
	return s.send(ctx, b)
}

// send runs a batch in one implicit transaction. Batches lock rows across
// tables in whatever order the entry lists them, so concurrent writers can
// deadlock; Postgres aborts one side, which retries after a jittered pause.
func (s *Store) send(ctx context.Context, b *pgx.Batch) error {
	var err error
	for attempt := range 6 {
		if attempt > 0 {
			d := time.Duration(25<<attempt)*time.Millisecond + rand.N(100*time.Millisecond)
			select {
			case <-ctx.Done():
				return errors.Join(err, ctx.Err())
			case <-time.After(d):
			}
		}
		err = s.db.SendBatch(ctx, b).Close()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
			return err
		}
	}
	return err
}

func queueAuthor(b *pgx.Batch, a AuthorResource, full bool) {
	queueAuthorRow(b, a, true)

	// Sorted so concurrent writers mostly lock rows in the same order.
	works := slices.SortedFunc(slices.Values(a.Works), func(x, y workResource) int {
		return cmp.Compare(x.ForeignID, y.ForeignID)
	})
	ids := make([]int64, 0, len(works))
	for _, w := range works {
		ids = append(ids, w.ForeignID)
		if full {
			queueWork(b, w, fromAuthor)
		}
	}
	ids = slices.Compact(ids)
	if len(ids) > 0 {
		b.Queue(`INSERT INTO author_works (author_id, work_id)
			SELECT $1, unnest($2::bigint[])
			ON CONFLICT DO NOTHING`, a.ForeignID, ids)
	}
	for _, sr := range a.Series {
		queueSeries(b, sr, false)
	}
}

// queueAuthorRow upserts an author's own fields. Authors embedded in works
// lack some details, so they only insert (overwrite false).
func queueAuthorRow(b *pgx.Batch, a AuthorResource, overwrite bool) {
	if a.ForeignID == 0 {
		return
	}
	a.Works, a.Series = nil, nil
	data, _ := json.Marshal(a)
	q := `INSERT INTO authors (id, name, data) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING`
	if overwrite {
		q = `INSERT INTO authors (id, name, data) VALUES ($1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, data = EXCLUDED.data, updated_at = NOW()`
	}
	b.Queue(q, a.ForeignID, a.Name, data)
}

// workSource says which kind of cache entry a work came from, which decides
// what it may overwrite.
type workSource int

const (
	// fromWork is a work entry: it owns the work row and its Books are the
	// listed editions.
	fromWork workSource = iota
	// fromBook is a book entry: it owns its one edition, which isn't
	// necessarily listed, and only inserts the work.
	fromBook
	// fromAuthor is a copy embedded in an author entry, possibly older or
	// retitled for disambiguation: insert only, editions listed.
	fromAuthor
)

// queueWork upserts a work, its credits, editions and series links.
func queueWork(b *pgx.Batch, w workResource, src workSource) {
	if w.ForeignID == 0 {
		return
	}
	overwrite := src == fromWork

	for _, e := range w.Books {
		queueEdition(b, e, w.ForeignID, src != fromAuthor, src != fromBook)
	}
	for _, a := range w.Authors {
		queueAuthorRow(b, a, false)
	}
	for _, sr := range w.Series {
		queueSeries(b, sr, false)
	}

	row := w
	row.Books = nil
	row.Authors = make([]AuthorResource, 0, len(w.Authors))
	for _, a := range w.Authors {
		a.Works, a.Series = nil, nil
		row.Authors = append(row.Authors, a)
	}
	data, _ := json.Marshal(row)

	conflict := `DO NOTHING`
	if overwrite {
		conflict = `DO UPDATE SET title = EXCLUDED.title, full_title = EXCLUDED.full_title,
			short_title = EXCLUDED.short_title, release_date = EXCLUDED.release_date,
			rating_count = EXCLUDED.rating_count, data = EXCLUDED.data, updated_at = NOW()`
	}
	b.Queue(`INSERT INTO works (id, title, full_title, short_title, release_date, rating_count, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (id) `+conflict,
		w.ForeignID, w.Title, w.FullTitle, w.ShortTitle, w.ReleaseDate, w.RatingCount, data)

	if overwrite && len(w.Authors) > 0 {
		b.Queue(`DELETE FROM work_authors WHERE work_id = $1`, w.ForeignID)
	}
	for i, a := range w.Authors {
		b.Queue(`INSERT INTO work_authors (work_id, author_id, position) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, w.ForeignID, a.ForeignID, i)
	}
}

func queueEdition(b *pgx.Batch, e bookResource, workID int64, overwrite bool, listed bool) {
	if e.ForeignID == 0 {
		return
	}
	data, _ := json.Marshal(e)
	conflict := `DO UPDATE SET listed = editions.listed OR EXCLUDED.listed`
	if overwrite {
		conflict = `DO UPDATE SET work_id = EXCLUDED.work_id, title = EXCLUDED.title,
			language = EXCLUDED.language, format = EXCLUDED.format, isbn13 = EXCLUDED.isbn13,
			asin = EXCLUDED.asin, is_ebook = EXCLUDED.is_ebook,
			listed = editions.listed OR EXCLUDED.listed, data = EXCLUDED.data, updated_at = NOW()`
	}
	b.Queue(`INSERT INTO editions (id, work_id, title, language, format, isbn13, asin, is_ebook, listed, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (id) `+conflict,
		e.ForeignID, workID, e.Title, e.Language, e.Format, e.Isbn13, e.Asin, e.IsEbook, listed, data)
}

// queueSeries upserts a series and its work links. Series embedded in works
// carry only that work's link, so they only insert the series row.
func queueSeries(b *pgx.Batch, sr SeriesResource, overwrite bool) {
	if sr.ForeignID == 0 {
		return
	}
	links := sr.LinkItems
	sr.LinkItems = nil
	data, _ := json.Marshal(sr)
	conflict := `DO NOTHING`
	if overwrite {
		conflict = `DO UPDATE SET title = EXCLUDED.title, data = EXCLUDED.data, updated_at = NOW()`
	}
	b.Queue(`INSERT INTO series (id, title, data) VALUES ($1, $2, $3) ON CONFLICT (id) `+conflict,
		sr.ForeignID, sr.Title, data)
	for _, l := range links {
		if l.ForeignWorkID == 0 {
			continue
		}
		b.Queue(`INSERT INTO series_works (series_id, work_id, position, sort, is_primary)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (series_id, work_id) DO UPDATE SET position = EXCLUDED.position,
				sort = EXCLUDED.sort, is_primary = EXCLUDED.is_primary`,
			sr.ForeignID, l.ForeignWorkID, l.PositionInSeries, l.SeriesPosition, l.Primary)
	}
}

// BackfillOnce runs Backfill unless a previous run finished.
func (s *Store) BackfillOnce(ctx context.Context) {
	var done bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM store_meta WHERE key = 'backfill')`).Scan(&done)
	if err != nil {
		Log(ctx).Error("problem checking backfill", "err", err)
		return
	}
	if done {
		return
	}
	if _, err := s.Backfill(ctx); err != nil {
		Log(ctx).Error("backfill failed", "err", err)
	}
}

// Backfill loads every author, work, book and series entry in the cache table
// into the store, expired ones included. It makes no upstream requests and is
// safe to rerun. Given keys, it loads only those and doesn't record a run.
func (s *Store) Backfill(ctx context.Context, keys ...string) (int64, error) {
	start := time.Now()
	Log(ctx).Info("starting store backfill", "keys", len(keys))

	q, args := `SELECT key, value FROM cache WHERE key ~ '^[abws][0-9]+$'`, []any{}
	if len(keys) > 0 {
		q, args = `SELECT key, value FROM cache WHERE key = ANY($1)`, []any{keys}
	}
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var n, failed atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)

	for rows.Next() {
		var key string
		var compressed []byte
		if err := rows.Scan(&key, &compressed); err != nil {
			return n.Load(), err
		}
		g.Go(func() error {
			buf := _buffers.Get()
			defer buf.Free()
			if err := decompress(gctx, bytes.NewReader(compressed), buf); err != nil {
				failed.Add(1)
				return nil
			}
			if err := s.ingest(gctx, key, buf.Bytes(), true); err != nil {
				Log(gctx).Warn("problem backfilling", "key", key, "err", err)
				failed.Add(1)
				return nil
			}
			if c := n.Add(1); c%10000 == 0 {
				Log(gctx).Info("backfill progress", "entries", c)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return n.Load(), err
	}
	if err := rows.Err(); err != nil {
		return n.Load(), err
	}

	summary := fmt.Sprintf("%d entries, %d failed, %s", n.Load(), failed.Load(), time.Since(start).Round(time.Second))
	if len(keys) > 0 {
		Log(ctx).Info("finished store backfill", "summary", summary)
		return n.Load(), nil
	}
	_, err = s.db.Exec(ctx, `INSERT INTO store_meta (key, value) VALUES ('backfill', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`, summary)
	Log(ctx).Info("finished store backfill", "summary", summary)
	return n.Load(), err
}

// StoreCache writes every cache Set through to the store.
type StoreCache struct {
	cache[[]byte]
	store *Store
}

// NewStoreCache wraps c so its writes also land in s.
func NewStoreCache(c cache[[]byte], s *Store) *StoreCache {
	return &StoreCache{cache: c, store: s}
}

// Set implements cache.
func (c *StoreCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	c.cache.Set(ctx, key, val, ttl)
	c.store.Ingest(context.WithoutCancel(ctx), key, val)
}
