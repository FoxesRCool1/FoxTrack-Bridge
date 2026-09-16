package ai

import (
	"sort"
	"strings"
	"unicode"
)

// This is a lexical search over a fixed, small corpus (about 14 topics).
// There is no index and no external search library; a linear scan is correct
// and fast enough.

// SECTION 1 — tokenising.

// helpStopWords are the words dropped from a query before scoring. They carry
// no signal about which topic a user means.
var helpStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "can": true, "do": true, "does": true,
	"for": true, "from": true, "how": true, "i": true, "in": true, "is": true,
	"it": true, "its": true, "me": true, "my": true, "not": true, "of": true,
	"on": true, "or": true, "that": true, "the": true, "to": true, "what": true,
	"when": true, "where": true, "which": true, "why": true, "with": true,
	"you": true, "your": true,
}

// tokenize lowercases s, splits it on every rune that is neither a letter nor
// a digit, and drops tokens shorter than two runes and stop words. Duplicates
// are kept, in order.
func tokenize(s string) []string {
	lower := strings.ToLower(s)
	parts := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len([]rune(p)) < 2 {
			continue
		}
		if helpStopWords[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// SECTION 2 — scoring.

// scoreTopic scores one topic against a query. query is the raw query string
// (used for the whole-phrase bonus) and queryTokens is its tokenised form.
// The score is the sum of per-token contributions plus a one-time bonus for a
// whole-phrase match in the title or body.
func scoreTopic(t Topic, query string, queryTokens []string) int {
	titleLower := strings.ToLower(t.Title)
	bodyLower := strings.ToLower(t.Body)

	keywordLower := make([]string, len(t.Keywords))
	for i, k := range t.Keywords {
		keywordLower[i] = strings.ToLower(k)
	}

	// Deduplicate the query tokens, keeping first-seen order.
	seen := make(map[string]bool, len(queryTokens))
	distinct := make([]string, 0, len(queryTokens))
	for _, tok := range queryTokens {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		distinct = append(distinct, tok)
	}

	score := 0
	for _, tok := range distinct {
		// Keyword: an exact match replaces the substring match.
		exact := false
		contains := false
		for _, k := range keywordLower {
			if k == tok {
				exact = true
				break
			}
			if strings.Contains(k, tok) {
				contains = true
			}
		}
		if exact {
			score += 12
		} else if contains {
			score += 6
		}
		if strings.Contains(titleLower, tok) {
			score += 8
		}
		if strings.Contains(bodyLower, tok) {
			score += 2
		}
	}

	// Whole-phrase bonus: the lowercased, whitespace-collapsed query appearing
	// as a substring of the title or body, counted once.
	phrase := strings.Join(strings.Fields(strings.ToLower(query)), " ")
	if phrase != "" && (strings.Contains(titleLower, phrase) || strings.Contains(bodyLower, phrase)) {
		score += 5
	}

	return score
}

// SECTION 3 — the two search entry points.

const helpMaxResults = 3
const helpExcerptRunes = 900
const helpMinScore = 6

// SearchHit is one search result: a topic's identity plus a bounded excerpt of
// its body.
type SearchHit struct {
	ID        string `json:"topic_id"`
	Title     string `json:"title"`
	Excerpt   string `json:"excerpt"`
	Truncated bool   `json:"truncated,omitempty"`
}

// SearchHelp tokenises the query, scores every topic, and returns the top
// helpMaxResults survivors (score descending, ID ascending for ties). It always
// returns a non-nil slice so it marshals as [] rather than null.
func SearchHelp(query string) []SearchHit {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return []SearchHit{}
	}

	type scored struct {
		topic Topic
		score int
	}
	survivors := make([]scored, 0, len(Topics))
	for _, t := range Topics {
		s := scoreTopic(t, query, tokens)
		if s < helpMinScore {
			continue
		}
		survivors = append(survivors, scored{topic: t, score: s})
	}

	sort.SliceStable(survivors, func(i, j int) bool {
		if survivors[i].score != survivors[j].score {
			return survivors[i].score > survivors[j].score
		}
		return survivors[i].topic.ID < survivors[j].topic.ID
	})

	if len(survivors) > helpMaxResults {
		survivors = survivors[:helpMaxResults]
	}

	hits := make([]SearchHit, 0, len(survivors))
	for _, s := range survivors {
		excerpt, truncated := Excerpt(s.topic.Body)
		hits = append(hits, SearchHit{
			ID:        s.topic.ID,
			Title:     s.topic.Title,
			Excerpt:   excerpt,
			Truncated: truncated,
		})
	}
	return hits
}

// Excerpt returns body unchanged (with false) if it is helpExcerptRunes runes
// or fewer. Otherwise it cuts to helpExcerptRunes runes, trims back to the last
// whitespace so it does not end mid-word, appends a notice, and returns true.
// All lengths are measured in runes.
func Excerpt(body string) (string, bool) {
	runes := []rune(body)
	if len(runes) <= helpExcerptRunes {
		return body, false
	}

	cut := string(runes[:helpExcerptRunes])
	if idx := strings.LastIndexFunc(cut, unicode.IsSpace); idx >= 0 {
		cut = cut[:idx]
	}
	cut = strings.TrimRightFunc(cut, unicode.IsSpace)
	return cut + "\n\n(This topic is longer. Use get_help_topic to read all of it.)", true
}

// BestHelpExcerpts renders the deterministic pre-fetch used to put help into
// the system prompt. It runs SearchHelp and, if anything is found, formats up
// to max hits. A max below 1 is treated as 1.
func BestHelpExcerpts(query string, max int) string {
	hits := SearchHelp(query)
	if len(hits) == 0 {
		return ""
	}
	if max < 1 {
		max = 1
	}
	if len(hits) > max {
		hits = hits[:max]
	}

	var b strings.Builder
	b.WriteString("\n\nHelp library extracts for this message. These came from search_help and count as a search_help result:\n")
	for _, h := range hits {
		b.WriteString("\n--- ")
		b.WriteString(h.Title)
		b.WriteString(" (topic_id: ")
		b.WriteString(h.ID)
		b.WriteString(") ---\n")
		b.WriteString(h.Excerpt)
		b.WriteString("\n")
	}
	return b.String()
}
