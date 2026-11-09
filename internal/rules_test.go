package internal

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// _rulesMu serializes tests that run rules: a run replaces every verdict.
var _rulesMu sync.Mutex

func TestJunkReason(t *testing.T) {
	t.Parallel()
	for _, title := range []string{
		"The Hitchhiker's Guide to the Galaxy Box Set", "Dune Omnibus", "Summary of Sapiens",
		"Hamlet: A Graphic Novel", "Manga Classics: Othello", "Dungeon Crawler Carl 8-Book Collection",
		"SparkNotes: Moby Dick", "Emma (Abridged)", "The Hobbit for Kids",
	} {
		assert.NotEmpty(t, junkReason(title), title)
	}
	for _, title := range []string{
		"The Hitchhiker's Guide to the Galaxy", "Life of Pi", "Casebook of the Black Widowers",
		"The Complete Stories", "Letters to a Young Poet", "Summary", "Conversations with Friends",
	} {
		assert.Empty(t, junkReason(title), title)
	}
}

func TestForeignReason(t *testing.T) {
	t.Parallel()
	assert.NotEmpty(t, foreignReason("Бойня", []string{"eng"}), "tags lie")
	assert.NotEmpty(t, foreignReason("Kolijevka za macu", []string{"srp", "hrv"}))
	assert.Empty(t, foreignReason("Galápagos", []string{"eng", "spa"}))
	assert.Empty(t, foreignReason("Galápagos", nil), "no tags, Latin title")
}

func TestSiblingKey(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "classics illustrated typee", siblingKey("Classics Illustrated: Typee"))
	assert.NotEqual(t, siblingKey("Classics Illustrated"), siblingKey("Classics Illustrated: Typee"))
	assert.Equal(t, "dune", siblingKey("The DUNE (Illustrated)"))
	assert.Equal(t, "moby dick", siblingKey("Moby-Dick"))
	assert.Equal(t, "бойня", siblingKey("Бойня"))
	assert.Empty(t, siblingKey("(1984)"))
}

func TestClassifyWorksSiblings(t *testing.T) {
	t.Parallel()
	works := map[int64]*ruleWork{
		1: {id: 1, title: "Dune", ratingCount: 100},
		2: {id: 2, title: "Dune", ratingCount: 3, canon: true},
		3: {id: 3, title: "The Dune", ratingCount: 900, gone: true},
		4: {id: 4, title: "Emma", languages: []string{"rus"}},
		5: {id: 5, title: "Emma", languages: []string{"rus"}},
	}
	got := map[string]string{}
	for _, v := range classifyWorks(works, map[int64][]int64{9: {1, 2, 3, 4, 5}}) {
		got[fmt.Sprint(v.workID, v.rule)] = v.reason
	}
	assert.Equal(t, "duplicate of 2", got["1sibling"], "canon is the keeper, even with fewer ratings")
	assert.NotContains(t, got, "2sibling")
	assert.NotContains(t, got, "3sibling", "hidden works take no part")
	assert.NotContains(t, got, "4sibling", "no sibling verdict without a good keeper")
	assert.NotContains(t, got, "5sibling")
}

func TestRulesAppliedToAuthor(t *testing.T) {
	t.Parallel()
	_rulesMu.Lock()
	defer _rulesMu.Unlock()
	ctx := t.Context()
	base := newTestStore(t)

	ids := testIDs(10)
	authorID := ids[0]
	good, dup, junk, foreign, canonJunk, unknown := ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]
	author := AuthorResource{ForeignID: authorID, Name: "Rule Author"}
	ingest := func(id int64, title string, rating int64, lang string) {
		base.Ingest(ctx, WorkKey(id), mustJSON(t, workResource{
			ForeignID: id, Title: title, FullTitle: title, RatingCount: rating,
			Authors: []AuthorResource{author},
			Books:   []bookResource{{ForeignID: id + 100, Language: lang}},
		}))
	}
	ingest(good, "Rule Novel", 100, "eng")
	ingest(dup, "Rule Novel", 1, "eng")
	ingest(junk, "Rule Novel Study Guide", 1, "eng")
	ingest(foreign, "Роман", 1, "rus")
	ingest(canonJunk, "Rule Saga Omnibus", 1, "eng")

	a := AuthorResource{ForeignID: authorID, Name: "Rule Author"}
	for _, id := range []int64{good, dup, junk, foreign, canonJunk, unknown} {
		a.Works = append(a.Works, workResource{ForeignID: id, Books: []bookResource{}})
	}
	blob := mustJSON(t, a)
	base.Ingest(ctx, AuthorKey(authorID), mustJSON(t, AuthorResource{
		ForeignID: authorID, Name: "Rule Author",
		Works: []workResource{{ForeignID: good}, {ForeignID: dup}, {ForeignID: junk}, {ForeignID: foreign}, {ForeignID: canonJunk}},
	}))
	_, err := base.ApplyCuration(ctx, CurationRequest{Source: "test", Works: []Patch{
		{"id": mustJSON(t, canonJunk), "status": mustJSON(t, "canon")},
	}})
	require.NoError(t, err)

	res, err := base.RunRules(ctx)
	require.NoError(t, err)
	assert.Positive(t, res.Works)
	assert.Positive(t, res.Junk)

	// Verdicts alone change nothing.
	got, err := base.CurateAuthor(ctx, authorID, blob)
	require.NoError(t, err)
	assert.Equal(t, string(blob), string(got))

	p, err := base.PreviewAuthor(ctx, authorID, "", true)
	require.NoError(t, err)
	reasons := map[int64]string{}
	for _, w := range p.Removed {
		reasons[w.ID] = w.Reason
	}
	require.Len(t, reasons, 3, "%v", reasons)
	assert.True(t, strings.HasPrefix(reasons[junk], "rule junk: "), reasons[junk])
	assert.True(t, strings.HasPrefix(reasons[foreign], "rule foreign: "), reasons[foreign])
	assert.Equal(t, fmt.Sprintf("rule sibling: duplicate of %d", good), reasons[dup])

	on := *base
	on.SetApplyRules(true)
	got, err = on.CurateAuthor(ctx, authorID, blob)
	require.NoError(t, err)
	_, works := decodeAuthor(t, got)
	assert.Contains(t, works, good)
	assert.Contains(t, works, canonJunk, "canon beats a junk verdict")
	assert.Contains(t, works, unknown, "works the store hasn't seen stay")
	assert.NotContains(t, works, dup)
	assert.NotContains(t, works, junk)
	assert.NotContains(t, works, foreign)
}

