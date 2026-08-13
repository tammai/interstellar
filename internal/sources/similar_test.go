package sources

import (
	"strings"
	"testing"
)

// The property that matters most. A false merge deletes a real story before
// anything has judged it — no trace in the sky, no way to notice — while a missed
// duplicate costs one row that the synthesizer usually clusters anyway. So these
// pairs must all stay separate.
func TestSameArticleKeepsDistinctStoriesApart(t *testing.T) {
	pairs := [][2]string{
		// Same subject, different release. The classic false-merge trap.
		{"OpenAI releases GPT-5 to all paid tiers", "OpenAI releases GPT-5.1 to all paid tiers"},
		{"Anthropic raises Series E at $60B", "Anthropic raises Series F at $90B"},
		// Same shape of headline, different company.
		{"Google announces a new open-weight model family", "Meta announces a new open-weight model family"},
		// Opposite claims.
		{"Court rules training on scraped text is fair use", "Court rules training on scraped text is not fair use"},
		// Weekly columns with recurring titles.
		{"AI news roundup for the week of March 3", "AI news roundup for the week of March 10"},
		// Generic short titles from different blogs.
		{"Weekly links", "Weekly links"},
		{"Notes", "Notes"},
	}
	for _, p := range pairs {
		if SameArticle(p[0], p[1]) {
			t.Errorf("merged two distinct stories (%.3f similarity):\n  %q\n  %q",
				TitleSimilarity(p[0], p[1]), p[0], p[1])
		}
	}
}

// And the republishes it exists to catch.
func TestSameArticleCatchesRepublishes(t *testing.T) {
	pairs := [][2]string{
		// Publisher suffix appended by a syndicating outlet.
		{"Agent harness patterns are converging on tool loops",
			"Agent harness patterns are converging on tool loops | TechOutlet"},
		{"Agent harness patterns are converging on tool loops",
			"Agent harness patterns are converging on tool loops — The Other Site"},
		// Case and punctuation differences.
		{"Agent Harness Patterns Are Converging On Tool Loops",
			"agent harness patterns are converging on tool loops"},
		{"Why retrieval beats raw context length, revisited",
			"Why retrieval beats raw context length -- revisited"},
		// Smart quotes and entities, which feeds mangle differently.
		{"The model’s context window isn’t the bottleneck anymore",
			"The model's context window isn't the bottleneck anymore"},
	}
	for _, p := range pairs {
		if !SameArticle(p[0], p[1]) {
			t.Errorf("missed a republish (%.3f similarity):\n  %q\n  %q",
				TitleSimilarity(p[0], p[1]), p[0], p[1])
		}
	}
}

// A short title is never distinctive enough to merge on, however alike.
func TestSameArticleRefusesShortTitles(t *testing.T) {
	if SameArticle("GPT-5 is out", "GPT-5 is out") {
		t.Error("merged on a title too short to be distinctive")
	}
	// Identical and long is fine.
	long := "A sufficiently long and distinctive headline about agent harnesses"
	if !SameArticle(long, long) {
		t.Error("failed to match a long identical title")
	}
}

func TestNormalizeTitleStripsPublisherSuffixes(t *testing.T) {
	cases := map[string]string{
		"Headline here | Some Outlet":       "headline here",
		"Headline here — Some Outlet":       "headline here",
		"Headline here - Outlet":            "headline here",
		"Headline: with a real colon in it": "headline with a real colon in it",
		"Mixed   Whitespace\tand\nnewlines": "mixed whitespace and newlines",
		"Punctuation, removed! Entirely?":   "punctuation removed entirely",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// A long tail after a dash is part of the headline, not a byline, so it stays.
func TestNormalizeTitleKeepsLongTails(t *testing.T) {
	in := "The bottleneck moved - and everyone is still optimising the old one"
	got := NormalizeTitle(in)
	if !strings.Contains(got, "everyone is still optimising the old one") {
		t.Errorf("NormalizeTitle(%q) = %q; a long tail is headline, not a publisher", in, got)
	}
}

func TestTitleSimilarityIsSymmetric(t *testing.T) {
	a := "Agent harness patterns are converging"
	b := "Agent harness patterns converge, mostly"
	if TitleSimilarity(a, b) != TitleSimilarity(b, a) {
		t.Error("similarity is not symmetric")
	}
	if TitleSimilarity("", "") != 0 {
		t.Error("two empty titles should score 0, not 1")
	}
	if TitleSimilarity("anything", "") != 0 {
		t.Error("an empty title should score 0")
	}
}

// A documented limitation, pinned so that anyone lowering the threshold sees what
// they are trading away.
//
// A single substituted preposition scores 0.90 -- below the 0.92 bar -- so this
// republish is missed. That is the intended direction: the same substitution in
// "Google announces X" against "Meta announces X" scores 0.875, and the two cases
// are indistinguishable by token overlap alone. Missing a duplicate costs one extra
// row that the synthesizer usually clusters anyway; merging two companies' launches
// destroys a story with no trace. If this ever needs fixing, it needs a measure that
// knows which word is the subject, not a lower number here.
func TestKnownMissSingleSubstitutedPreposition(t *testing.T) {
	a := "Speech models are finally learning to interrupt politely in conversation"
	b := "Speech models are finally learning to interrupt politely during conversation"

	if SameArticle(a, b) {
		t.Error("this pair now merges; the trade-off documented here has changed and " +
			"the false-merge cases need rechecking")
	}
	if got := TitleSimilarity(a, b); got < 0.85 || got >= TitleSimilarityThreshold {
		t.Errorf("similarity %.3f is no longer just below the threshold; retune deliberately", got)
	}
}

// The disqualifiers, tested directly rather than only through SameArticle.
func TestNumbersAndNegationsBlockMerges(t *testing.T) {
	// Same words, different number: a different release.
	if numbersAgree(NormalizeTitle("OpenAI ships GPT-5"), NormalizeTitle("OpenAI ships GPT-5.1")) {
		t.Error("GPT-5 and GPT-5.1 report the same numbers")
	}
	// Same numbers is not a merge on its own, just not a disqualifier.
	if !numbersAgree(NormalizeTitle("Model 5 arrives"), NormalizeTitle("The 5 model arrives")) {
		t.Error("identical numeric tokens should agree")
	}
	if !negationsAgree(NormalizeTitle("Court allows it"), NormalizeTitle("A court allows it")) {
		t.Error("two titles with no negation should agree")
	}
	if negationsAgree(NormalizeTitle("Court allows it"), NormalizeTitle("Court does not allow it")) {
		t.Error("a negation on one side only should not agree")
	}
}
