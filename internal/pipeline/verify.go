package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// BatchReport is one classify batch's outcome.
type BatchReport struct {
	Index    int
	Items    int
	Verdicts int
	// Dropped counts verdicts that were not answers to this batch: an id that
	// was not in it, or one answered twice. Surfaced because a model doing
	// either is a reason to distrust it, and netting out against a truncated
	// batch is how it would otherwise stay invisible.
	Dropped  int
	Repaired bool
	Err      error
	Latency  time.Duration
}

// VerifyReport is the §10 gate result: "classifier JSON parses 100% on a
// 200-item sample".
type VerifyReport struct {
	Model        string
	Provider     string
	Topic        string
	Items        int
	Batches      []BatchReport
	ParsedOK     int
	Repairs      int
	MissingItems int // items the model never returned a verdict for
	Elapsed      time.Duration
}

// Passed reports whether every batch parsed.
//
// Repairs count as a pass: the pipeline recovers from them in production, and a
// gate that failed on a recovered batch would reject models that work. They are
// reported separately so a model needing constant repair is still visible.
func (r VerifyReport) Passed() bool {
	return len(r.Batches) > 0 && r.ParsedOK == len(r.Batches)
}

// VerifyClassifier runs a sample of the instance's own items through the real
// classify path.
//
// It deliberately reuses classifyBatch rather than reimplementing the prompt: a
// gate that exercised a different code path would prove nothing about the code
// that actually runs. Sampling the instance's own items matters too — a model
// that handles synthetic titles may still choke on the punctuation, emoji and
// mixed languages real feeds produce.
func VerifyClassifier(
	ctx context.Context,
	db *store.DB,
	client *llm.Client,
	cfg config.Runtime,
	sampleSize int,
	log *slog.Logger,
) (VerifyReport, error) {
	report := VerifyReport{}

	topics, err := db.ActiveTopics()
	if err != nil {
		return report, err
	}
	var topic store.Topic
	for _, t := range topics {
		if !t.NeedsSetup() {
			topic = t
			break
		}
	}
	if topic.ID == 0 {
		return report, fmt.Errorf("no topic has a definition yet; write one before running the gate")
	}
	report.Topic = topic.Title

	// An empty watermark means "everything", which is what a sample wants.
	items, err := db.ItemsSince("", sampleSize)
	if err != nil {
		return report, err
	}
	if len(items) == 0 {
		return report, fmt.Errorf("no items to sample; run a refresh first")
	}
	report.Items = len(items)

	batchSize := cfg.ClassifyBatchSize
	if batchSize <= 0 {
		batchSize = 30
	}

	start := time.Now()
	seen := map[int64]bool{}

	for i, offset := 0, 0; offset < len(items); i, offset = i+1, offset+batchSize {
		end := min(offset+batchSize, len(items))
		batch := items[offset:end]

		batchStart := time.Now()
		verdicts, dropped, repaired, err := classifyBatch(ctx, client, topic, batch, log)
		br := BatchReport{
			Index:    i + 1,
			Items:    len(batch),
			Verdicts: len(verdicts),
			Dropped:  dropped,
			Repaired: repaired,
			Err:      err,
			Latency:  time.Since(batchStart),
		}

		if err == nil {
			report.ParsedOK++
			for _, v := range verdicts {
				seen[v.ItemID] = true
			}
		}
		if repaired {
			report.Repairs++
		}
		report.Batches = append(report.Batches, br)

		if ctx.Err() != nil {
			break
		}
	}

	for _, it := range items {
		if !seen[it.ID] {
			report.MissingItems++
		}
	}
	report.Elapsed = time.Since(start)

	chain := client.Config().Roles[llm.RoleClassifier]
	report.Provider = chain.Primary.Provider
	report.Model = chain.Primary.Model

	return report, nil
}
