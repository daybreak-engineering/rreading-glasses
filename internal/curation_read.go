package internal

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuthorSummary is one author search hit.
type AuthorSummary struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Mode  string `json:"mode"`
	Works int    `json:"works"` // Served list size before curation.
}

// EditionSummary describes an edition.
type EditionSummary struct {
	ID       int64            `json:"id"`
	WorkID   int64            `json:"work_id"`
	Title    string           `json:"title"`
	Language string           `json:"language"`
	Format   string           `json:"format"`
	Isbn13   string           `json:"isbn13,omitempty"`
	Asin     string           `json:"asin,omitempty"`
	IsEbook  bool             `json:"is_ebook"`
	Listed   bool             `json:"listed"`
	Curation *EditionCuration `json:"curation,omitempty"`
}

// WorkSummary is one row of an author's works.
type WorkSummary struct {
	ID          int64           `json:"id"`
	Title       string          `json:"title"`
	FullTitle   string          `json:"full_title"`
	ReleaseDate string          `json:"release_date"`
	RatingCount int64           `json:"rating_count"`
	InList      bool            `json:"in_list"`          // In today's served list.
	Credit      *int            `json:"credit,omitempty"` // This author's credit position, 0 = first.
	Served      bool            `json:"served"`           // Kept once curation is applied.
	Excluded    string          `json:"excluded,omitempty"`
	Curation    *WorkCuration   `json:"curation,omitempty"`
	BestEdition *EditionSummary `json:"best_edition,omitempty"`
	Editions    int             `json:"editions"`
	Languages   []string        `json:"languages"` // Of listed editions.
}

// AuthorDetail is an author with its candidate works.
type AuthorDetail struct {
	ID       int64          `json:"id"`
	Name     string         `json:"name"`
	Curation AuthorCuration `json:"curation"`
	Total    int            `json:"total"` // Works matching the filter, before paging.
	Works    []WorkSummary  `json:"works"`
}

// AuthorWorksFilter narrows AuthorDetail's works.
type AuthorWorksFilter struct {
	Status string // canon, hidden, none, served, excluded, or "" for all.
	Query  string // Title substring.
	Limit  int
	Offset int
}

// Credit is an author credited on a work.
type Credit struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
}

// SeriesLink is a work's place in a series.
type SeriesLink struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Position string `json:"position"`
}

// WorkDetail is a work with its editions, credits and curation.
type WorkDetail struct {
	ID          int64            `json:"id"`
	Title       string           `json:"title"`
	FullTitle   string           `json:"full_title"`
	ReleaseDate string           `json:"release_date"`
	RatingCount int64            `json:"rating_count"`
	URL         string           `json:"url"`
	BestBookID  int64            `json:"best_book_id"`
	Curation    *WorkCuration    `json:"curation,omitempty"`
	Authors     []Credit         `json:"authors"`
	Series      []SeriesLink     `json:"series"`
	MergedFrom  []int64          `json:"merged_from"`
	Editions    []EditionSummary `json:"editions"`
}

// LogEntry is one curation change.
type LogEntry struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	EntityID int64     `json:"entity_id"`
	Before   any       `json:"before"`
	After    any       `json:"after"`
	Source   string    `json:"source"`
}

// SearchAuthors finds authors by name substring or exact ID.
func (s *Store) SearchAuthors(ctx context.Context, q string, limit int) ([]AuthorSummary, error) {
	rows, err := s.db.Query(ctx, `SELECT a.id, a.name, COALESCE(ac.mode, 'rules'),
			(SELECT count(*) FROM author_works aw WHERE aw.author_id = a.id)
		FROM authors a LEFT JOIN author_curation ac ON ac.author_id = a.id
		WHERE a.name ILIKE '%' || $1 || '%' OR a.id::text = $1
		ORDER BY 4 DESC, a.id LIMIT $2`, q, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuthorSummary, error) {
		var a AuthorSummary
		err := r.Scan(&a.ID, &a.Name, &a.Mode, &a.Works)
		return a, err
	})
}

