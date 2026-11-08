package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuthorCuration is the manual state of an author.
type AuthorCuration struct {
	ID         int64      `json:"id"`
	Mode       string     `json:"mode"` // rules or allowlist
	MergedInto *int64     `json:"merged_into"`
	Note       string     `json:"note"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

// WorkCuration is the manual state of a work.
type WorkCuration struct {
	ID              int64      `json:"id"`
	Status          *string    `json:"status"` // canon, hidden or null
	MergedInto      *int64     `json:"merged_into"`
	Title           *string    `json:"title"`
	EditionID       *int64     `json:"edition_id"`
	PrimaryAuthorID *int64     `json:"primary_author_id"`
	Note            string     `json:"note"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
}

// EditionCuration is the manual state of an edition.
type EditionCuration struct {
	ID        int64      `json:"id"`
	Language  *string    `json:"language"`
	Hidden    bool       `json:"hidden"`
	Note      string     `json:"note"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Patch is a partial update keyed by field name, plus "id". Absent fields
// are left alone; null (or "") clears a field.
type Patch map[string]json.RawMessage

// CurationRequest applies patches in one transaction: all or nothing.
type CurationRequest struct {
	Source   string  `json:"source"` // Recorded in the log, e.g. "mcp" or "import:canon.tsv".
	Authors  []Patch `json:"authors"`
	Works    []Patch `json:"works"`
	Editions []Patch `json:"editions"`
	// DryRun validates and previews the affected authors, then rolls back.
	DryRun bool `json:"dry_run"`
}

// CurationResult counts rows that changed.
type CurationResult struct {
	Authors   int       `json:"authors"`
	Works     int       `json:"works"`
	Editions  int       `json:"editions"`
	Unchanged int       `json:"unchanged"`
	DryRun    bool      `json:"dry_run"`
	Previews  []Preview `json:"previews,omitempty"` // Dry runs only.
}

// _maxPreviews caps the authors a dry run previews.
const _maxPreviews = 100

var errDryRun = errors.New("dry run")

func badRequest(format string, args ...any) error {
	return errors.Join(errBadRequest, fmt.Errorf(format, args...))
}

// ApplyCuration validates and applies a curation request.
func (s *Store) ApplyCuration(ctx context.Context, req CurationRequest) (CurationResult, error) {
	var res CurationResult
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		res = CurationResult{}
		for i, p := range req.Authors {
			changed, err := patchAuthor(ctx, tx, p, req.Source)
			if err != nil {
				return fmt.Errorf("authors[%d]: %w", i, err)
			}
			count(&res.Authors, &res.Unchanged, changed)
		}
		for i, p := range req.Works {
			changed, err := patchWork(ctx, tx, p, req.Source)
			if err != nil {
				return fmt.Errorf("works[%d]: %w", i, err)
			}
			count(&res.Works, &res.Unchanged, changed)
		}
		for i, p := range req.Editions {
			changed, err := patchEdition(ctx, tx, p, req.Source)
			if err != nil {
				return fmt.Errorf("editions[%d]: %w", i, err)
			}
			count(&res.Editions, &res.Unchanged, changed)
		}
		if err := checkMergeChains(ctx, tx); err != nil {
			return err
		}
		if !req.DryRun {
			return nil
		}
		authors, err := affectedAuthors(ctx, tx, req)
		if err != nil {
			return err
		}
		for _, id := range authors {
			p, err := previewAuthor(ctx, tx, id, "")
			if err != nil {
				return err
			}
			res.Previews = append(res.Previews, p)
		}
		return errDryRun // Roll back.
	})
	if errors.Is(err, errDryRun) {
		res.DryRun, err = true, nil
	}
	return res, err
}

// affectedAuthors lists authors whose curated list a request may change:
// patched authors and their merge targets, and authors listed on, credited
// on or assigned patched works and their merge targets. Run after the
// patches, inside their transaction.
func affectedAuthors(ctx context.Context, tx pgx.Tx, req CurationRequest) ([]int64, error) {
	var authors, works []int64
	for _, p := range req.Authors {
		if id, err := patchID(p); err == nil {
			authors = append(authors, id)
		}
	}
	for _, p := range req.Works {
		if id, err := patchID(p); err == nil {
			works = append(works, id)
		}
	}
	rows, err := tx.Query(ctx, `WITH w AS (
			SELECT unnest($2::bigint[]) AS id
			UNION SELECT merged_into FROM work_curation WHERE work_id = ANY($2) AND merged_into IS NOT NULL
		), a AS (
			SELECT unnest($1::bigint[]) AS id
			UNION SELECT author_id FROM author_works WHERE work_id IN (SELECT id FROM w)
			UNION SELECT author_id FROM work_authors WHERE work_id IN (SELECT id FROM w)
			UNION SELECT primary_author_id FROM work_curation
				WHERE work_id IN (SELECT id FROM w) AND primary_author_id IS NOT NULL
		)
		SELECT id FROM a
		UNION SELECT merged_into FROM author_curation WHERE author_id IN (SELECT id FROM a) AND merged_into IS NOT NULL
		ORDER BY 1 LIMIT $3`, authors, works, _maxPreviews)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func count(changed, unchanged *int, ok bool) {
	if ok {
		*changed++
	} else {
		*unchanged++
	}
}

// checkMergeChains rejects merging into something that is itself merged, so
// resolving a merge is always one step.
func checkMergeChains(ctx context.Context, tx pgx.Tx) error {
	for _, table := range []string{"work_curation", "author_curation"} {
		col := "work_id"
		if table == "author_curation" {
			col = "author_id"
		}
		var id, target int64
		err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT a.%[2]s, a.merged_into FROM %[1]s a
			JOIN %[1]s b ON b.%[2]s = a.merged_into
			WHERE b.merged_into IS NOT NULL LIMIT 1`, table, col)).Scan(&id, &target)
		if err == nil {
			return badRequest("%d is merged into %d, which is itself merged", id, target)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

func patchAuthor(ctx context.Context, tx pgx.Tx, p Patch, source string) (bool, error) {
	id, err := patchID(p)
	if err != nil {
		return false, err
	}
	if err := mustExist(ctx, tx, "authors", id); err != nil {
		return false, err
	}

	cur := AuthorCuration{ID: id, Mode: "rules"}
	err = tx.QueryRow(ctx, `SELECT mode, merged_into, note FROM author_curation WHERE author_id = $1 FOR UPDATE`, id).
		Scan(&cur.Mode, &cur.MergedInto, &cur.Note)
	existed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	before := cur

	err = nil // Reused below; ErrNoRows from the lookup isn't a patch error.
	for k, v := range p {
		switch k {
		case "id":
		case "mode":
			var m *string
			if m, err = optEnum(v, "rules", "allowlist"); err == nil {
				cur.Mode = "rules"
				if m != nil {
					cur.Mode = *m
				}
			}
		case "merged_into":
			cur.MergedInto, err = optInt(v)
		case "note":
			cur.Note, err = str(v)
		default:
			err = fmt.Errorf("unknown author field %q", k)
		}
		if err != nil {
			return false, badRequest("author %d: %s: %v", id, k, err)
		}
	}

	if cur.MergedInto != nil {
		if *cur.MergedInto == id {
			return false, badRequest("author %d can't be merged into itself", id)
		}
		if err := mustExist(ctx, tx, "authors", *cur.MergedInto); err != nil {
			return false, err
		}
	}
	if existed && reflect.DeepEqual(before, cur) {
		return false, nil
	}

	_, err = tx.Exec(ctx, `INSERT INTO author_curation (author_id, mode, merged_into, note) VALUES ($1, $2, $3, $4)
		ON CONFLICT (author_id) DO UPDATE SET mode = EXCLUDED.mode, merged_into = EXCLUDED.merged_into,
			note = EXCLUDED.note, updated_at = NOW()`, id, cur.Mode, cur.MergedInto, cur.Note)
	if err != nil {
		return false, err
	}
	return true, logChange(ctx, tx, "author", id, existed, before, cur, source)
}

func patchWork(ctx context.Context, tx pgx.Tx, p Patch, source string) (bool, error) {
	id, err := patchID(p)
	if err != nil {
		return false, err
	}
	if err := mustExist(ctx, tx, "works", id); err != nil {
		return false, err
	}

	cur := WorkCuration{ID: id}
	err = tx.QueryRow(ctx, `SELECT status, merged_into, title, edition_id, primary_author_id, note
		FROM work_curation WHERE work_id = $1 FOR UPDATE`, id).
		Scan(&cur.Status, &cur.MergedInto, &cur.Title, &cur.EditionID, &cur.PrimaryAuthorID, &cur.Note)
	existed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	before := cur

	err = nil // Reused below; ErrNoRows from the lookup isn't a patch error.
	for k, v := range p {
		switch k {
		case "id":
		case "status":
			cur.Status, err = optEnum(v, "canon", "hidden")
		case "merged_into":
			cur.MergedInto, err = optInt(v)
		case "title":
			cur.Title, err = optString(v)
		case "edition_id":
			cur.EditionID, err = optInt(v)
		case "primary_author_id":
			cur.PrimaryAuthorID, err = optInt(v)
		case "note":
			cur.Note, err = str(v)
		default:
			err = fmt.Errorf("unknown work field %q", k)
		}
		if err != nil {
			return false, badRequest("work %d: %s: %v", id, k, err)
		}
	}

	if cur.MergedInto != nil {
		if *cur.MergedInto == id {
			return false, badRequest("work %d can't be merged into itself", id)
		}
		if err := mustExist(ctx, tx, "works", *cur.MergedInto); err != nil {
			return false, err
		}
	}
	if cur.EditionID != nil {
		// The edition must belong to this work or to one merged into it.
		var ok bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM editions e WHERE e.id = $1 AND (e.work_id = $2
			OR e.work_id IN (SELECT work_id FROM work_curation WHERE merged_into = $2)))`, *cur.EditionID, id).Scan(&ok)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, badRequest("edition %d is not an edition of work %d or a work merged into it", *cur.EditionID, id)
		}
	}
	if cur.PrimaryAuthorID != nil {
		if err := mustExist(ctx, tx, "authors", *cur.PrimaryAuthorID); err != nil {
			return false, err
		}
	}
	if existed && reflect.DeepEqual(before, cur) {
		return false, nil
	}

	_, err = tx.Exec(ctx, `INSERT INTO work_curation (work_id, status, merged_into, title, edition_id, primary_author_id, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (work_id) DO UPDATE SET status = EXCLUDED.status, merged_into = EXCLUDED.merged_into,
			title = EXCLUDED.title, edition_id = EXCLUDED.edition_id,
			primary_author_id = EXCLUDED.primary_author_id, note = EXCLUDED.note, updated_at = NOW()`,
		id, cur.Status, cur.MergedInto, cur.Title, cur.EditionID, cur.PrimaryAuthorID, cur.Note)
	if err != nil {
		return false, err
	}
	return true, logChange(ctx, tx, "work", id, existed, before, cur, source)
}

func patchEdition(ctx context.Context, tx pgx.Tx, p Patch, source string) (bool, error) {
	id, err := patchID(p)
	if err != nil {
		return false, err
	}
	if err := mustExist(ctx, tx, "editions", id); err != nil {
		return false, err
	}

	cur := EditionCuration{ID: id}
	err = tx.QueryRow(ctx, `SELECT language, hidden, note FROM edition_curation WHERE edition_id = $1 FOR UPDATE`, id).
		Scan(&cur.Language, &cur.Hidden, &cur.Note)
	existed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	before := cur

	err = nil // Reused below; ErrNoRows from the lookup isn't a patch error.
	for k, v := range p {
		switch k {
		case "id":
		case "language":
			cur.Language, err = optString(v)
		case "hidden":
			err = json.Unmarshal(v, &cur.Hidden)
		case "note":
			cur.Note, err = str(v)
		default:
			err = fmt.Errorf("unknown edition field %q", k)
		}
		if err != nil {
			return false, badRequest("edition %d: %s: %v", id, k, err)
		}
	}
	if existed && reflect.DeepEqual(before, cur) {
		return false, nil
	}

	_, err = tx.Exec(ctx, `INSERT INTO edition_curation (edition_id, language, hidden, note) VALUES ($1, $2, $3, $4)
		ON CONFLICT (edition_id) DO UPDATE SET language = EXCLUDED.language, hidden = EXCLUDED.hidden,
			note = EXCLUDED.note, updated_at = NOW()`, id, cur.Language, cur.Hidden, cur.Note)
	if err != nil {
		return false, err
	}
	return true, logChange(ctx, tx, "edition", id, existed, before, cur, source)
}

func logChange(ctx context.Context, tx pgx.Tx, kind string, id int64, existed bool, before, after any, source string) error {
	var b []byte
	if existed {
		b, _ = json.Marshal(before)
	}
	a, _ := json.Marshal(after)
	_, err := tx.Exec(ctx, `INSERT INTO curation_log (kind, entity_id, before, after, source) VALUES ($1, $2, $3, $4, $5)`,
		kind, id, b, a, source)
	return err
}

// mustExist checks a store table (authors, works, editions) has the ID.
func mustExist(ctx context.Context, tx pgx.Tx, table string, id int64) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1)`, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return badRequest("%s %d is not in the store", table[:len(table)-1], id)
	}
	return nil
}

func patchID(p Patch) (int64, error) {
	raw, ok := p["id"]
	if !ok {
		return 0, badRequest("missing id")
	}
	id, err := optInt(raw)
	if err != nil || id == nil || *id <= 0 {
		return 0, badRequest("invalid id %s", raw)
	}
	return *id, nil
}

func isNull(v json.RawMessage) bool {
	return len(v) == 0 || string(v) == "null"
}

func optString(v json.RawMessage) (*string, error) {
	if isNull(v) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return nil, err
	}
	if s == "" {
		return nil, nil
	}
	return &s, nil
}

