package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// authorBlob is a's cached response. seriesID links every work.
func (f curationFixture) authorBlob(t *testing.T, seriesID int64, extra ...int64) (AuthorResource, []byte) {
	t.Helper()
	a := AuthorResource{ForeignID: f.a, Name: "Author A"}
	sr := SeriesResource{ForeignID: seriesID, Title: "S"}
	books := map[int64][]bookResource{
		f.w[1]: {{ForeignID: f.w1Ed, Title: "One", Language: "eng"}},
		f.w[2]: {{ForeignID: f.otherEd}},
		f.w[3]: {{ForeignID: f.w3Ed}},
	}
	ids := append(append([]int64{}, f.w[1:]...), extra...)
	for _, id := range ids {
		a.Works = append(a.Works, workResource{
			ForeignID: id, Title: "orig", FullTitle: "orig", ShortTitle: "orig", BestBookID: 7, Books: books[id],
		})
		sr.LinkItems = append(sr.LinkItems, seriesWorkLinkResource{ForeignWorkID: id})
	}
	a.Series = []SeriesResource{sr}
	return a, mustJSON(t, a)
}

func decodeAuthor(t *testing.T, b []byte) (AuthorResource, map[int64]workResource) {
	t.Helper()
	var a AuthorResource
	require.NoError(t, json.Unmarshal(b, &a))
	m := map[int64]workResource{}
	for _, w := range a.Works {
		m[w.ForeignID] = w
	}
	return a, m
}

func editionIDs(w workResource) []int64 {
	out := []int64{}
	for _, b := range w.Books {
		out = append(out, b.ForeignID)
	}
	return out
}

func TestCurateUncuratedIsByteIdentical(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)

	// BestBookID 7 isn't in Books; that alone mustn't count as a change.
	_, blob := f.authorBlob(t, 1)
	got, err := f.s.CurateAuthor(ctx, f.a, blob)
	require.NoError(t, err)
	assert.Equal(t, string(blob), string(got))

	work := mustJSON(t, workResource{ForeignID: f.w[1], BestBookID: 7, Books: []bookResource{{ForeignID: f.w1Ed}}})
	got, err = f.s.CurateWork(ctx, f.w[1], work)
	require.NoError(t, err)
	assert.Equal(t, string(work), string(got))
}

func TestCurateAuthorRules(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)
	unknown := testIDs(1)[0]

	_, err := f.apply(t, fmt.Sprintf(`{
		"works": [
			{"id": %d, "status": "hidden"},
			{"id": %d, "merged_into": %d},
			{"id": %d, "title": "Better", "edition_id": %d},
			{"id": %d, "status": "canon"}
		]}`, f.w[2], f.w[3], f.w[1], f.w[1], f.w1Ed, f.unlisted))
	require.NoError(t, err)

	_, blob := f.authorBlob(t, 1, unknown)
	got, err := f.s.CurateAuthor(ctx, f.a, blob)
	require.NoError(t, err)
	a, works := decodeAuthor(t, got)

	assert.NotContains(t, works, f.w[2], "hidden")
	assert.NotContains(t, works, f.w[3], "merged away")
	assert.Contains(t, works, f.w[4], "uncurated stays")
	assert.Contains(t, works, unknown, "rules mode never removes what the store hasn't seen")
	assert.Contains(t, works, f.unlisted, "canon work added from the store")

	w1 := works[f.w[1]]
	assert.Equal(t, "Better", w1.Title)
	assert.Equal(t, "Better", w1.FullTitle)
	assert.Equal(t, f.w1Ed, w1.BestBookID)
	assert.ElementsMatch(t, []int64{f.w1Ed, f.w3Ed}, editionIDs(w1), "merged work's editions served here")

	links := []int64{}
	for _, l := range a.Series[0].LinkItems {
		links = append(links, l.ForeignWorkID)
	}
	assert.NotContains(t, links, f.w[2])
	assert.NotContains(t, links, f.w[3])
	assert.Contains(t, links, f.w[1])
}

