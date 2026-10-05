# Interstellar — Engineering Review

## Overall assessment

The classification pipeline in `internal/pipeline/classify.go` is thoughtfully engineered for an LLM-backed production system.

Strong areas:
- Topic × batch parallelization with bounded concurrency.
- Defensive LLM output parsing and one-shot repair.
- Verdict whitelisting and duplicate suppression.
- Explicit distinction between successful work, dropped verdicts, repairs, and watermark coverage.
- Conservative watermark semantics that avoid silently skipping unclassified items.

The biggest operational weakness is the interaction between the shared LLM call cap and the current all-or-nothing watermark commit.

## Classification architecture

```text
Active topics
    ↓
Items after classify watermark
    ↓
Window (max 2,000 items)
    ↓
Split into batches
    ↓
Topic × batch work queue
    ↓
Bounded worker pool
    ↓
LLM classification
    ↓
Parse / repair / validate / dedupe
    ↓
Save matches
    ↓
Compute committed prefix
    ↓
Commit watermark + stats
```

## Main issue: LLM cap + watermark

When the shared LLM call cap is reached:

```text
Batch 1 ✓
Batch 2 ✓
...
Batch N ✓
Batch N+1 → ErrCapReached
             ↓
Classify returns error
             ↓
watermark is NOT committed
```

Successful matches may already have been saved, but the watermark remains at its previous value.

On the next run, those successfully classified items are sent to the LLM again.

Example:

```text
Items 1–1500 → successfully classified
Item 1501     → call cap reached

Watermark remains before item 1

Next run:
Items 1–1500 are classified again
```

This is safe from silent data loss, but it wastes LLM calls and money.

## Recommended fix: per-range checkpointing

Commit progress for each successfully completed range.

Example:

```text
Range 1: all topics ✓
Range 2: all topics ✓
Range 3: all topics ✓
Range 4: one topic hits cap ✗
Range 5: cannot be committed
```

The watermark should advance to the end of Range 3.

Next run starts at Range 4.

### Critical rule

Only advance through a **contiguous prefix** where every active topic has successfully classified the range. Do not simply set the watermark to where the LLM limit was reached.

### Transaction requirement

Saving matches and advancing the watermark should be atomic for each committed range:

```text
BEGIN
  save matches
  update stats
  update watermark
COMMIT
```

## Should `classifyWindowLimit` remain 2,000?

### Preferred

Keep:

```text
classifyWindowLimit = 2000
```

and implement per-range checkpointing.

The window is a safety boundary; it does not need to be small enough to guarantee that every run fits under the LLM call cap.

### Current practical problem

With:
- 7 topics
- batch size = 30
- 2,000-item window

the theoretical classifier calls are:

```text
ceil(2000 / 30) × 7 = 67 × 7 = 469 calls
```

before repair calls or other LLM calls in the same run.

The shared default cap of 400 therefore means a full 2,000-item classification window can exceed the budget.

If checkpointing is not implemented yet, reducing the window to roughly 1,200–1,500 items is a pragmatic mitigation. It is not a substitute for checkpointing.

## Watermark model improvement

A timestamp-only watermark has a boundary problem if multiple items have exactly the same timestamp.

Prefer a compound cursor:

```text
(fetched_at, item_id)
```

with deterministic ordering:

```sql
WHERE
    fetched_at > ?
    OR (fetched_at = ? AND id > ?)
ORDER BY fetched_at, id
```

This avoids accidentally skipping items sharing a timestamp.

## Adding topics incrementally

If Day 1 has Topics A and B and the watermark advances, then Topics C and D added on Day 2 only classify items after the current global watermark.

They do not automatically classify historical items already behind the watermark.

If historical coverage is desired for a new topic, add a topic-specific backfill mechanism.

## Synthesis and LLM cap

Synthesis is lower volume. With the seeded topics it is roughly one call per topic, about 7 normal calls, plus possible repair calls.

If synthesis hits the LLM cap:
- synthesis returns an error;
- the pipeline stops;
- later stages do not run;
- there is no equivalent range-based synthesis watermark.

This is lower priority than classification checkpointing because synthesis produces far fewer calls.

## UI monitoring recommendation

Expose classifier progress in the existing status/quality UI:

```text
Classifier
────────────────────────
Status:       Healthy / Behind / Stopped
Watermark:    2026-10-01 10:42
Pending:      237 items

Last pass:
Items:        1,200
Batches:      40
Matches:      186
Repairs:      2
Dropped:      3

LLM calls:
280 / 400
```

When the cap is hit:

```text
Status:       Stopped — LLM limit reached
Watermark:    2026-10-01 10:42
Pending:      1,532 items
LLM calls:    400 / 400
```

Most useful indicators:
- watermark;
- pending items;
- last successful range;
- LLM calls used / cap;
- last error.

## Model recommendation

For OpenRouter, a good price/quality starting configuration is:

```text
Classifier    → DeepSeek V4 Flash
Extractor     → DeepSeek V4 Flash
Synthesis     → GPT-5 Mini
Ask           → GPT-5 Mini
Repair        → same model as the original request
```

Classification produces hundreds of calls and is relatively simple, so a cheap capable model is appropriate. Extraction is mostly structured output. Synthesis and Ask are user-facing and quality-sensitive but low volume, so a stronger model is worth the spend.

Exact OpenRouter pricing and model availability can change, so treat model names/prices as configuration rather than hard-coded assumptions.

## Recommended implementation priority

### P0 — Fix classification checkpointing
Commit the watermark after each fully successful contiguous range.

### P0 — Make match persistence + watermark update transactional
Avoid partial database state.

### P1 — Change watermark to `(FetchedAt, ItemID)`
Avoid timestamp-boundary data loss.

### P1 — Add classifier status to UI
Show watermark, pending items, call usage, and last failure.

### P2 — Add topic-specific historical backfill
Useful when users add topics after historical articles have already been processed.

### P3 — Add synthesis checkpointing
Only if synthesis becomes a meaningful source of wasted calls.

## Bottom line

The existing classifier is structurally strong. The main issue is not the 2,000-item window itself.

The key improvement is:

```text
Current:

LLM cap
   ↓
watermark unchanged
   ↓
previously completed work gets classified again


Recommended:

LLM cap
   ↓
commit successful contiguous ranges
   ↓
watermark advances to last safe range
   ↓
next run resumes exactly where it stopped
```

This gives Interstellar both safety and efficient LLM usage.
