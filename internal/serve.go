package internal

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Curator applies curation to responses on their way out. It's applied in the
// handler only: the controller's cached records stay upstream data, so
// internal readers (denormalization, refreshes) never see curated lists.
type Curator interface {
	CurateAuthor(ctx context.Context, authorID int64, blob []byte) ([]byte, error)
	CurateWork(ctx context.Context, workID int64, blob []byte) ([]byte, error)
	CurateBook(ctx context.Context, blob []byte) ([]byte, error)
	CurateSearch(ctx context.Context, results []SearchResource) ([]SearchResource, error)
}

var _ Curator = (*Store)(nil)

// CurateAuthor applies curation to a cached author response. Without curation
// touching the author it returns blob unchanged.
//
// Curation only removes what it names: in rules mode, a work the store hasn't
// seen stays. Allowlist mode keeps only canon works, and a merged author keeps
// nothing.
func (s *Store) CurateAuthor(ctx context.Context, authorID int64, blob []byte) ([]byte, error) {
	var a AuthorResource
	if err := json.Unmarshal(blob, &a); err != nil {
		return nil, fmt.Errorf("decoding author %d: %w", authorID, err)
	}
	view, err := effectiveWorks(ctx, s.db, authorID, "")
	if err != nil {
		return nil, err
	}

	inView := make(map[int64]bool, len(view.Works))
	for _, id := range view.Works {
		inView[id] = true
	}
	inBlob := make(map[int64]bool, len(a.Works))
	kept := make([]workResource, 0, len(a.Works))
	for _, w := range a.Works {
		inBlob[w.ForeignID] = true
		switch {
		case view.MergedInto != nil:
		case inView[w.ForeignID]:
			kept = append(kept, w)
		case view.Mode == "rules" && view.Excluded[w.ForeignID] == "":
			kept = append(kept, w)
		}
	}
	removed := len(kept) != len(a.Works)

	var missing []int64
	for _, id := range view.Works {
		if !inBlob[id] {
			missing = append(missing, id)
		}
	}
	added, err := s.curatedAdditions(ctx, missing)
	if err != nil {
		return nil, err
	}
	kept = append(kept, added...)

	works, changed, err := s.curateWorks(ctx, kept)
	if err != nil {
		return nil, err
	}
	if !removed && len(added) == 0 && !changed {
		return blob, nil
	}

	if len(added) > 0 {
		slices.SortFunc(works, func(x, y workResource) int { return cmp.Compare(x.ForeignID, y.ForeignID) })
	}
	a.Works = works
	if removed {
		a.Series = dropSeriesLinks(a.Series, works)
	}
	return json.Marshal(a)
}

// CurateWork applies curation to a cached work response. Hidden and merged
// works must stay fetchable by ID: Bookshelf refreshes a manually added book
// that left its author's list through this lookup, and keeps it if it resolves.
func (s *Store) CurateWork(ctx context.Context, workID int64, blob []byte) ([]byte, error) {
	var w workResource
	if err := json.Unmarshal(blob, &w); err != nil {
		return nil, fmt.Errorf("decoding work %d: %w", workID, err)
	}
	works, changed, err := s.curateWorks(ctx, []workResource{w})
	if err != nil {
		return nil, err
	}
	if !changed {
		return blob, nil
	}
	return json.Marshal(works[0])
}

// curatedAdditions builds works curation adds to a list. Ones without curation
// are skipped: a work in author_works but not the cached list only means the
// cached list is an in-progress refresh's snapshot.
func (s *Store) curatedAdditions(ctx context.Context, ids []int64) ([]workResource, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `SELECT w.data FROM works w WHERE w.id = ANY($1)
		AND EXISTS (SELECT 1 FROM work_curation wc WHERE wc.work_id = w.id OR wc.merged_into = w.id)`, ids)
	if err != nil {
		return nil, err
	}
	datas, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	out := make([]workResource, 0, len(datas))
	workIDs := make([]int64, 0, len(datas))
	for _, data := range datas {
		var w workResource
		if err := json.Unmarshal(data, &w); err != nil {
			return nil, err
		}
		out = append(out, w)
		workIDs = append(workIDs, w.ForeignID)
	}
	books, err := s.listedEditions(ctx, workIDs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Books = books[out[i].ForeignID]
	}
	return out, nil
}

