package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/notify"
	"github.com/tammai/interstellar/internal/store"
)

// deltaWindowDays is how long a change stays "already said".
//
// Fourteen days, from the brief. Applied to the GROUP (entity + change type),
// not to a single claim -- see store.Delta for why hashing a url instead would
// send one launch once per outlet that covered it.
const deltaWindowDays = 14

// maxDeltasPerPush bounds one message. A digest nobody finishes reading is a
// digest nobody reads, and Slack's own text limit is a worse place to find out.
const maxDeltasPerPush = 40

// urgentTypes post immediately rather than waiting for the daily digest.
//
// An incident is actionable within the hour -- a breach in a tool the owner runs
// in their own pipeline is not a thing to read tomorrow morning -- and an
// acquisition changes what a tool IS, which invalidates a decision rather than
// informing one. Everything else can wait for the batch, and the batch is what
// keeps this from becoming a notification firehose that gets muted.
var urgentTypes = map[string]bool{
	"incident":    true,
	"acquisition": true,
}

// PushResult summarizes a push.
type PushResult struct {
	Deltas  int    `json:"deltas"`
	Urgent  int    `json:"urgent"`
	Sent    bool   `json:"sent"`
	Skipped string `json:"skipped,omitempty"`
}

// Push sends the day's changes to Slack.
//
// An empty run posts NOTHING. That is the whole discipline of a signal channel:
// a daily message saying "nothing happened" trains the reader to stop looking,
// and the first genuinely quiet week would undo whatever attention the channel
// had earned. Silence is the correct output for a quiet day.
func Push(
	ctx context.Context,
	db *store.DB,
	cfg config.Runtime,
	log *slog.Logger,
) (PushResult, error) {
	var result PushResult

	if !cfg.SlackEnabled {
		result.Skipped = "Slack delivery is off"
		return result, nil
	}
	url, ok := cfg.SlackWebhook()
	if !ok {
		// Named by VARIABLE, never by value, the same way an unset provider key
		// is reported: the owner needs to know which env var to set, and the
		// value must never reach a log.
		result.Skipped = fmt.Sprintf("%s is not set in the environment", cfg.SlackWebhookEnv)
		return result, nil
	}

	deltas, err := db.PendingDeltas(deltaWindowDays, maxDeltasPerPush)
	if err != nil {
		return result, err
	}
	result.Deltas = len(deltas)
	if len(deltas) == 0 {
		result.Skipped = "nothing new"
		return result, nil
	}

	// Urgent items go on their own, ahead of the digest, so an incident is not
	// buried at position nine of a layer nobody scrolls to.
	var urgent, ordinary []store.Delta
	for _, d := range deltas {
		if urgentTypes[d.ChangeType] {
			urgent = append(urgent, d)
		} else {
			ordinary = append(ordinary, d)
		}
	}
	result.Urgent = len(urgent)

	var sentClaims []int64
	for _, d := range urgent {
		if err := notify.SendSlack(ctx, url, notify.SlackMessage{Text: urgentText(d)}); err != nil {
			// Stop at the first failure and mark only what actually went. A claim
			// stamped as pushed but never delivered is gone -- the next run's
			// selection will not see it again.
			log.Warn("urgent push failed", "entity", d.Entity, "err", err)
			return result, markSent(db, sentClaims, err)
		}
		sentClaims = append(sentClaims, d.Claims...)
	}

	if len(ordinary) > 0 {
		if err := notify.SendSlack(ctx, url, notify.SlackMessage{Text: digestText(ordinary)}); err != nil {
			log.Warn("digest push failed", "err", err)
			return result, markSent(db, sentClaims, err)
		}
		for _, d := range ordinary {
			sentClaims = append(sentClaims, d.Claims...)
		}
	}

	if err := db.MarkDeltasPushed(sentClaims); err != nil {
		return result, err
	}
	result.Sent = true

	log.Info("pushed deltas", "deltas", result.Deltas, "urgent", result.Urgent)
	return result, nil
}

// markSent records a partial delivery and returns the original failure.
func markSent(db *store.DB, claimIDs []int64, cause error) error {
	if err := db.MarkDeltasPushed(claimIDs); err != nil {
		return fmt.Errorf("%w (and could not record what was sent: %v)", cause, err)
	}
	return cause
}

func urgentText(d store.Delta) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s* — %s\n", strings.ToUpper(d.ChangeType), d.Entity)
	b.WriteString(d.Summary)
	b.WriteString("\n")
	b.WriteString(sourceLine(d))
	return b.String()
}

// digestText groups the day's changes by layer.
//
// By layer rather than by change type, because the layer is how the owner thinks
// about the field -- the twelve of them are the map -- and a reader scanning for
// "anything in code review?" should find one place to look.
func digestText(deltas []store.Delta) string {
	byLayer := map[string][]store.Delta{}
	for _, d := range deltas {
		byLayer[d.Layer] = append(byLayer[d.Layer], d)
	}
	layers := make([]string, 0, len(byLayer))
	for l := range byLayer {
		layers = append(layers, l)
	}
	// Busiest layer first: the most-changed corner of the field is the one worth
	// reading, and alphabetical order would put agent_frameworks above it forever.
	sort.Slice(layers, func(i, j int) bool {
		if len(byLayer[layers[i]]) != len(byLayer[layers[j]]) {
			return len(byLayer[layers[i]]) > len(byLayer[layers[j]])
		}
		return layers[i] < layers[j]
	})

	var b strings.Builder
	// plural() is weekly.go's, shared rather than reimplemented: two helpers with
	// the same name and different signatures in one package is a compile error the
	// first time and a silent inconsistency in the copy every time after.
	fmt.Fprintf(&b, "*%s today*\n", plural(len(deltas), "change", "changes"))

	for _, layer := range layers {
		fmt.Fprintf(&b, "\n*%s*\n", strings.ReplaceAll(layer, "_", " "))
		for _, d := range byLayer[layer] {
			fmt.Fprintf(&b, "• *%s* (%s) — %s\n  %s\n",
				d.Entity, d.ChangeType, d.Summary, sourceLine(d))
		}
	}
	return b.String()
}

// sourceLine states corroboration and links the best source.
//
// The count is here for the same reason signals.source_count drives the gold
// flag: several outlets carrying one story is the strongest available evidence
// that it matters, and it is the thing a one-line summary cannot convey.
func sourceLine(d store.Delta) string {
	var parts []string
	if d.Verified {
		parts = append(parts, "from the vendor")
	}
	if d.Sources > 1 {
		parts = append(parts, fmt.Sprintf("%d sources", d.Sources))
	}
	suffix := ""
	if len(parts) > 0 {
		suffix = " · " + strings.Join(parts, " · ")
	}
	return "<" + d.URL + "|link>" + suffix
}
