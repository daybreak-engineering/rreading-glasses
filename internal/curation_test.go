package internal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// curationFixture is an author with five works in its served list, plus a
// second author.
type curationFixture struct {
	s        *Store
	a, b     int64
	w        [6]int64 // w[1]..w[5]
	otherEd  int64    // An edition of w[2].
	w1Ed     int64
	w3Ed     int64
	unlisted int64 // Credited to a, not in a's served list.
}

func newCurationFixture(t *testing.T) curationFixture {
	t.Helper()
	ctx := t.Context()
	s := newTestStore(t)

	ids := testIDs(20)
	f := curationFixture{s: s, a: ids[0], b: ids[1], unlisted: ids[7], w1Ed: ids[10], otherEd: ids[11], w3Ed: ids[12]}
	copy(f.w[1:], ids[2:7])

	author := AuthorResource{ForeignID: f.a, Name: "Author A"}
	other := AuthorResource{ForeignID: f.b, Name: "Author B"}
	works := []workResource{}
	for i := 1; i <= 5; i++ {
		works = append(works, workResource{ForeignID: f.w[i]})
		var books []bookResource
		switch i {
		case 1:
			books = []bookResource{{ForeignID: f.w1Ed}}
		case 2:
			books = []bookResource{{ForeignID: f.otherEd}}
		case 3:
			books = []bookResource{{ForeignID: f.w3Ed}}
		}
		s.Ingest(ctx, WorkKey(f.w[i]), mustJSON(t, workResource{
			ForeignID: f.w[i], Title: fmt.Sprintf("Work %d", i), RatingCount: int64(10 - i),
			Authors: []AuthorResource{author, other}, Books: books,
		}))
	}
	s.Ingest(ctx, WorkKey(f.unlisted), mustJSON(t, workResource{
		ForeignID: f.unlisted, Title: "Illustrated", Authors: []AuthorResource{other, author},
	}))
	s.Ingest(ctx, AuthorKey(f.a), mustJSON(t, AuthorResource{ForeignID: f.a, Name: "Author A", Works: works}))
	s.Ingest(ctx, AuthorKey(f.b), mustJSON(t, AuthorResource{ForeignID: f.b, Name: "Author B"}))
	return f
}

func (f curationFixture) apply(t *testing.T, req string) (CurationResult, error) {
	t.Helper()
	var r CurationRequest
	require.NoError(t, json.Unmarshal([]byte(req), &r))
	if r.Source == "" {
		r.Source = "test"
	}
	return f.s.ApplyCuration(t.Context(), r)
}

func TestCurationRulesMode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)

	v, err := f.s.EffectiveWorks(ctx, f.a)
	require.NoError(t, err)
	assert.Equal(t, []int64{f.w[1], f.w[2], f.w[3], f.w[4], f.w[5]}, v.Works, "uncurated = served list")
	assert.Empty(t, v.Excluded, "works a is only credited on aren't reported")

	res, err := f.apply(t, fmt.Sprintf(`{"works": [
		{"id": %d, "status": "hidden"},
		{"id": %d, "merged_into": "%d"},
		{"id": %d, "status": "canon"},
		{"id": %d, "primary_author_id": %d}
	]}`, f.w[2], f.w[3], f.w[1], f.unlisted, f.w[5], f.b))
	require.NoError(t, err)
	assert.Equal(t, CurationResult{Works: 4}, res)

	v, err = f.s.EffectiveWorks(ctx, f.a)
	require.NoError(t, err)
	assert.Equal(t, []int64{f.w[1], f.w[4], f.unlisted}, v.Works)
	assert.Equal(t, map[int64]string{
		f.w[2]: "hidden",
		f.w[3]: fmt.Sprintf("merged into %d", f.w[1]),
		f.w[5]: fmt.Sprintf("credited to %d", f.b),
	}, v.Excluded)

	vb, err := f.s.EffectiveWorks(ctx, f.b)
	require.NoError(t, err)
	// w5 by override; canon adds "Illustrated" for every credited author.
	assert.Equal(t, []int64{f.w[5], f.unlisted}, vb.Works)
}