func TestCurateAuthorAllowlist(t *testing.T) {
	t.Parallel()
	f := newCurationFixture(t)
	unknown := testIDs(1)[0]

	_, err := f.apply(t, fmt.Sprintf(`{
		"authors": [{"id": %d, "mode": "allowlist"}],
		"works": [{"id": %d, "status": "canon"}]}`, f.a, f.w[1]))
	require.NoError(t, err)

	_, blob := f.authorBlob(t, 1, unknown)
	got, err := f.s.CurateAuthor(t.Context(), f.a, blob)
	require.NoError(t, err)
	a, works := decodeAuthor(t, got)
	assert.Len(t, works, 1)
	assert.Contains(t, works, f.w[1])
	assert.Len(t, a.Series[0].LinkItems, 1)
}

func TestCurateMergedAuthorIsEmpty(t *testing.T) {
	t.Parallel()
	f := newCurationFixture(t)

	_, err := f.apply(t, fmt.Sprintf(`{"authors": [{"id": %d, "merged_into": %d}]}`, f.a, f.b))
	require.NoError(t, err)

	_, blob := f.authorBlob(t, 1)
	got, err := f.s.CurateAuthor(t.Context(), f.a, blob)
	require.NoError(t, err)
	_, works := decodeAuthor(t, got)
	assert.Empty(t, works)
}

func TestCurateWorkEditions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)
	second := f.otherEd + 5000

	blob := mustJSON(t, workResource{ForeignID: f.w[1], BestBookID: f.w1Ed, Books: []bookResource{
		{ForeignID: f.w1Ed, Language: "eng"}, {ForeignID: second, Language: "rus"},
	}})
	s := f.s
	s.Ingest(ctx, BookKey(second), mustJSON(t, workResource{ForeignID: f.w[1], Books: []bookResource{{ForeignID: second}}}))

	_, err := f.apply(t, fmt.Sprintf(`{"editions": [
		{"id": %d, "hidden": true},
		{"id": %d, "language": "eng"}]}`, f.w1Ed, second))
	require.NoError(t, err)

	got, err := s.CurateWork(ctx, f.w[1], blob)
	require.NoError(t, err)
	var w workResource
	require.NoError(t, json.Unmarshal(got, &w))
	assert.Equal(t, []int64{second}, editionIDs(w))
	assert.Equal(t, "eng", w.Books[0].Language)
	assert.Equal(t, second, w.BestBookID, "best edition was hidden")

	// Hiding every edition keeps them rather than serving none.
	_, err = f.apply(t, fmt.Sprintf(`{"editions": [{"id": %d, "hidden": true}]}`, second))
	require.NoError(t, err)
	got, err = s.CurateWork(ctx, f.w[1], blob)
	require.NoError(t, err)
	assert.Equal(t, string(blob), string(got))

	// Hidden works stay fetchable.
	_, err = f.apply(t, fmt.Sprintf(`{"works": [{"id": %d, "status": "hidden", "title": "Still here"}]}`, f.w[1]))
	require.NoError(t, err)
	got, err = s.CurateWork(ctx, f.w[1], blob)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(got, &w))
	assert.Equal(t, "Still here", w.Title)
}

type fakeCurator struct{ err error }

func (f fakeCurator) CurateAuthor(_ context.Context, _ int64, _ []byte) ([]byte, error) {
	return []byte(`{"curated":true}`), f.err
}

func (f fakeCurator) CurateWork(_ context.Context, _ int64, _ []byte) ([]byte, error) {
	return []byte(`{"curated":true}`), f.err
}

func (f fakeCurator) CurateBook(_ context.Context, _ []byte) ([]byte, error) {
	return []byte(`{"Authors":[{"ForeignId":99}]}`), f.err
}

func (f fakeCurator) CurateSearch(_ context.Context, r []SearchResource) ([]SearchResource, error) {
	return r[:0], f.err
}

func (f fakeCurator) CurateSeries(_ context.Context, b []byte) ([]byte, error) {
	return b, f.err
}

func (f fakeCurator) CurateRecommended(_ context.Context, r RecommentationsResource) (RecommentationsResource, error) {
	return r, f.err
}