func str(v json.RawMessage) (string, error) {
	s, err := optString(v)
	if s == nil {
		return "", err
	}
	return *s, err
}

// optInt accepts a number or a numeric string.
func optInt(v json.RawMessage) (*int64, error) {
	if isNull(v) || string(v) == `""` {
		return nil, nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return nil, err
		}
		n = json.Number(s)
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

func optEnum(v json.RawMessage, allowed ...string) (*string, error) {
	s, err := optString(v)
	if err != nil || s == nil {
		return s, err
	}
	if !slices.Contains(allowed, *s) {
		return nil, fmt.Errorf("must be one of %v or null", allowed)
	}
	return s, nil
}

// AuthorView is the author list we'd serve with curation applied.
type AuthorView struct {
	AuthorID int64            `json:"author_id"`
	Mode     string           `json:"mode"`
	Works    []int64          `json:"works"`
	Excluded map[int64]string `json:"excluded"` // Candidate work -> why it's left out.
}

// EffectiveWorks applies curation to an author's list. Candidates are the
// served list (author_works), works credited to the author, and works whose
// primary author is overridden to it, including those of authors merged into
// it. Rules mode keeps the served list plus canon and overridden works, minus
// hidden ones; allowlist mode keeps only canon works. Merged works are
// replaced by their target.
func (s *Store) EffectiveWorks(ctx context.Context, authorID int64) (AuthorView, error) {
	return effectiveWorks(ctx, s.db, authorID, "")
}

// querier is a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// effectiveWorks is EffectiveWorks on q; a non-empty mode overrides the
// author's.
func effectiveWorks(ctx context.Context, q querier, authorID int64, mode string) (AuthorView, error) {
	v := AuthorView{AuthorID: authorID, Mode: "rules", Works: []int64{}, Excluded: map[int64]string{}}

	var merged *int64
	err := q.QueryRow(ctx, `SELECT mode, merged_into FROM author_curation WHERE author_id = $1`, authorID).Scan(&v.Mode, &merged)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	if mode != "" {
		v.Mode = mode
	}
	if merged != nil {
		return v, nil // Served under the target author.
	}

	ids := map[int64]bool{authorID: true}
	rows, err := q.Query(ctx, `SELECT author_id FROM author_curation WHERE merged_into = $1`, authorID)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return v, err
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return v, err
	}
	authorIDs := make([]int64, 0, len(ids))
	for id := range ids {
		authorIDs = append(authorIDs, id)
	}

	rows, err = q.Query(ctx, `WITH cand AS (
			SELECT work_id, TRUE AS listed FROM author_works WHERE author_id = ANY($1)
			UNION SELECT work_id, FALSE FROM work_authors WHERE author_id = ANY($1)
			UNION SELECT work_id, FALSE FROM work_curation WHERE primary_author_id = ANY($1)
		)
		SELECT c.work_id, bool_or(c.listed), wc.status, wc.merged_into, wc.primary_author_id,
			t.status, t.primary_author_id
		FROM cand c
		LEFT JOIN work_curation wc ON wc.work_id = c.work_id
		LEFT JOIN work_curation t ON t.work_id = wc.merged_into
		GROUP BY c.work_id, wc.status, wc.merged_into, wc.primary_author_id, t.status, t.primary_author_id`, authorIDs)
	if err != nil {
		return v, err
	}
	defer rows.Close()

	keep := map[int64]bool{}

	for rows.Next() {
		var id int64
		var listed bool
		var status, tStatus *string
		var mergedInto, primary, tPrimary *int64
		if err := rows.Scan(&id, &listed, &status, &mergedInto, &primary, &tStatus, &tPrimary); err != nil {
			return v, err
		}
		if mergedInto != nil {
			v.Excluded[id] = fmt.Sprintf("merged into %d", *mergedInto)
			id, status, primary = *mergedInto, tStatus, tPrimary
		}
		if reason := v.exclude(ids, listed, status, primary); reason != "" {
			if _, seen := v.Excluded[id]; !seen && !keep[id] && reason != "-" {
				v.Excluded[id] = reason
			}
			continue
		}
		keep[id] = true
		delete(v.Excluded, id)
	}
	if err := rows.Err(); err != nil {
		return v, err
	}

	for id := range keep {
		v.Works = append(v.Works, id)
	}
	slices.Sort(v.Works)
	return v, nil
}

// exclude returns why a candidate is left out, or "" to keep it. "-" means
// rules mode never served it (e.g. the author is only the illustrator).
func (v AuthorView) exclude(ids map[int64]bool, listed bool, status *string, primary *int64) string {
	if primary != nil && !ids[*primary] {
		return fmt.Sprintf("credited to %d", *primary)
	}
	if status != nil && *status == "hidden" {
		return "hidden"
	}
	canon := status != nil && *status == "canon"
	if v.Mode == "allowlist" {
		if !canon {
			return "not canon"
		}
		return ""
	}
	if listed || canon || primary != nil {
		return ""
	}
	return "-"
}