func TestPickEdition(t *testing.T) {
	t.Parallel()
	clean := ruleEdition{id: 1, title: "Cat's Cradle", language: "eng", format: "Paperback", isbn13: "1"}
	ebook := ruleEdition{id: 2, title: "Cat's Cradle: A Novel", language: "eng", format: "eBook", isEbook: true}
	wrong := ruleEdition{id: 3, title: "Motherhood Realized", language: "eng"}
	cyrillic := ruleEdition{id: 4, title: "Колиска для кішки", language: "eng"}
	german := ruleEdition{id: 5, title: "Cat's Cradle", language: "ger"}
	audio := ruleEdition{id: 6, title: "Cat's Cradle", language: "eng", format: "Audible.com"}
	noisy := ruleEdition{id: 7, title: "Cat's Cradle (Illustrated Edition)", language: "eng"}
	all := []ruleEdition{clean, ebook, wrong, cyrillic, german, audio, noisy}

	pick, _ := pickEdition(all, "Cat's Cradle", clean.id)
	assert.Zero(t, pick, "a clean default stays")

	for _, bad := range []ruleEdition{wrong, cyrillic, german, audio} {
		pick, reason := pickEdition(all, "Cat's Cradle", bad.id)
		assert.Equal(t, clean.id, pick, bad.title)
		assert.NotEmpty(t, reason)
	}

	pick, reason := pickEdition([]ruleEdition{noisy, clean}, "Cat's Cradle", noisy.id)
	assert.Equal(t, clean.id, pick)
	assert.Contains(t, reason, "illustrated")

	study := ruleEdition{id: 8, title: "A Study in Scarlet", language: "eng"}
	pick, _ = pickEdition([]ruleEdition{study}, "A Study in Scarlet", study.id)
	assert.Zero(t, pick, "a noise word in the work's own title doesn't count against it")

	pick, _ = pickEdition([]ruleEdition{wrong, german}, "Cat's Cradle", wrong.id)
	assert.Zero(t, pick, "nothing better to switch to")
}

func TestRuleEditionsApplied(t *testing.T) {
	t.Parallel()
	_rulesMu.Lock()
	defer _rulesMu.Unlock()
	ctx := t.Context()
	base := newTestStore(t)

	ids := testIDs(2)
	workID, pinnedWork := ids[0], ids[1]
	const bad, good = 300, 600 // Edition ID offsets from the work's.
	ingest := func(id int64) {
		base.Ingest(ctx, WorkKey(id), mustJSON(t, workResource{
			ForeignID: id, Title: "Edition Test", BestBookID: id + bad,
			Books: []bookResource{{ForeignID: id + bad, Title: "Wrong Book", Language: "eng"}, {ForeignID: id + good, Title: "Edition Test", Language: "eng"}},
		}))
	}
	ingest(workID)
	ingest(pinnedWork)
	_, err := base.ApplyCuration(ctx, CurationRequest{Source: "test", Works: []Patch{
		{"id": mustJSON(t, pinnedWork), "edition_id": mustJSON(t, pinnedWork+bad)},
	}})
	require.NoError(t, err)

	res, err := base.RunRules(ctx)
	require.NoError(t, err)
	assert.Positive(t, res.Editions)

	d, err := base.Work(ctx, workID)
	require.NoError(t, err)
	require.NotNil(t, d.RuleEdition)
	assert.Equal(t, workID+good, *d.RuleEdition)

	blob := func(id int64) []byte {
		return mustJSON(t, workResource{ForeignID: id, BestBookID: id + bad, Books: []bookResource{{ForeignID: id + bad}, {ForeignID: id + good}}})
	}
	got, err := base.CurateWork(ctx, workID, blob(workID))
	require.NoError(t, err)
	assert.Equal(t, string(blob(workID)), string(got), "no effect until rules are applied")

	on := *base
	on.SetApplyRules(true)
	best := func(id int64) int64 {
		got, err := on.CurateWork(ctx, id, blob(id))
		require.NoError(t, err)
		var w workResource
		require.NoError(t, json.Unmarshal(got, &w))
		return w.BestBookID
	}
	assert.Equal(t, workID+good, best(workID))
	assert.Equal(t, pinnedWork+bad, best(pinnedWork), "a hand-picked edition wins")
}