func TestHandlerCurator(t *testing.T) {
	t.Parallel()
	cache := newMemoryCache()
	getter := NewMockgetter(gomock.NewController(t))
	ctrl, err := NewController(cache, getter, nil, nil)
	require.NoError(t, err)

	raw := mustJSON(t, workResource{ForeignID: 5, Title: "upstream"})
	cache.Set(t.Context(), WorkKey(5), raw, _staleTTL*100)

	get := func(h *Handler) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/work/5", nil)
		h.getWorkID(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := get(NewHandler(ctrl))
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, string(raw), body, "no curator serves upstream data")

	h := NewHandler(ctrl)
	h.SetCurator(fakeCurator{})
	_, body = get(h)
	assert.JSONEq(t, `{"curated":true}`, body)

	cached, ok := cache.Get(t.Context(), WorkKey(5))
	require.True(t, ok)
	assert.Equal(t, string(raw), string(cached), "the cache keeps upstream data")

	h.SetCurator(fakeCurator{err: errors.New("db down")})
	code, body = get(h)
	assert.Equal(t, http.StatusInternalServerError, code, "failing beats serving uncurated lists")
	assert.NotContains(t, body, "upstream")
}

func TestHandlerCuratesBookLookups(t *testing.T) {
	t.Parallel()
	cache := newMemoryCache()
	ctrl, err := NewController(cache, NewMockgetter(gomock.NewController(t)), nil, nil)
	require.NoError(t, err)
	cache.Set(t.Context(), BookKey(1), mustJSON(t, workResource{
		ForeignID: 5, Authors: []AuthorResource{{ForeignID: 7}},
	}), _staleTTL*100)

	h := NewHandler(ctrl)
	h.SetCurator(fakeCurator{})
	rec := httptest.NewRecorder()
	h.getBookID(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/book/1", nil))
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/author/99?edition=1", rec.Header().Get("Location"), "redirects to the curated author")
}

func TestCurateSearch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)
	unknown := testIDs(1)[0]
	hit := func(work, book, author int64) SearchResource {
		return SearchResource{WorkID: work, BookID: book, Author: SearchResourceAuthor{ID: author}}
	}

	// Without curation results pass through, unknown works included.
	in := []SearchResource{hit(f.w[1], f.w1Ed, f.a), hit(unknown, 3, f.a)}
	got, err := f.s.CurateSearch(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, in, got)

	shown, spare := f.otherEd+7000, f.otherEd+7001
	f.s.Ingest(ctx, WorkKey(f.w[4]), mustJSON(t, workResource{
		ForeignID: f.w[4], Title: "Work 4", BestBookID: shown,
		Books: []bookResource{{ForeignID: shown}, {ForeignID: spare}},
	}))
	_, err = f.apply(t, fmt.Sprintf(`{
		"authors": [{"id": %d, "merged_into": %d}],
		"works": [
			{"id": %d, "edition_id": %d},
			{"id": %d, "status": "hidden"},
			{"id": %d, "merged_into": %d},
			{"id": %d, "primary_author_id": %d}],
		"editions": [{"id": %d, "hidden": true}]}`,
		f.b, f.a, f.w[1], f.w1Ed, f.w[2], f.w[3], f.w[1], f.w[5], f.a, spare))
	require.NoError(t, err)

	// A merged work takes its target's place, and its rank.
	got, err = f.s.CurateSearch(ctx, []SearchResource{hit(f.w[3], f.w3Ed, f.a), hit(f.w[1], f.w1Ed, f.a)})
	require.NoError(t, err)
	assert.Equal(t, []SearchResource{hit(f.w[1], f.w1Ed, f.a)}, got)

	got, err = f.s.CurateSearch(ctx, []SearchResource{
		hit(f.w[2], f.otherEd, f.a), // Hidden.
		hit(f.w[4], spare, f.a),     // Hidden edition lands on the best one.
		hit(f.w[5], 9, f.b),         // Primary author override; b is merged too.
		hit(unknown, 3, f.b),        // Merged author resolves.
	})
	require.NoError(t, err)
	assert.Equal(t, []SearchResource{hit(f.w[4], shown, f.a), hit(f.w[5], 9, f.a), hit(unknown, 3, f.a)}, got)
}