// AuthorWorks returns an author's candidate works, most-rated first, with
// curation and whether each survives it.
func (s *Store) AuthorWorks(ctx context.Context, authorID int64, f AuthorWorksFilter) (AuthorDetail, error) {
	d := AuthorDetail{ID: authorID, Curation: AuthorCuration{ID: authorID, Mode: "rules"}, Works: []WorkSummary{}}
	err := s.db.QueryRow(ctx, `SELECT name FROM authors WHERE id = $1`, authorID).Scan(&d.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, errNotFound
	}
	if err != nil {
		return d, err
	}
	err = s.db.QueryRow(ctx, `SELECT mode, merged_into, note, updated_at FROM author_curation WHERE author_id = $1`, authorID).
		Scan(&d.Curation.Mode, &d.Curation.MergedInto, &d.Curation.Note, &d.Curation.UpdatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return d, err
	}

	view, err := s.EffectiveWorks(ctx, authorID)
	if err != nil {
		return d, err
	}
	served := map[int64]bool{}
	for _, id := range view.Works {
		served[id] = true
	}

	rows, err := s.db.Query(ctx, `WITH cand AS (
			SELECT work_id FROM author_works WHERE author_id = $1
			UNION SELECT work_id FROM work_authors WHERE author_id = $1
			UNION SELECT work_id FROM work_curation WHERE primary_author_id = $1
		)
		SELECT c.work_id, COALESCE(w.title, ''), COALESCE(w.full_title, ''), COALESCE(w.release_date, ''),
			COALESCE(w.rating_count, 0),
			EXISTS (SELECT 1 FROM author_works aw WHERE aw.author_id = $1 AND aw.work_id = c.work_id),
			(SELECT position FROM work_authors wa WHERE wa.work_id = c.work_id AND wa.author_id = $1),
			wc.work_id IS NOT NULL, wc.status, wc.merged_into, wc.title, wc.edition_id, wc.primary_author_id,
			COALESCE(wc.note, ''), wc.updated_at,
			be.id, COALESCE(be.title, ''), COALESCE(be.language, ''), COALESCE(be.format, ''),
			COALESCE(be.isbn13, ''), COALESCE(be.asin, ''), COALESCE(be.is_ebook, FALSE), COALESCE(be.listed, FALSE),
			(SELECT count(*) FROM editions e WHERE e.work_id = c.work_id),
			ARRAY(SELECT DISTINCT e.language FROM editions e
				WHERE e.work_id = c.work_id AND e.listed AND e.language <> '' ORDER BY 1)
		FROM cand c
		LEFT JOIN works w ON w.id = c.work_id
		LEFT JOIN work_curation wc ON wc.work_id = c.work_id
		LEFT JOIN editions be ON be.id = COALESCE(wc.edition_id, (w.data->>'BestBookId')::bigint)
		ORDER BY COALESCE(w.rating_count, 0) DESC, c.work_id`, authorID)
	if err != nil {
		return d, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (WorkSummary, error) {
		var w WorkSummary
		var hasCuration bool
		var wc WorkCuration
		var beID *int64
		var be EditionSummary
		err := r.Scan(&w.ID, &w.Title, &w.FullTitle, &w.ReleaseDate, &w.RatingCount, &w.InList, &w.Credit,
			&hasCuration, &wc.Status, &wc.MergedInto, &wc.Title, &wc.EditionID, &wc.PrimaryAuthorID, &wc.Note, &wc.UpdatedAt,
			&beID, &be.Title, &be.Language, &be.Format, &be.Isbn13, &be.Asin, &be.IsEbook, &be.Listed,
			&w.Editions, &w.Languages)
		if hasCuration {
			wc.ID = w.ID
			w.Curation = &wc
		}
		if beID != nil {
			be.ID, be.WorkID = *beID, w.ID
			w.BestEdition = &be
		}
		w.Served = served[w.ID]
		w.Excluded = view.Excluded[w.ID]
		return w, err
	})
	if err != nil {
		return d, err
	}

	q := strings.ToLower(f.Query)
	for _, w := range all {
		if q != "" && !strings.Contains(strings.ToLower(w.FullTitle+" "+w.Title), q) {
			continue
		}
		var status string
		if w.Curation != nil && w.Curation.Status != nil {
			status = *w.Curation.Status
		}
		switch f.Status {
		case "canon", "hidden":
			if status != f.Status {
				continue
			}
		case "none":
			if status != "" {
				continue
			}
		case "served":
			if !w.Served {
				continue
			}
		case "excluded":
			if w.Excluded == "" {
				continue
			}
		}
		d.Total++
		if d.Total <= f.Offset || (f.Limit > 0 && len(d.Works) >= f.Limit) {
			continue
		}
		d.Works = append(d.Works, w)
	}
	return d, nil
}

