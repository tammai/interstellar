package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// maxSignalTitle is the §5.3 cap. Enforced here rather than trusted from the
// model, because the sky has no room to reflow a long title.
const maxSignalTitle = 120

// synthesizeWindowDays bounds which matches a compose pass considers. One day
// would miss a story that broke late yesterday and got picked up this morning.
const synthesizeWindowDays = 2

// maxMatchesPerTopic bounds one compose call.
//
// How many matches a topic has is decided by the classifier's generosity, not by
// us, so this is the backstop that keeps a compose call's cost and latency
// predictable regardless. Forty of a day's best items is already more than a
// person will read; the ones dropped are the lowest-scoring.
const maxMatchesPerTopic = 40

// SynthesizeResult summarizes a compose pass.
type SynthesizeResult struct {
	TopicsRun      int      `json:"topics_run"`
	SignalsWritten int      `json:"signals_written"`
	HotSignals     int      `json:"hot_signals"`
	Problems       []string `json:"problems,omitempty"`
}

type composedSignal struct {
	Title   string  `json:"title"`
	Reason  string  `json:"reason"`
	ItemIDs []int64 `json:"item_ids"`
}

// Synthesize clusters each topic's matched items into signals -- the stars.
//
// This is the pass that resolves rev 1's "dedup vs trending are the same
// problem" note: exact-URL duplicates were already collapsed at ingest, and here
// a model groups *different* links telling the same story. What it is NOT
// allowed to do is count: score and source_count are computed from the database
// afterwards, because source_count decides whether a star is gold and a model
// guessing at it would be both wasteful and wrong.
func Synthesize(
	ctx context.Context,
	db *store.DB,
	client *llm.Client,
	cfg config.Runtime,
	runDate string,
	log *slog.Logger,
) (SynthesizeResult, error) {
	var result SynthesizeResult

	topics, err := db.ActiveTopics()
	if err != nil {
		return result, err
	}

	for _, topic := range topics {
		if topic.NeedsSetup() {
			continue
		}

		matched, err := db.MatchesForTopic(topic.ID, synthesizeWindowDays, maxMatchesPerTopic)
		if err != nil {
			return result, err
		}
		if len(matched) == 0 {
			// No matches is a normal, meaningful outcome: the list view renders
			// "No signal today" for it (§6). Clear the day's group so a stale
			// star from an earlier run today doesn't linger.
			if err := db.ReplaceSignals(runDate, topic.ID, nil); err != nil {
				return result, err
			}
			continue
		}

		result.TopicsRun++

		signals, err := composeTopic(ctx, client, topic, matched, log)
		if err != nil {
			if errors.Is(err, llm.ErrCapReached) || errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return result, err
			}
			log.Warn("synthesize failed", "topic", topic.Title, "err", err)
			result.Problems = append(result.Problems, fmt.Sprintf("topic %q: %v", topic.Title, err))
			continue
		}

		known := make(map[int64]bool, len(matched))
		for _, m := range matched {
			known[m.ItemID] = true
		}

		toWrite := make([]store.NewSignal, 0, len(signals))
		for _, s := range signals {
			title := strings.TrimSpace(s.Title)
			if title == "" {
				continue
			}
			if len(title) > maxSignalTitle {
				title = strings.TrimSpace(sources.Truncate(title, maxSignalTitle))
			}

			// Keep only member ids that were actually in the input. A cluster
			// referencing an item the model invented would produce a star whose
			// detail panel contradicts itself.
			members := make([]int64, 0, len(s.ItemIDs))
			seen := map[int64]bool{}
			for _, id := range s.ItemIDs {
				if known[id] && !seen[id] {
					seen[id] = true
					members = append(members, id)
				}
			}
			if len(members) == 0 {
				continue
			}

			toWrite = append(toWrite, store.NewSignal{
				RunDate: runDate,
				TopicID: topic.ID,
				Title:   title,
				Reason:  strings.TrimSpace(s.Reason),
				ItemIDs: members,
			})
		}

		if err := db.ReplaceSignals(runDate, topic.ID, toWrite); err != nil {
			return result, err
		}
		result.SignalsWritten += len(toWrite)
	}

	// Count hot signals from what was actually stored, since source_count is
	// computed during the write.
	stored, err := db.RecentSignals(1)
	if err == nil {
		for _, s := range stored {
			if s.SourceCount >= cfg.HotThreshold {
				result.HotSignals++
			}
		}
	}

	log.Info("synthesize complete",
		"topics", result.TopicsRun,
		"signals", result.SignalsWritten,
		"hot", result.HotSignals,
	)
	return result, nil
}

