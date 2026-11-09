package internal

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// _junkTitle matches titles that aren't an author's own work. It's deliberately
// narrow: rules can remove library entries, so "guide to" (Hitchhiker's) and
// "life of" (Pi) don't belong here.
var _junkTitle = regexp.MustCompile(`(?i)\b(` +
	`box(ed)?\s*set|boxset|\d+\s*-?\s*book\s+(set|collection|bundle)|omnibus|` +
	`books?\s+\d+\s*-\s*\d+|study\s*guide|sparknotes|cliffs?\s*notes|york\s+notes|` +
	`summary\s+of|analysis\s+of|(reader'?s|teacher'?s|study)\s+(guide|companion|edition)|` +
	`lesson\s+plans?|workbook|quiz|colou?ring\s+book|graphic\s+novel|manga|retold|` +
	`abridged|for\s+kids|key\s+takeaways|conversation\s+starters` +
	`)\b`)

// _ruleLanguage is the language a work needs an edition in.
const _ruleLanguage = "eng"

// junkReason returns what makes a title junk, or "".
func junkReason(titles ...string) string {
	for _, t := range titles {
		if m := _junkTitle.FindString(t); m != "" {
			return "title has " + strconv.Quote(strings.ToLower(m))
		}
	}
	return ""
}

// foreignReason returns why a work looks foreign, or "". Hardcover's language
// tags aren't reliable, so a non-Latin title counts even with an English tag.
func foreignReason(title string, languages []string) string {
	if nonLatin(title) {
		return "non-Latin title"
	}
	if len(languages) > 0 && !slices.Contains(languages, _ruleLanguage) {
		return "editions only in " + strings.Join(languages, ", ")
	}
	return ""
}

func nonLatin(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r)
	})
}

// _parenthetical matches a bracketed aside in a title.
var _parenthetical = regexp.MustCompile(`\([^()]*\)|\[[^\[\]]*\]`)

// _numberedAside matches an aside that tells volumes apart. A bare year doesn't.
var _numberedAside = regexp.MustCompile(`(?i)\b(vol|volume|book|part|issue|no)\b|\d`)

// _bareYear matches an aside holding only a year.
var _bareYear = regexp.MustCompile(`^[(\[]\s*\d{4}\s*[)\]]$`)

// _genericSubtitle matches a subtitle that only describes the form, so
// "The Wind's Twelve Quarters: Stories" is still "Wind's Twelve Quarters".
var _genericSubtitle = regexp.MustCompile(`(?i):\s*(an?\s+)?(novel|novella|memoir|stories|short\s+stories|tales|essays|poems|play)\s*$`)

// siblingKey normalizes a title for finding duplicates: no aside, leading
// article, case or punctuation. Numbered asides stay, since "(Volume 2)" and
// "(Volume 3)" are different books.
func siblingKey(title string) string {
	s := strings.ToLower(_parenthetical.ReplaceAllStringFunc(title, func(aside string) string {
		if _numberedAside.MatchString(aside) && !_bareYear.MatchString(aside) {
			return aside
		}
		return " "
	}))
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
	for _, article := range []string{"the ", "a ", "an "} {
		if rest, ok := strings.CutPrefix(s, article); ok {
			return rest
		}
	}
	return s
}

// _comicGenre matches a Hardcover tag for comics.
var _comicGenre = regexp.MustCompile(`(?i)^\W*(comics?|comic\s+books?|graphic\s+novels?(:.*)?|comics\s*&\s*graphic\s+novels.*|` +
	`manga|anime\s*&\s*manga|graphic\s+novels\s*&\s*manga|cartoons\s+and\s+comics|adult\s+graphic\s+novels|.*\scomic\s+books)\W*$`)

// _comicFormat, _comicSeries and _comicTitle match comics by edition format,
// series name and full title.
var (
	_comicFormat = regexp.MustCompile(`(?i)comic|graphic\s+novel|single\s+issue`)
	_comicSeries = regexp.MustCompile(`(?i)single\s+issues|comic\s+adaptation|\(comics?\)`)
	_comicTitle  = regexp.MustCompile(`(?i)comic\s+adaptation|\(comics?\)|single\s+issues`)
)

// comicReason returns why a work looks like a comic, or "". Only the first
// genre counts: tags are most-used first, and prose often carries "Comics"
// further down (The Great Gatsby, Sapiens).
func comicReason(w *ruleWork) string {
	switch {
	case len(w.genres) > 0 && _comicGenre.MatchString(w.genres[0]):
		return "first genre is " + strconv.Quote(w.genres[0])
	case _comicFormat.MatchString(w.format):
		return "default edition format is " + strconv.Quote(w.format)
	case _comicTitle.MatchString(w.fullTitle):
		return "title says so"
	}
	for _, s := range w.series {
		if _comicSeries.MatchString(s) {
			return "series " + strconv.Quote(s)
		}
	}
	return ""
}

// _editionNoise marks editions that make a poor default.
var _editionNoise = regexp.MustCompile(`(?i)\b(` +
	`illustrated|annotated|level\s*\d|study|guide|summary|abridged|retold|graphic|` +
	`large\s+print|edition|condensed|readers?|beginner|a\s+novel|paperback|hardcover` +
	`)\b`)