func TestCurationAllowlistAndPreview(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)

	_, err := f.apply(t, fmt.Sprintf(`{
		"authors": [{"id": %d, "mode": "allowlist"}],
		"works": [{"id": %d, "status": "canon"}, {"id": %d, "merged_into": %d}, {"id": %d, "status": "canon"}]
	}`, f.a, f.w[1], f.w[3], f.w[1], f.unlisted))
	require.NoError(t, err)

	v, err := f.s.EffectiveWorks(ctx, f.a)
	require.NoError(t, err)
	assert.Equal(t, "allowlist", v.Mode)
	assert.Equal(t, []int64{f.w[1], f.unlisted}, v.Works)
	assert.Equal(t, "not canon", v.Excluded[f.w[2]])

	p, err := f.s.PreviewAuthor(ctx, f.a)
	require.NoError(t, err)
	assert.Equal(t, 5, p.Served)
	assert.Equal(t, 2, p.Curated)
	assert.Equal(t, []PreviewWork{{ID: f.unlisted, Title: "Illustrated"}}, p.Added)
	removed := map[int64]string{}
	for _, w := range p.Removed {
		removed[w.ID] = w.Reason
	}
	assert.Equal(t, map[int64]string{
		f.w[2]: "not canon", f.w[3]: fmt.Sprintf("merged into %d", f.w[1]),
		f.w[4]: "not canon", f.w[5]: "not canon",
	}, removed)

	d, err := f.s.AuthorWorks(ctx, f.a, AuthorWorksFilter{Status: "served"})
	require.NoError(t, err)
	assert.Equal(t, 2, d.Total)
}

func TestCurationAuthorMerge(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)

	_, err := f.apply(t, fmt.Sprintf(`{"authors": [{"id": %d, "merged_into": %d}]}`, f.b, f.a))
	require.NoError(t, err)

	vb, err := f.s.EffectiveWorks(ctx, f.b)
	require.NoError(t, err)
	assert.Empty(t, vb.Works)

	_, err = f.apply(t, fmt.Sprintf(`{"works": [{"id": %d, "primary_author_id": %d}]}`, f.unlisted, f.b))
	require.NoError(t, err)
	v, err := f.s.EffectiveWorks(ctx, f.a)
	require.NoError(t, err)
	assert.Contains(t, v.Works, f.unlisted, "credited to an author merged into a")
}

func TestCurationValidation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)

	_, err := f.apply(t, fmt.Sprintf(`{"works": [{"id": %d, "merged_into": %d}]}`, f.w[3], f.w[1]))
	require.NoError(t, err)

	tests := map[string]string{
		"chain":           fmt.Sprintf(`{"works": [{"id": %d, "status": "canon"}, {"id": %d, "merged_into": %d}]}`, f.w[4], f.w[1], f.w[2]),
		"self merge":      fmt.Sprintf(`{"works": [{"id": %d, "merged_into": %d}]}`, f.w[4], f.w[4]),
		"foreign edition": fmt.Sprintf(`{"works": [{"id": %d, "edition_id": %d}]}`, f.w[1], f.otherEd),
		"unknown field":   fmt.Sprintf(`{"works": [{"id": %d, "colour": "red"}]}`, f.w[1]),
		"unknown work":    `{"works": [{"id": 1, "status": "canon"}]}`,
		"bad status":      fmt.Sprintf(`{"works": [{"id": %d, "status": "maybe"}]}`, f.w[1]),
		"missing id":      `{"editions": [{"hidden": true}]}`,
		"bad mode":        fmt.Sprintf(`{"authors": [{"id": %d, "mode": "all"}]}`, f.a),
	}
	for name, req := range tests {
		_, err := f.apply(t, req)
		assert.ErrorIs(t, err, errBadRequest, name)
	}

	// The chain request's first patch was rolled back with it.
	d, err := f.s.Work(ctx, f.w[4])
	require.NoError(t, err)
	assert.Nil(t, d.Curation)

	// An edition of a work merged into this one is fine.
	_, err = f.apply(t, fmt.Sprintf(`{"works": [{"id": %d, "edition_id": %d}]}`, f.w[1], f.w3Ed))
	require.NoError(t, err)
	d, err = f.s.Work(ctx, f.w[1])
	require.NoError(t, err)
	assert.Equal(t, []int64{f.w[3]}, d.MergedFrom)
	assert.Len(t, d.Editions, 2)
}

