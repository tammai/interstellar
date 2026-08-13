package sources

import (
	"strings"
	"unicode"
)

// Fuzzy title matching, for the third level of deduplication.
//
// Spec §4 names two mechanisms that are not the same thing: URL canonicalization
// plus INSERT OR IGNORE at ingest (the exact same link from two feeds), and LLM
// clustering at synthesis (different links telling the same story). This sits
// between them and catches what neither does -- the same article at two URLs,
// which is a syndicated republish, an AMP variant, or a feed that changed its
// permalinks.
//
// It runs on titles only, never on content, and it is deliberately conservative.
// A false merge is far worse than a miss: a missed duplicate costs one extra row
// and the synthesizer usually clusters it anyway, while a false merge deletes a
// real story before anything has judged it, leaving no trace in the sky and no way
// to notice. Every threshold here is set to fail toward "these are different".

// TitleSimilarityThreshold is how alike two normalized titles must be to count as
// the same article.
//
// 0.92 is high on purpose. Headlines about the same topic on the same day are
// routinely 0.7-0.85 alike -- "OpenAI releases GPT-5" and "OpenAI releases GPT-5.1"
// are two stories, not one -- and only near-identical strings clear this bar.
const TitleSimilarityThreshold = 0.92

// minTitleRunes is the shortest title worth comparing. Very short titles collide by
// accident: "Weekly links" from two different blogs are unrelated posts that happen
// to share a name.
const minTitleRunes = 18

// NormalizeTitle strips what varies between republishes of the same article.
//
// Case, punctuation and whitespace go; word order and the words themselves stay.
// Publisher suffixes are removed because a syndicating outlet appends its own name
// -- the same piece arrives as "Headline" from the source and "Headline | Other
// Outlet" from the aggregator.
func NormalizeTitle(s string) string {
	// Cut at the last separator, not the first: a headline may legitimately contain
	// a dash or colon, and the publisher suffix is what trails everything else.
	for _, sep := range []string{" | ", " — ", " – ", " - "} {
		if i := strings.LastIndex(s, sep); i > len(sep) {
			// Only treat it as a suffix if what follows is short. A long tail is part
			// of the headline, not a byline.
			if tail := s[i+len(sep):]; len(strings.Fields(tail)) <= 4 {
				s = s[:i]
			}
		}
	}

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		case !lastSpace:
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// TitleSimilarity scores two raw titles from 0 to 1.
//
// Token-set Dice coefficient rather than edit distance: republished headlines
// differ by inserted or dropped words far more often than by typos, and edit
// distance on a 60-character string is dominated by length rather than meaning.
// Comparing sets of words also makes the result symmetric and order-insensitive,
// which is what "the same headline, reformatted" actually looks like.
func TitleSimilarity(a, b string) float64 {
	left := tokenSet(NormalizeTitle(a))
	right := tokenSet(NormalizeTitle(b))
	if len(left) == 0 || len(right) == 0 {
		return 0
	}

	shared := 0
	for tok := range left {
		if right[tok] {
			shared++
		}
	}
	return 2 * float64(shared) / float64(len(left)+len(right))
}

// negations are words whose presence flips a headline's meaning.
//
// One of these is worth more than the eight words around it, and a token-overlap
// score cannot see that: "training on scraped text is fair use" and "...is not fair
// use" are 0.95 alike and report opposite outcomes. Measured, not guessed -- that
// pair merged before this existed.
var negations = map[string]bool{
	"not": true, "no": true, "never": true, "none": true, "without": true,
	"cannot": true, "cant": true, "isnt": true, "wont": true, "doesnt": true,
	"denies": true, "denied": true, "rejects": true, "rejected": true,
	"fails": true, "failed": true, "halts": true, "halted": true,
	"drops": true, "dropped": true, "delays": true, "delayed": true,
}

// SameArticle reports whether two titles are near-certainly the same piece.
//
// Similarity alone is not enough, so this is a conjunction: the titles must be long
// enough to be distinctive, must agree on every number and every negation, and only
// then must clear the similarity bar. The two extra conditions can only ever prevent
// a merge, never cause one, which is the right direction for the whole design.
func SameArticle(a, b string) bool {
	na, nb := NormalizeTitle(a), NormalizeTitle(b)
	if len([]rune(na)) < minTitleRunes || len([]rune(nb)) < minTitleRunes {
		return false
	}
	if na == nb {
		return true
	}
	if !numbersAgree(na, nb) || !negationsAgree(na, nb) {
		return false
	}
	return TitleSimilarity(a, b) >= TitleSimilarityThreshold
}

// numbersAgree requires the two titles to carry the same set of numeric tokens.
//
// Numbers in a headline are almost always the thing that makes it a distinct story:
// a version (GPT-5 against GPT-5.1), a funding round, a date, a count. Normalization
// splits "5.1" into "5" and "1", so the version difference survives as a token even
// though the punctuation does not -- which is exactly what makes this check work.
func numbersAgree(a, b string) bool {
	return equalStringSets(numericTokens(a), numericTokens(b))
}

func negationsAgree(a, b string) bool {
	return equalStringSets(matchingTokens(a, negations), matchingTokens(b, negations))
}

func numericTokens(normalized string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.Fields(normalized) {
		if isNumeric(tok) {
			out[tok] = true
		}
	}
	return out
}

func matchingTokens(normalized string, set map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.Fields(normalized) {
		if set[tok] {
			out[tok] = true
		}
	}
	return out
}

func isNumeric(tok string) bool {
	for _, r := range tok {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return tok != ""
}

func equalStringSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func tokenSet(normalized string) map[string]bool {
	fields := strings.Fields(normalized)
	out := make(map[string]bool, len(fields))
	for _, f := range fields {
		out[f] = true
	}
	return out
}
