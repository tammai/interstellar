package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// weeklyPromptSignals caps how many signals reach the round-up prompt.
//
// A week at ~8 signals a day is under 60, so this is headroom rather than a real
// limit -- but a first run over a backlog can produce far more, and a model handed
// 300 stories writes about the first ten and pads. Ordered by source_count, so what
// gets cut is the least corroborated.
const weeklyPromptSignals = 80

// WeeklyResult summarizes a round-up pass.
type WeeklyResult struct {
	WeekStart string `json:"week_start"`
	WeekEnd   string `json:"week_end"`
	Signals   int    `json:"signals"`
	Topics    int    `json:"topics"`
	Written   bool   `json:"written"`
	// Skipped says why nothing was written, when nothing was.
	Skipped string `json:"skipped,omitempty"`
}

// WriteWeekly composes the round-up for the week containing `when`.
//
// Runs on the SYNTHESIZER's chain and is logged under its own role. The
// synthesizer's job is already "turn a pile of matched items into a story a person
// reads", which is this task at a different scale -- so it gets the good model
// rather than the cheap one, and its own line in the cost log.
func WriteWeekly(
	ctx context.Context,
	db *store.DB,
	client *llm.Client,
	when time.Time,
	log *slog.Logger,
) (WeeklyResult, error) {
	start, end := store.WeekBounds(when)
	result := WeeklyResult{WeekStart: start, WeekEnd: end}

	signals, err := db.SignalsForWeek(start, end)
	if err != nil {
		return result, err
	}
	result.Signals = len(signals)

	topics := map[int64]bool{}
	for _, s := range signals {
		topics[s.TopicID] = true
	}
	result.Topics = len(topics)

	// A week with nothing in it gets no round-up. Writing "there was no news" costs
	// a call on the good model to say something the absence of a digest already
	// says, and it would occupy the week slot so a later re-run looked done.
	if len(signals) == 0 {
		result.Skipped = "no signals in this week"
		return result, nil
	}

	if len(signals) > weeklyPromptSignals {
		signals = signals[:weeklyPromptSignals]
	}

	resp, err := client.CompleteAs(ctx, llm.RoleSynthesizer, llm.RoleWeekly, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: weeklySystemPrompt},
			{Role: "user", Content: weeklyPrompt(start, end, signals)},
		},
		MaxTokens: 1200,
	})
	if err != nil {
		return result, err
	}

	body := strings.TrimSpace(resp.Text)
	if body == "" {
		return result, fmt.Errorf("the model returned an empty round-up")
	}

	if _, err := db.SaveWeeklyDigest(store.WeeklyDigest{
		WeekStart: start, WeekEnd: end, Body: body,
		Signals: result.Signals, Topics: result.Topics,
		TokensIn: resp.Usage.TokensIn, TokensOut: resp.Usage.TokensOut,
	}); err != nil {
		return result, err
	}
	result.Written = true

	log.Info("weekly round-up written",
		"week", start, "signals", result.Signals, "topics", result.Topics)
	return result, nil
}

const weeklySystemPrompt = `You write a short weekly round-up of someone's own news feed.

You are given the week's signals -- stories their pipeline already judged relevant,
each with the topic it matched, how many independent sources carried it, and why it
mattered.

Write plain prose, 200-350 words. Structure it as two or three short paragraphs, or
a few grouped points if the week really was a list. Rules:

- Lead with the single most consequential thing, not a summary of the summary.
- Group by what happened, not by topic. Two topics converging on one story is the
  most interesting thing a week can contain, and a topic-by-topic recital hides it.
- Source count is your evidence of significance: something four outlets carried
  outranks something one blog did, whatever the titles suggest.
- Say what changed. "X released Y" is worth more than "there was discussion of Y".
- If the week was thin, say so briefly rather than inflating it. A short honest
  round-up is worth more than a padded one.
- No preamble, no sign-off, no headings, no markdown, no bullet characters.
- Write in English.`

func weeklyPrompt(start, end string, signals []store.SearchHit) string {
	var b strings.Builder

	fmt.Fprintf(&b, "WEEK: %s to %s\n\nSIGNALS (most corroborated first):\n", start, end)

	for _, s := range signals {
		fmt.Fprintf(&b, "\n- %s\n", s.Title)
		fmt.Fprintf(&b, "  topic: %s · sources: %d · %s\n", s.TopicTitle, s.SourceCount, s.Date)
		if s.Reason != "" {
			fmt.Fprintf(&b, "  why it mattered: %s\n", s.Reason)
		}
	}

	b.WriteString("\nWrite the round-up.")
	return b.String()
}
