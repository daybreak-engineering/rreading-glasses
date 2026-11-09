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

// siblingKey normalizes a title for finding duplicates: no subtitle, leading
// article, case or punctuation.
func siblingKey(title string) string {
	s := strings.ToLower(title)
	if i := strings.IndexAny(s, ":(["); i >= 0 {
		s = s[:i]
	}
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

type ruleWork struct {
	id          int64
	title       string
	fullTitle   string
	ratingCount int64
	languages   []string
	canon       bool
	gone        bool // Hidden or merged by hand.
	junk        bool
	foreign     bool
}

type verdict struct {
	workID int64
	rule   string
	reason string
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
	}

	marked := map[int64]bool{}
	for _, ids := range lists {
		groups := map[string][]*ruleWork{}
		for _, id := range ids {
			w := works[id]
			if w == nil || w.gone {
				continue
			}
			if key := siblingKey(w.title); key != "" {
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
					cmpTrueFirst(!x.junk && !x.foreign, !y.junk && !y.foreign),
					cmp.Compare(y.ratingCount, x.ratingCount),
					cmp.Compare(x.id, y.id),
				)
			})
			if g[0].junk || g[0].foreign {
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
	Works   int    `json:"works"`
	Junk    int    `json:"junk"`
	Foreign int    `json:"foreign"`
	Sibling int    `json:"sibling"`
	Took    string `json:"took"`
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
			COALESCE(wc.status = 'hidden', FALSE) OR wc.merged_into IS NOT NULL
		FROM works w LEFT JOIN work_curation wc ON wc.work_id = w.id`)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		w := &ruleWork{}
		if err := rows.Scan(&w.id, &w.title, &w.fullTitle, &w.ratingCount, &w.languages, &w.canon, &w.gone); err != nil {
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
		}
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM rule_verdicts`); err != nil {
			return err
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"rule_verdicts"}, []string{"work_id", "rule", "reason"},
			pgx.CopyFromRows(copyRows))
		return err
	})
	res.Took = time.Since(start).Round(time.Millisecond).String()
	return res, err
}

// SetApplyRules makes rule verdicts affect author lists in rules mode.
func (s *Store) SetApplyRules(on bool) {
	s.applyRules = on
}