// Work returns a work with its credits, series, merges and editions.
func (s *Store) Work(ctx context.Context, workID int64) (WorkDetail, error) {
	d := WorkDetail{ID: workID, Authors: []Credit{}, Series: []SeriesLink{}, MergedFrom: []int64{}, Editions: []EditionSummary{}}
	var bestBook *int64
	err := s.db.QueryRow(ctx, `SELECT title, full_title, release_date, rating_count, COALESCE(data->>'Url', ''),
			(data->>'BestBookId')::bigint FROM works WHERE id = $1`, workID).
		Scan(&d.Title, &d.FullTitle, &d.ReleaseDate, &d.RatingCount, &d.URL, &bestBook)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, errNotFound
	}
	if err != nil {
		return d, err
	}
	if bestBook != nil {
		d.BestBookID = *bestBook
	}

	var wc WorkCuration
	err = s.db.QueryRow(ctx, `SELECT status, merged_into, title, edition_id, primary_author_id, note, updated_at
		FROM work_curation WHERE work_id = $1`, workID).
		Scan(&wc.Status, &wc.MergedInto, &wc.Title, &wc.EditionID, &wc.PrimaryAuthorID, &wc.Note, &wc.UpdatedAt)
	if err == nil {
		wc.ID = workID
		d.Curation = &wc
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return d, err
	}

	rows, err := s.db.Query(ctx, `SELECT wa.author_id, COALESCE(a.name, ''), wa.position FROM work_authors wa
		LEFT JOIN authors a ON a.id = wa.author_id WHERE wa.work_id = $1 ORDER BY wa.position`, workID)
	if err != nil {
		return d, err
	}
	if d.Authors, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Credit]); err != nil {
		return d, err
	}

	rows, err = s.db.Query(ctx, `SELECT sw.series_id, COALESCE(s.title, ''), sw.position FROM series_works sw
		LEFT JOIN series s ON s.id = sw.series_id WHERE sw.work_id = $1 ORDER BY sw.series_id`, workID)
	if err != nil {
		return d, err
	}
	if d.Series, err = pgx.CollectRows(rows, pgx.RowToStructByPos[SeriesLink]); err != nil {
		return d, err
	}

	rows, err = s.db.Query(ctx, `SELECT work_id FROM work_curation WHERE merged_into = $1 ORDER BY work_id`, workID)
	if err != nil {
		return d, err
	}
	if d.MergedFrom, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
		return d, err
	}

	ids := append([]int64{workID}, d.MergedFrom...)
	d.Editions, err = s.editions(ctx, `e.work_id = ANY($1) ORDER BY e.listed DESC, e.id`, ids)
	return d, err
}

// EditionsByCode finds editions by ISBN-13 or ASIN.
func (s *Store) EditionsByCode(ctx context.Context, isbn13, asin string) ([]EditionSummary, error) {
	if isbn13 != "" {
		return s.editions(ctx, `e.isbn13 = $1 ORDER BY e.id`, isbn13)
	}
	return s.editions(ctx, `e.asin = $1 ORDER BY e.id`, asin)
}