func TestCurateBook(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)
	author := AuthorResource{ForeignID: f.a, Name: "Author A"}

	book := func(work, edition int64) []byte {
		return mustJSON(t, workResource{
			ForeignID: work, Title: "own", Authors: []AuthorResource{author},
			Books: []bookResource{{ForeignID: edition, Title: "ed"}},
		})
	}
	blob := book(f.w[3], f.w3Ed)
	got, err := f.s.CurateBook(ctx, blob)
	require.NoError(t, err)
	assert.Equal(t, string(blob), string(got), "uncurated is untouched")

	_, err = f.apply(t, fmt.Sprintf(`{"works": [
		{"id": %d, "merged_into": %d},
		{"id": %d, "title": "Better"},
		{"id": %d, "status": "hidden"}]}`, f.w[3], f.w[1], f.w[1], f.w[2]))
	require.NoError(t, err)

	got, err = f.s.CurateBook(ctx, blob)
	require.NoError(t, err)
	var w workResource
	require.NoError(t, json.Unmarshal(got, &w))
	assert.Equal(t, f.w[1], w.ForeignID, "merged work's edition resolves to the target")
	assert.Equal(t, "Better", w.Title)
	assert.Equal(t, []int64{f.w3Ed}, editionIDs(w))

	hidden := book(f.w[2], f.otherEd)
	got, err = f.s.CurateBook(ctx, hidden)
	require.NoError(t, err)
	assert.Equal(t, string(hidden), string(got), "direct lookups still find hidden works")

	// The primary author decides where /book redirects.
	two := mustJSON(t, workResource{
		ForeignID: f.w[4], Authors: []AuthorResource{author, {ForeignID: f.b, Name: "Author B"}},
		Books: []bookResource{{ForeignID: 1}},
	})
	_, err = f.apply(t, fmt.Sprintf(`{"works": [{"id": %d, "primary_author_id": %d}]}`, f.w[4], f.b))
	require.NoError(t, err)
	got, err = f.s.CurateBook(ctx, two)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(got, &w))
	assert.Equal(t, f.b, w.Authors[0].ForeignID)
}

func TestCurateSeriesAndRecommended(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)
	unknown := testIDs(1)[0]
	link := func(work int64, pos string) seriesWorkLinkResource {
		return seriesWorkLinkResource{ForeignWorkID: work, PositionInSeries: pos}
	}

	blob := mustJSON(t, SeriesResource{ForeignID: 7, Title: "S", LinkItems: []seriesWorkLinkResource{
		link(f.w[1], "1"), link(f.w[2], "2"), link(f.w[3], "3"), link(unknown, "4"),
	}})
	got, err := f.s.CurateSeries(ctx, blob)
	require.NoError(t, err)
	assert.Equal(t, string(blob), string(got), "uncurated is untouched")

	_, err = f.apply(t, fmt.Sprintf(`{"works": [
		{"id": %d, "status": "hidden"},
		{"id": %d, "merged_into": %d}]}`, f.w[2], f.w[3], f.w[4]))
	require.NoError(t, err)

	got, err = f.s.CurateSeries(ctx, blob)
	require.NoError(t, err)
	var sr SeriesResource
	require.NoError(t, json.Unmarshal(got, &sr))
	assert.Equal(t, []seriesWorkLinkResource{link(f.w[1], "1"), link(f.w[4], "3"), link(unknown, "4")}, sr.LinkItems)

	recs, err := f.s.CurateRecommended(ctx, RecommentationsResource{WorkIDs: []int64{f.w[3], f.w[2], f.w[4], unknown}})
	require.NoError(t, err)
	assert.Equal(t, []int64{f.w[4], unknown}, recs.WorkIDs, "merged takes its target's place, repeats go")
}

func TestRecommendationsReturnsTrending(t *testing.T) {
	t.Parallel()
	getter := NewMockgetter(gomock.NewController(t)) // Any GetWork call fails the test.
	ctrl, err := NewController(newMemoryCache(), getter, nil, nil)
	require.NoError(t, err)
	getter.EXPECT().Recommendations(gomock.Any(), int64(1)).Return(RecommentationsResource{WorkIDs: []int64{1, 2}}, nil)

	got, err := ctrl.Recommendations(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, []int64{1, 2}, got.WorkIDs)
}