func TestCurationLogAndNoops(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newCurationFixture(t)

	req := fmt.Sprintf(`{"works": [{"id": %d, "title": "Better", "note": "typo"}]}`, f.w[1])
	res, err := f.apply(t, req)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Works)
	res, err = f.apply(t, req)
	require.NoError(t, err)
	assert.Equal(t, CurationResult{Unchanged: 1}, res)

	_, err = f.apply(t, fmt.Sprintf(`{"works": [{"id": %d, "title": null}]}`, f.w[1]))
	require.NoError(t, err)

	log, err := f.s.CurationLog(ctx, "work", f.w[1], 10)
	require.NoError(t, err)
	require.Len(t, log, 2)
	assert.Nil(t, log[1].Before, "first change has no before")
	assert.Equal(t, "Better", log[1].After.(map[string]any)["title"])
	assert.Equal(t, "Better", log[0].Before.(map[string]any)["title"])
	assert.Nil(t, log[0].After.(map[string]any)["title"])
	assert.Equal(t, "typo", log[0].After.(map[string]any)["note"], "absent fields are kept")
}

func TestAdminAPI(t *testing.T) {
	t.Parallel()
	f := newCurationFixture(t)
	srv := httptest.NewServer(NewAdminMux(f.s, "secret"))
	t.Cleanup(srv.Close)

	do := func(method, path, token, body string) (int, map[string]any) {
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var out any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		m, _ := out.(map[string]any)
		if m == nil {
			m = map[string]any{"list": out}
		}
		return resp.StatusCode, m
	}

	code, _ := do("GET", "/admin/authors?q=x", "", "")
	assert.Equal(t, http.StatusUnauthorized, code)
	code, _ = do("GET", "/admin/authors?q=x", "wrong", "")
	assert.Equal(t, http.StatusUnauthorized, code)

	code, body := do("GET", "/admin/authors/1", "secret", "")
	assert.Equal(t, http.StatusNotFound, code, body)
	code, body = do("GET", "/admin/authors/abc", "secret", "")
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = do("GET", "/admin/editions", "secret", "")
	assert.Equal(t, http.StatusBadRequest, code, body)

	code, body = do("POST", "/admin/curation", "secret",
		fmt.Sprintf(`{"source": "test", "works": [{"id": %d, "status": "hidden"}]}`, f.w[2]))
	require.Equal(t, http.StatusOK, code, body)
	assert.InDelta(t, 1, body["works"], 0)

	code, body = do("POST", "/admin/curation", "secret", `{"works": []}`)
	assert.Equal(t, http.StatusBadRequest, code, "source required: %v", body)
	code, body = do("POST", "/admin/curation", "secret", `{"source": "x", "work": []}`)
	assert.Equal(t, http.StatusBadRequest, code, "typo'd key rejected: %v", body)

	code, body = do("GET", fmt.Sprintf("/admin/authors/%d?status=excluded", f.a), "secret", "")
	require.Equal(t, http.StatusOK, code, body)
	assert.InDelta(t, 1, body["total"], 0)

	code, body = do("GET", fmt.Sprintf("/admin/authors/%d/preview", f.a), "secret", "")
	require.Equal(t, http.StatusOK, code, body)
	assert.InDelta(t, 4, body["curated"], 0)

	code, body = do("GET", fmt.Sprintf("/admin/log?kind=work&id=%d", f.w[2]), "secret", "")
	require.Equal(t, http.StatusOK, code, body)
	assert.Len(t, body["list"], 1)
}