// _audioFormat matches formats that aren't text.
var _audioFormat = regexp.MustCompile(`(?i)audio|audible|\bcd\b|cassette|mp3`)

type ruleEdition struct {
	id       int64
	title    string
	language string
	format   string
	isbn13   string
	isEbook  bool
}

// headKey is siblingKey for the part of a title before any subtitle.
func headKey(title string) string {
	if i := strings.Index(title, ":"); i >= 0 {
		title = title[:i]
	}
	return siblingKey(title)
}

// editionFit rates an edition as a work's default: 0 can't be one, higher is
// better. reason says why it can't, or what's off about it.
func editionFit(e ruleEdition, workTitle, fullTitle string) (score int, reason string) {
	switch {
	case nonLatin(e.title):
		return 0, "non-Latin title"
	case _audioFormat.MatchString(e.format):
		return 0, "audio format"
	case e.language != "" && e.language != _ruleLanguage:
		return 0, "language " + e.language
	}
	// Language tags lie, so the title has to agree with the work's.
	switch {
	case siblingKey(e.title) == siblingKey(workTitle), siblingKey(e.title) == siblingKey(fullTitle):
		score = 40
	case headKey(e.title) == headKey(workTitle) && headKey(workTitle) != "":
		score = 30
	default:
		return 0, "title doesn't match the work"
	}
	if e.language == _ruleLanguage {
		score += 4
	}
	if e.isEbook {
		score += 2
	}
	if e.isbn13 != "" {
		score++
	}
	// A noise word in the work's own name doesn't count (A Study in Scarlet).
	if m := strings.ToLower(_editionNoise.FindString(e.title)); m != "" && !strings.Contains(strings.ToLower(cmp.Or(fullTitle, workTitle)), m) {
		return score - 20, "title has " + strconv.Quote(m)
	}
	return score, ""
}

// pickEdition returns a better default than the current one, if the current
// one is poor and something clean exists.
func pickEdition(editions []ruleEdition, workTitle, fullTitle string, current int64) (int64, string) {
	reason := "no current edition"
	foreign := false // An untagged edition may be the same translation.
	for _, e := range editions {
		if e.id != current {
			continue
		}
		var score int
		if score, reason = editionFit(e, workTitle, fullTitle); score >= 30 {
			return 0, "" // Fine as it is.
		}
		foreign = e.language != "" && e.language != _ruleLanguage
	}
	var best ruleEdition
	bestScore := 0
	for _, e := range editions {
		score, _ := editionFit(e, workTitle, fullTitle)
		if score < 30 || (foreign && e.language != _ruleLanguage) {
			continue
		}
		if score > bestScore || (score == bestScore && e.id < best.id) {
			best, bestScore = e, score
		}
	}
	if bestScore == 0 {
		return 0, ""
	}
	return best.id, "current edition: " + reason
}

type ruleWork struct {
	id          int64
	title       string
	fullTitle   string
	ratingCount int64
	languages   []string
	genres      []string // Most-used first.
	series      []string
	format      string // Of the default edition.
	canon       bool
	gone        bool // Hidden or merged by hand.
	pinned      bool // Default edition chosen by hand.
	bestBookID  int64
	junk        bool
	foreign     bool
	comic       bool
}

type verdict struct {
	workID int64
	rule   string
	reason string
}

// workKey is the sibling key for a work. It uses the full title because the
// short one can be just a series name ("Hellboy").
func workKey(w *ruleWork) string {
	return siblingKey(_genericSubtitle.ReplaceAllString(cmp.Or(w.fullTitle, w.title), ""))
}

// classifyWorks computes verdicts. lists is each author's served works.
func classifyWorks(works map[int64]*ruleWork, lists map[int64][]int64) []verdict {
	var out []verdict
	for _, w := range works {
		if reason := junkReason(w.title, w.fullTitle); reason != "" {
			w.junk = true
			out = append(out, verdict{w.id, "junk", reason})
		}
		if reason := foreignReason(w.title, w.languages); reason != "" {
			w.foreign = true
			out = append(out, verdict{w.id, "foreign", reason})
		}
		if reason := comicReason(w); reason != "" {
			w.comic = true
			out = append(out, verdict{w.id, "comic", reason})
		}
	}

	marked := map[int64]bool{}
	for _, ids := range lists {
		groups := map[string][]*ruleWork{}
		for _, id := range ids {
			w := works[id]
			if w == nil || w.gone {
				continue
			}
			if key := workKey(w); key != "" {
				groups[key] = append(groups[key], w)
			}
		}
		for _, g := range groups {
			if len(g) < 2 {
				continue
			}
			slices.SortFunc(g, func(x, y *ruleWork) int {
				return cmp.Or(
					cmpTrueFirst(x.canon, y.canon),
					cmpTrueFirst(!x.junk && !x.foreign && !x.comic, !y.junk && !y.foreign && !y.comic),
					cmp.Compare(y.ratingCount, x.ratingCount),
					cmp.Compare(x.id, y.id),
				)
			})
			if g[0].junk || g[0].foreign || g[0].comic {
				continue // Nothing worth keeping to be a duplicate of.
			}
			for _, w := range g[1:] {
				if !marked[w.id] {
					marked[w.id] = true
					out = append(out, verdict{w.id, "sibling", "duplicate of " + strconv.FormatInt(g[0].id, 10)})
				}
			}
		}
	}
	return out
}