func composeTopic(
	ctx context.Context,
	client *llm.Client,
	topic store.Topic,
	matched []store.MatchedItem,
	log *slog.Logger,
) ([]composedSignal, error) {
	prompt := synthesizePrompt(topic, matched)

	resp, err := client.Complete(ctx, llm.RoleSynthesizer, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: synthesizeSystemPrompt},
			{Role: "user", Content: prompt},
		},
		JSONMode: true,
	})
	if err != nil {
		return nil, err
	}

	signals, parseErr := parseComposed(resp.Text)
	if parseErr == nil {
		return signals, nil
	}

	log.Debug("synthesizer output unparseable, repairing", "topic", topic.Title, "err", parseErr)

	repair, repairErr := client.Complete(ctx, llm.RoleSynthesizer, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: synthesizeSystemPrompt},
			{Role: "user", Content: prompt},
			{Role: "assistant", Content: resp.Text},
			{Role: "user", Content: "That was not valid JSON. Reply with only the JSON array, " +
				"no prose and no code fences."},
		},
		JSONMode: true,
	})
	if repairErr != nil {
		return nil, fmt.Errorf("unparseable output and repair failed: %w", repairErr)
	}

	signals, parseErr = parseComposed(repair.Text)
	if parseErr != nil {
		return nil, fmt.Errorf("unparseable output after repair: %w", parseErr)
	}
	return signals, nil
}

func parseComposed(text string) ([]composedSignal, error) {
	elems, err := llm.ExtractArray(text)
	if err != nil {
		return nil, err
	}
	signals, dropped := llm.DecodeElements(elems, func(s composedSignal) error {
		if strings.TrimSpace(s.Title) == "" {
			return errors.New("empty title")
		}
		if len(s.ItemIDs) == 0 {
			return errors.New("no member items")
		}
		return nil
	})
	if dropped > 0 && len(signals) == 0 {
		return nil, fmt.Errorf("all %d clusters malformed", dropped)
	}
	return signals, nil
}

const synthesizeSystemPrompt = `You group news items into stories.

Several items often cover the same event from different angles or outlets. Put
those in one story. An item covering something distinct is its own story.

Reply with ONLY a JSON array:
[{"title": "<max 120 chars>", "reason": "<why this matters to the reader>", "item_ids": [<numbers>]}]

Rules:
- Every item_id you were given must appear in exactly one story.
- Never invent an item_id.
- The title states what happened, concretely. Not "AI news roundup" but what the
  news actually is. No trailing punctuation.
- The reason is one or two sentences on why it is worth the reader's attention,
  written for someone who already cares about this topic. Say what is new or
  what it changes, not that it is "interesting" or "important".
- Do not count sources or rank the stories; that is computed elsewhere.
- Always write the title and reason in English, even when the topic definition is
  written in another language. The definition is the owner's private note; the
  title and reason are what the sky and the list show.

No preamble, no code fences.`

func synthesizePrompt(topic store.Topic, matched []store.MatchedItem) string {
	var b strings.Builder

	fmt.Fprintf(&b, "TOPIC: %s\n\nDEFINITION:\n%s\n\nMATCHED ITEMS:\n", topic.Title, topic.Definition)

	for _, m := range matched {
		summary := m.Summary
		if len(summary) > maxSummaryChars {
			summary = summary[:maxSummaryChars]
		}
		fmt.Fprintf(&b, "\nitem_id: %d\ntitle: %s\n", m.ItemID, m.Title)
		if summary != "" {
			fmt.Fprintf(&b, "summary: %s\n", summary)
		}
		if m.Reason != "" {
			fmt.Fprintf(&b, "why it matched: %s\n", m.Reason)
		}
	}

	fmt.Fprintf(&b, "\nGroup all %d items into stories. Return the JSON array only.", len(matched))
	return b.String()
}
