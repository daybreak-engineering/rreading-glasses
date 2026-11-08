package internal

import (
	"encoding/json"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const _testDSN = "postgres://postgres@localhost:5432/test"

// testIDs returns n IDs unlikely to collide with other runs on the same DB.
func testIDs(n int) []int64 {
	base := rand.Int64N(1<<40) * 1000
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = base + int64(i) + 1
	}
	return ids
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestStoreIngest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := NewStore(ctx, _testDSN)
	require.NoError(t, err)

	ids := testIDs(6)
	authorID, workID, otherWork, ed1, ed2, seriesID := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5]
	author := AuthorResource{ForeignID: authorID, Name: "Ursula"}

	// A book lookup stores the edition, unlisted.
	s.Ingest(ctx, BookKey(ed2), mustJSON(t, workResource{
		ForeignID: workID, Title: "Earthsea",
		Authors: []AuthorResource{author},
		Books:   []bookResource{{ForeignID: ed2, Title: "Terremare", Language: "ita"}},
	}))

	// The work lists ed1 and ed2.
	s.Ingest(ctx, WorkKey(workID), mustJSON(t, workResource{
		ForeignID: workID, Title: "A Wizard of Earthsea",
		Authors: []AuthorResource{author},
		Books:   []bookResource{{ForeignID: ed1, Title: "A Wizard of Earthsea", Isbn13: "9780547773742"}, {ForeignID: ed2}},
		Series: []SeriesResource{{ForeignID: seriesID, Title: "Earthsea", LinkItems: []seriesWorkLinkResource{
			{ForeignWorkID: workID, PositionInSeries: "1", SeriesPosition: 1},
		}}},
	}))

	// A later book lookup doesn't unlist ed2.
	s.Ingest(ctx, BookKey(ed2), mustJSON(t, workResource{
		ForeignID: workID, Books: []bookResource{{ForeignID: ed2}},
	}))

	var title string
	require.NoError(t, s.db.QueryRow(ctx, `SELECT title FROM works WHERE id = $1`, workID).Scan(&title))
	assert.Equal(t, "A Wizard of Earthsea", title)

	var listed int
	require.NoError(t, s.db.QueryRow(ctx, `SELECT count(*) FROM editions WHERE work_id = $1 AND listed`, workID).Scan(&listed))
	assert.Equal(t, 2, listed)

	var edWork int64
	require.NoError(t, s.db.QueryRow(ctx, `SELECT work_id FROM editions WHERE isbn13 = '9780547773742' AND id = $1`, ed1).Scan(&edWork))
	assert.Equal(t, workID, edWork)

	var pos int
	require.NoError(t, s.db.QueryRow(ctx, `SELECT position FROM work_authors WHERE work_id = $1 AND author_id = $2`, workID, authorID).Scan(&pos))
	assert.Equal(t, 0, pos)

	var seriesPos string
	require.NoError(t, s.db.QueryRow(ctx, `SELECT position FROM series_works WHERE series_id = $1 AND work_id = $2`, seriesID, workID).Scan(&seriesPos))
	assert.Equal(t, "1", seriesPos)

	// Author lists only grow.
	s.Ingest(ctx, AuthorKey(authorID), mustJSON(t, AuthorResource{
		ForeignID: authorID, Name: "Ursula K. Le Guin",
		Works: []workResource{{ForeignID: workID}, {ForeignID: otherWork}},
	}))
	s.Ingest(ctx, AuthorKey(authorID), mustJSON(t, AuthorResource{
		ForeignID: authorID, Name: "Ursula K. Le Guin",
		Works: []workResource{{ForeignID: otherWork}},
	}))

	var works int
	require.NoError(t, s.db.QueryRow(ctx, `SELECT count(*) FROM author_works WHERE author_id = $1`, authorID).Scan(&works))
	assert.Equal(t, 2, works)

	var name string
	require.NoError(t, s.db.QueryRow(ctx, `SELECT name FROM authors WHERE id = $1`, authorID).Scan(&name))
	assert.Equal(t, "Ursula K. Le Guin", name, "full author record wins over embedded copies")

	// Live author writes don't store the embedded works.
	var n int
	require.NoError(t, s.db.QueryRow(ctx, `SELECT count(*) FROM works WHERE id = $1`, otherWork).Scan(&n))
	assert.Equal(t, 0, n)

	// Sentinels and non-entity keys are ignored.
	s.Ingest(ctx, WorkKey(otherWork), _missing)
	s.Ingest(ctx, refreshAuthorKey(authorID), mustJSON(t, AuthorResource{ForeignID: otherWork}))
	require.NoError(t, s.db.QueryRow(ctx, `SELECT count(*) FROM authors WHERE id = $1`, otherWork).Scan(&n))
	assert.Equal(t, 0, n)
}