func cmpTrueFirst(x, y bool) int {
	switch {
	case x == y:
		return 0
	case x:
		return -1
	}
	return 1
}

// RulesResult summarizes a rules run.
type RulesResult struct {
	Works    int    `json:"works"`
	Junk     int    `json:"junk"`
	Foreign  int    `json:"foreign"`
	Sibling  int    `json:"sibling"`
	Comic    int    `json:"comic"`
	Editions int    `json:"editions"` // Works given a better default edition.
	Took     string `json:"took"`
}

// RunRules recomputes every verdict from stored data. It makes no upstream
// requests and changes nothing served until rules are applied.
func (s *Store) RunRules(ctx context.Context) (RulesResult, error) {
	start := time.Now()
	var res RulesResult

	works := map[int64]*ruleWork{}
	rows, err := s.db.Query(ctx, `SELECT w.id, w.title, w.full_title, w.rating_count,
			COALESCE((SELECT array_agg(DISTINCT e.language) FROM editions e
				WHERE e.work_id = w.id AND e.listed AND e.language <> ''), '{}'),
			COALESCE(wc.status = 'canon', FALSE),
			COALESCE(wc.status = 'hidden', FALSE) OR wc.merged_into IS NOT NULL,
			wc.edition_id IS NOT NULL, COALESCE((w.data->>'BestBookId')::bigint, 0),
			COALESCE(w.data->'Genres', '[]'::jsonb),
			COALESCE((SELECT array_agg(s.title) FROM series_works sw JOIN series s ON s.id = sw.series_id
				WHERE sw.work_id = w.id), '{}'),
			COALESCE((SELECT e.format FROM editions e WHERE e.id = (w.data->>'BestBookId')::bigint), '')
		FROM works w LEFT JOIN work_curation wc ON wc.work_id = w.id`)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		w := &ruleWork{}
		if err := rows.Scan(&w.id, &w.title, &w.fullTitle, &w.ratingCount, &w.languages, &w.canon, &w.gone,
			&w.pinned, &w.bestBookID, &w.genres, &w.series, &w.format); err != nil {
			return res, err
		}
		works[w.id] = w
	}
	if err := rows.Err(); err != nil {
		return res, err
	}

	rows, err = s.db.Query(ctx, `SELECT author_id, work_id FROM author_works`)
	if err != nil {
		return res, err
	}
	lists := map[int64][]int64{}
	var authorID, workID int64
	if _, err := pgx.ForEachRow(rows, []any{&authorID, &workID}, func() error {
		lists[authorID] = append(lists[authorID], workID)
		return nil
	}); err != nil {
		return res, err
	}

	rows, err = s.db.Query(ctx, `SELECT e.work_id, e.id, e.title, e.language, e.format, e.isbn13, e.is_ebook
		FROM editions e LEFT JOIN edition_curation ec ON ec.edition_id = e.id
		WHERE e.listed AND NOT COALESCE(ec.hidden, FALSE)`)
	if err != nil {
		return res, err
	}
	editions := map[int64][]ruleEdition{}
	var e ruleEdition
	if _, err := pgx.ForEachRow(rows, []any{&workID, &e.id, &e.title, &e.language, &e.format, &e.isbn13, &e.isEbook}, func() error {
		editions[workID] = append(editions[workID], e)
		return nil
	}); err != nil {
		return res, err
	}
	var editionRows [][]any
	for id, w := range works {
		if w.pinned || w.gone {
			continue
		}
		if pick, reason := pickEdition(editions[id], w.title, w.fullTitle, w.bestBookID); pick != 0 {
			editionRows = append(editionRows, []any{id, pick, reason})
		}
	}
	res.Editions = len(editionRows)

	verdicts := classifyWorks(works, lists)
	res.Works = len(works)
	copyRows := make([][]any, len(verdicts))
	for i, v := range verdicts {
		copyRows[i] = []any{v.workID, v.rule, v.reason}
		switch v.rule {
		case "junk":
			res.Junk++
		case "foreign":
			res.Foreign++
		case "sibling":
			res.Sibling++
		case "comic":
			res.Comic++
		}
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM rule_verdicts`); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"rule_verdicts"}, []string{"work_id", "rule", "reason"},
			pgx.CopyFromRows(copyRows)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM rule_editions`); err != nil {
			return err
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"rule_editions"}, []string{"work_id", "edition_id", "reason"},
			pgx.CopyFromRows(editionRows))
		return err
	})
	res.Took = time.Since(start).Round(time.Millisecond).String()
	return res, err
}

// SetApplyRules makes rule verdicts affect author lists in rules mode.
func (s *Store) SetApplyRules(on bool) {
	s.applyRules = on
}