// listedEditions returns the listed editions of some works, by work.
func (s *Store) listedEditions(ctx context.Context, workIDs []int64) (map[int64][]bookResource, error) {
	out := map[int64][]bookResource{}
	if len(workIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT work_id, data FROM editions WHERE work_id = ANY($1) AND listed ORDER BY id`, workIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var workID int64
		var data []byte
		if err := rows.Scan(&workID, &data); err != nil {
			return nil, err
		}
		var b bookResource
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		out[workID] = append(out[workID], b)
	}
	return out, rows.Err()
}

// curateWorks applies work and edition curation in place. It reports whether
// anything changed.
func (s *Store) curateWorks(ctx context.Context, works []workResource) ([]workResource, bool, error) {
	if len(works) == 0 {
		return works, false, nil
	}
	ids := make([]int64, len(works))
	for i, w := range works {
		ids[i] = w.ForeignID
	}

	rows, err := s.db.Query(ctx, `SELECT work_id, merged_into, title, edition_id FROM work_curation
		WHERE work_id = ANY($1) OR merged_into = ANY($1)`, ids)
	if err != nil {
		return nil, false, err
	}
	cur := map[int64]WorkCuration{}
	sources := map[int64]int64{} // Source -> target, for targets in works.
	var sourceIDs, picked []int64
	inWorks := make(map[int64]bool, len(ids))
	for _, id := range ids {
		inWorks[id] = true
	}
	for rows.Next() {
		var id int64
		var wc WorkCuration
		if err := rows.Scan(&id, &wc.MergedInto, &wc.Title, &wc.EditionID); err != nil {
			rows.Close()
			return nil, false, err
		}
		cur[id] = wc
		if wc.MergedInto != nil && inWorks[*wc.MergedInto] {
			sources[id] = *wc.MergedInto
			sourceIDs = append(sourceIDs, id)
		}
		if wc.EditionID != nil {
			picked = append(picked, *wc.EditionID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	fromSources, err := s.listedEditions(ctx, sourceIDs)
	if err != nil {
		return nil, false, err
	}
	extra := map[int64][]bookResource{} // Target -> merged-in editions.
	for src, books := range fromSources {
		extra[sources[src]] = append(extra[sources[src]], books...)
	}
	pickedBooks, err := s.editionsByID(ctx, picked)
	if err != nil {
		return nil, false, err
	}

	var editionIDs []int64
	for _, w := range works {
		for _, b := range w.Books {
			editionIDs = append(editionIDs, b.ForeignID)
		}
		for _, b := range extra[w.ForeignID] {
			editionIDs = append(editionIDs, b.ForeignID)
		}
	}
	editionIDs = append(editionIDs, picked...)
	eds, err := s.editionCurations(ctx, editionIDs)
	if err != nil {
		return nil, false, err
	}

	changed := false
	for i := range works {
		w := &works[i]
		wc, has := cur[w.ForeignID]
		if !has {
			wc = WorkCuration{}
		}
		if applyWork(w, wc, extra[w.ForeignID], pickedBooks, eds) {
			changed = true
		}
	}
	return works, changed, nil
}

func (s *Store) editionsByID(ctx context.Context, ids []int64) (map[int64]bookResource, error) {
	out := map[int64]bookResource{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT data FROM editions WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	datas, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, err
	}
	for _, data := range datas {
		var b bookResource
		if err := json.Unmarshal(data, &b); err != nil {
			return nil, err
		}
		out[b.ForeignID] = b
	}
	return out, nil
}

func (s *Store) editionCurations(ctx context.Context, ids []int64) (map[int64]EditionCuration, error) {
	out := map[int64]EditionCuration{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT edition_id, language, hidden FROM edition_curation WHERE edition_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ec EditionCuration
		if err := rows.Scan(&ec.ID, &ec.Language, &ec.Hidden); err != nil {
			return nil, err
		}
		out[ec.ID] = ec
	}
	return out, rows.Err()
}

// applyWork applies one work's curation and reports whether it changed
// anything.
func applyWork(w *workResource, wc WorkCuration, merged []bookResource, picked map[int64]bookResource, eds map[int64]EditionCuration) bool {
	changed := false
	if wc.Title != nil && (w.Title != *wc.Title || w.FullTitle != *wc.Title || w.ShortTitle != *wc.Title) {
		w.Title, w.FullTitle, w.ShortTitle = *wc.Title, *wc.Title, *wc.Title
		changed = true
	}

	have := make(map[int64]bool, len(w.Books)+len(merged))
	for _, b := range w.Books {
		have[b.ForeignID] = true
	}
	for _, b := range merged {
		if !have[b.ForeignID] {
			have[b.ForeignID] = true
			w.Books = append(w.Books, b)
			changed = true
		}
	}
	if wc.EditionID != nil {
		// The pick may be an edition the work doesn't list.
		if b, ok := picked[*wc.EditionID]; ok && !have[b.ForeignID] {
			have[b.ForeignID] = true
			w.Books = append(w.Books, b)
			changed = true
		}
		if have[*wc.EditionID] && w.BestBookID != *wc.EditionID {
			w.BestBookID = *wc.EditionID
			changed = true
		}
	}

	before, bestHidden := changed, false
	var books []bookResource
	for _, b := range w.Books {
		ec, has := eds[b.ForeignID]
		// A hidden edition stays if it was explicitly picked as the default.
		if has && ec.Hidden && (wc.EditionID == nil || *wc.EditionID != b.ForeignID) {
			changed = true
			bestHidden = bestHidden || b.ForeignID == w.BestBookID
			continue
		}
		if has && ec.Language != nil && b.Language != *ec.Language {
			b.Language = *ec.Language
			changed = true
		}
		books = append(books, b)
	}
	// Never serve a work with no editions.
	if len(books) == 0 {
		return before
	}
	if bestHidden {
		w.BestBookID = books[0].ForeignID
	}
	w.Books = books
	return changed
}

// dropSeriesLinks removes links to works no longer in the list.
func dropSeriesLinks(series []SeriesResource, works []workResource) []SeriesResource {
	in := make(map[int64]bool, len(works))
	for _, w := range works {
		in[w.ForeignID] = true
	}
	out := make([]SeriesResource, 0, len(series))
	for _, sr := range series {
		sr.LinkItems = slices.DeleteFunc(slices.Clone(sr.LinkItems), func(l seriesWorkLinkResource) bool {
			return !in[l.ForeignWorkID]
		})
		if len(sr.LinkItems) > 0 {
			out = append(out, sr)
		}
	}
	return out
}

// workInfo is what lookups need to know about a work.
type workInfo struct {
	status          *string
	mergedInto      *int64
	editionID       *int64
	primaryAuthorID *int64
	bestBookID      *int64
}

// best is the edition a lookup should land on, if known.
func (i workInfo) best() *int64 {
	if i.editionID != nil {
		return i.editionID
	}
	return i.bestBookID
}

func (s *Store) workInfos(ctx context.Context, ids []int64) (map[int64]workInfo, error) {
	out := make(map[int64]workInfo, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT w.id, wc.status, wc.merged_into, wc.edition_id, wc.primary_author_id,
			(w.data->>'BestBookId')::bigint
		FROM works w LEFT JOIN work_curation wc ON wc.work_id = w.id WHERE w.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var i workInfo
		if err := rows.Scan(&id, &i.status, &i.mergedInto, &i.editionID, &i.primaryAuthorID, &i.bestBookID); err != nil {
			return nil, err
		}
		out[id] = i
	}
	return out, rows.Err()
}

// mergedAuthors maps merged-away authors among ids to their targets.
func (s *Store) mergedAuthors(ctx context.Context, ids []int64) (map[int64]AuthorResource, error) {
	out := map[int64]AuthorResource{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT ac.author_id, a.data FROM author_curation ac
		JOIN authors a ON a.id = ac.merged_into WHERE ac.author_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		var a AuthorResource
		if err := json.Unmarshal(data, &a); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

// CurateSearch drops hidden works from search results and resolves merged
// works and authors, keeping rank. Works the store doesn't know pass through.
func (s *Store) CurateSearch(ctx context.Context, results []SearchResource) ([]SearchResource, error) {
	workIDs := make([]int64, 0, len(results))
	bookIDs := make([]int64, 0, len(results))
	authorIDs := make([]int64, 0, len(results))
	for _, r := range results {
		workIDs = append(workIDs, r.WorkID)
		bookIDs = append(bookIDs, r.BookID)
		authorIDs = append(authorIDs, r.Author.ID)
	}
	infos, err := s.workInfos(ctx, workIDs)
	if err != nil {
		return nil, err
	}
	var targets []int64
	for _, i := range infos {
		if i.mergedInto != nil {
			targets = append(targets, *i.mergedInto)
		}
	}
	targetInfos, err := s.workInfos(ctx, targets)
	if err != nil {
		return nil, err
	}
	eds, err := s.editionCurations(ctx, bookIDs)
	if err != nil {
		return nil, err
	}
	authors, err := s.mergedAuthors(ctx, authorIDs)
	if err != nil {
		return nil, err
	}

	out := make([]SearchResource, 0, len(results))
	seen := make(map[int64]bool, len(results))
	for _, r := range results {
		info := infos[r.WorkID]
		if info.mergedInto != nil {
			r.WorkID = *info.mergedInto
			info = targetInfos[r.WorkID]
			if b := info.best(); b != nil {
				r.BookID = *b
			}
		}
		if info.status != nil && *info.status == "hidden" {
			continue
		}
		if ec, has := eds[r.BookID]; has && ec.Hidden {
			if b := info.best(); b != nil {
				r.BookID = *b
			}
		}
		if info.primaryAuthorID != nil {
			r.Author.ID = *info.primaryAuthorID
		}
		if a, merged := authors[r.Author.ID]; merged {
			r.Author.ID = a.ForeignID
		}
		if seen[r.WorkID] {
			continue
		}
		seen[r.WorkID] = true
		out = append(out, r)
	}
	return out, nil
}

// CurateBook applies curation to an edition lookup, whose response is the
// edition's work with just that edition. An edition of a merged work resolves
// to the target. Hidden works stay as they are: a direct lookup still finds
// them.
func (s *Store) CurateBook(ctx context.Context, blob []byte) ([]byte, error) {
	var w workResource
	if err := json.Unmarshal(blob, &w); err != nil {
		return nil, fmt.Errorf("decoding book: %w", err)
	}
	infos, err := s.workInfos(ctx, []int64{w.ForeignID})
	if err != nil {
		return nil, err
	}
	info := infos[w.ForeignID]
	changed := false

	if info.mergedInto != nil {
		var data []byte
		err := s.db.QueryRow(ctx, `SELECT data FROM works WHERE id = $1`, *info.mergedInto).Scan(&data)
		if err != nil {
			return nil, fmt.Errorf("loading merge target %d: %w", *info.mergedInto, err)
		}
		var target workResource
		if err := json.Unmarshal(data, &target); err != nil {
			return nil, err
		}
		target.Books = w.Books
		w, changed = target, true
		targetInfos, err := s.workInfos(ctx, []int64{w.ForeignID})
		if err != nil {
			return nil, err
		}
		info = targetInfos[w.ForeignID]
	}

	var titled WorkCuration
	if err := s.db.QueryRow(ctx, `SELECT title FROM work_curation WHERE work_id = $1`, w.ForeignID).
		Scan(&titled.Title); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	ids := make([]int64, 0, len(w.Books))
	for _, b := range w.Books {
		ids = append(ids, b.ForeignID)
	}
	eds, err := s.editionCurations(ctx, ids)
	if err != nil {
		return nil, err
	}
	if applyWork(&w, titled, nil, nil, eds) {
		changed = true
	}

	if info.primaryAuthorID != nil && len(w.Authors) > 0 && w.Authors[0].ForeignID != *info.primaryAuthorID {
		if i := slices.IndexFunc(w.Authors, func(a AuthorResource) bool { return a.ForeignID == *info.primaryAuthorID }); i > 0 {
			a := w.Authors[i]
			w.Authors = append([]AuthorResource{a}, slices.Delete(slices.Clone(w.Authors), i, i+1)...)
			changed = true
		}
	}
	authorIDs := make([]int64, len(w.Authors))
	for i, a := range w.Authors {
		authorIDs[i] = a.ForeignID
	}
	merged, err := s.mergedAuthors(ctx, authorIDs)
	if err != nil {
		return nil, err
	}
	for i, a := range w.Authors {
		if t, has := merged[a.ForeignID]; has {
			w.Authors[i] = t
			changed = true
		}
	}

	if !changed {
		return blob, nil
	}
	return json.Marshal(w)
}