func TestStoreBackfill(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := NewStore(ctx, _testDSN)
	require.NoError(t, err)
	pg, err := newPostgresCache(ctx, _testDSN, NewMetrics())
	require.NoError(t, err)

	ids := testIDs(4)
	authorID, workID, embeddedWork, edID := ids[0], ids[1], ids[2], ids[3]

	pg.Set(ctx, WorkKey(workID), mustJSON(t, workResource{
		ForeignID: workID, Title: "fresh",
		Books: []bookResource{{ForeignID: edID}},
	}), 0) // Expired entries count too.
	pg.Set(ctx, AuthorKey(authorID), mustJSON(t, AuthorResource{
		ForeignID: authorID,
		Works: []workResource{
			{ForeignID: workID, Title: "stale copy"},
			{ForeignID: embeddedWork, Title: "only in author", Books: []bookResource{{ForeignID: edID + 1000}}},
		},
	}), time.Hour)

	n, err := s.Backfill(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(2))

	var title string
	require.NoError(t, s.db.QueryRow(ctx, `SELECT title FROM works WHERE id = $1`, workID).Scan(&title))
	assert.Equal(t, "fresh", title, "work entry wins over the author's copy")
	require.NoError(t, s.db.QueryRow(ctx, `SELECT title FROM works WHERE id = $1`, embeddedWork).Scan(&title))
	assert.Equal(t, "only in author", title)

	var listed bool
	require.NoError(t, s.db.QueryRow(ctx, `SELECT listed FROM editions WHERE id = $1`, edID+1000).Scan(&listed))
	assert.True(t, listed)

	var works int
	require.NoError(t, s.db.QueryRow(ctx, `SELECT count(*) FROM author_works WHERE author_id = $1`, authorID).Scan(&works))
	assert.Equal(t, 2, works)
}

func TestStoreCacheWritesThrough(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := NewStore(ctx, _testDSN)
	require.NoError(t, err)

	c := NewStoreCache(newMemoryCache(), s)
	workID := testIDs(1)[0]
	c.Set(ctx, WorkKey(workID), mustJSON(t, workResource{ForeignID: workID, Title: "x"}), time.Hour)
	require.NoError(t, c.Expire(ctx, WorkKey(workID)))

	var n int
	require.NoError(t, s.db.QueryRow(ctx, `SELECT count(*) FROM works WHERE id = $1`, workID).Scan(&n))
	assert.Equal(t, 1, n, "expiring the cache keeps the store row")
}

func TestStoreBackfillKeys(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := NewStore(ctx, _testDSN)
	require.NoError(t, err)
	pg, err := newPostgresCache(ctx, _testDSN, NewMetrics())
	require.NoError(t, err)

	ids := testIDs(2)
	pg.Set(ctx, WorkKey(ids[0]), mustJSON(t, workResource{ForeignID: ids[0]}), time.Hour)
	pg.Set(ctx, WorkKey(ids[1]), mustJSON(t, workResource{ForeignID: ids[1]}), time.Hour)

	n, err := s.Backfill(ctx, WorkKey(ids[0]))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	var got []int64
	rows, err := s.db.Query(ctx, `SELECT id FROM works WHERE id = ANY($1)`, ids)
	require.NoError(t, err)
	got, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	assert.Equal(t, []int64{ids[0]}, got)
}

// Full author entries sharing works, written concurrently, all land.
func TestStoreConcurrentIngest(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := NewStore(ctx, _testDSN)
	require.NoError(t, err)

	ids := testIDs(58)
	authors, works := ids[:8], ids[8:]
	var wg sync.WaitGroup
	errs := make([]error, len(authors))
	for i, aid := range authors {
		ws := make([]workResource, len(works))
		for j, perm := range rand.Perm(len(works)) {
			w := works[perm]
			ws[j] = workResource{ForeignID: w, Books: []bookResource{{ForeignID: w + 100}}}
		}
		val := mustJSON(t, AuthorResource{ForeignID: aid, Works: ws})
		wg.Go(func() { errs[i] = s.ingest(ctx, AuthorKey(aid), val, true) })
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	var n int
	require.NoError(t, s.db.QueryRow(ctx, `SELECT count(*) FROM author_works WHERE author_id = ANY($1)`, authors).Scan(&n))
	assert.Equal(t, len(authors)*len(works), n)
}