func (s *Store) editions(ctx context.Context, where string, args ...any) ([]EditionSummary, error) {
	rows, err := s.db.Query(ctx, `SELECT e.id, e.work_id, e.title, e.language, e.format, e.isbn13, e.asin,
			e.is_ebook, e.listed, ec.edition_id IS NOT NULL, ec.language, COALESCE(ec.hidden, FALSE),
			COALESCE(ec.note, ''), ec.updated_at
		FROM editions e LEFT JOIN edition_curation ec ON ec.edition_id = e.id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EditionSummary, error) {
		var e EditionSummary
		var has bool
		var ec EditionCuration
		err := r.Scan(&e.ID, &e.WorkID, &e.Title, &e.Language, &e.Format, &e.Isbn13, &e.Asin, &e.IsEbook, &e.Listed,
			&has, &ec.Language, &ec.Hidden, &ec.Note, &ec.UpdatedAt)
		if has {
			ec.ID = e.ID
			e.Curation = &ec
		}
		return e, err
	})
}

// SearchWorks finds works by title substring, optionally credited to an
// author.
func (s *Store) SearchWorks(ctx context.Context, q string, authorID int64, limit int) ([]WorkSummary, error) {
	rows, err := s.db.Query(ctx, `SELECT w.id, w.title, w.full_title, w.release_date, w.rating_count,
			(SELECT count(*) FROM editions e WHERE e.work_id = w.id)
		FROM works w
		WHERE (w.title ILIKE '%' || $1 || '%' OR w.full_title ILIKE '%' || $1 || '%')
			AND ($2::bigint = 0 OR EXISTS (SELECT 1 FROM work_authors wa WHERE wa.work_id = w.id AND wa.author_id = $2)
				OR EXISTS (SELECT 1 FROM author_works aw WHERE aw.work_id = w.id AND aw.author_id = $2))
		ORDER BY w.rating_count DESC, w.id LIMIT $3`, q, authorID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WorkSummary, error) {
		w := WorkSummary{Languages: []string{}}
		err := r.Scan(&w.ID, &w.Title, &w.FullTitle, &w.ReleaseDate, &w.RatingCount, &w.Editions)
		return w, err
	})
}

// CurationLog returns recent changes, newest first, optionally for one
// entity.
func (s *Store) CurationLog(ctx context.Context, kind string, id int64, limit int) ([]LogEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT id, at, kind, entity_id, before, after, source FROM curation_log
		WHERE ($1::text = '' OR kind = $1) AND ($2::bigint = 0 OR entity_id = $2)
		ORDER BY id DESC LIMIT $3`, kind, id, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LogEntry, error) {
		var e LogEntry
		err := r.Scan(&e.ID, &e.At, &e.Kind, &e.EntityID, &e.Before, &e.After, &e.Source)
		return e, err
	})
}

// Preview compares today's served list with the curated one.
type Preview struct {
	AuthorID int64         `json:"author_id"`
	Name     string        `json:"name"`
	Mode     string        `json:"mode"`
	Served   int           `json:"served"`  // Today's list size.
	Curated  int           `json:"curated"` // Size with curation applied.
	Removed  []PreviewWork `json:"removed"`
	Added    []PreviewWork `json:"added"`
}

// PreviewWork is a work that would enter or leave an author's list.
type PreviewWork struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Reason string `json:"reason,omitempty"`
}

// PreviewAuthor reports which works curation would remove from or add to an
// author's served list. A non-empty mode previews that mode instead.
func (s *Store) PreviewAuthor(ctx context.Context, authorID int64, mode string) (Preview, error) {
	return previewAuthor(ctx, s.db, authorID, mode)
}

func previewAuthor(ctx context.Context, q querier, authorID int64, mode string) (Preview, error) {
	p := Preview{AuthorID: authorID, Removed: []PreviewWork{}, Added: []PreviewWork{}}
	view, err := effectiveWorks(ctx, q, authorID, mode)
	if err != nil {
		return p, err
	}
	p.Mode, p.Curated = view.Mode, len(view.Works)
	if err := q.QueryRow(ctx, `SELECT name FROM authors WHERE id = $1`, authorID).Scan(&p.Name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, errNotFound
		}
		return p, err
	}

	rows, err := q.Query(ctx, `SELECT x.id, COALESCE(w.title, ''), x.id = ANY($2) AS curated,
			EXISTS (SELECT 1 FROM author_works aw WHERE aw.author_id = $1 AND aw.work_id = x.id) AS served
		FROM (SELECT work_id AS id FROM author_works WHERE author_id = $1 UNION SELECT unnest($2::bigint[])) x
		LEFT JOIN works w ON w.id = x.id
		ORDER BY COALESCE(w.rating_count, 0) DESC, x.id`, authorID, view.Works)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var w PreviewWork
		var curated, served bool
		if err := rows.Scan(&w.ID, &w.Title, &curated, &served); err != nil {
			return p, err
		}
		if served {
			p.Served++
		}
		switch {
		case served && !curated:
			w.Reason = view.Excluded[w.ID]
			p.Removed = append(p.Removed, w)
		case curated && !served:
			p.Added = append(p.Added, w)
		}
	}
	return p, rows.Err()
}
