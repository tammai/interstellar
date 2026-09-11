package pipeline

import (
	"slices"
	"strings"
	"testing"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// The classifier and synthesizer both run with JSONMode, whose response_format
// is {"type":"json_object"} -- a top-level OBJECT. A prompt asking for a
// top-level array under it does not error; the model answers with a single
// element and drops the rest of the batch, which is silent data loss and cost
// seven days of empty skies. These pin the prompts to the object shape.
func TestPromptsAskForObjectNotTopLevelArray(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, key string
	}{
		{"classifier", classifySystemPrompt, `"verdicts"`},
		{"synthesizer", synthesizeSystemPrompt, `"stories"`},
	} {
		if strings.Contains(tc.prompt, "ONLY a JSON array") {
			t.Errorf("%s: prompt asks for a top-level JSON array under JSONMode; "+
				"the model returns one element and silently drops the batch", tc.name)
		}
		if !strings.Contains(tc.prompt, "ONLY a JSON object") {
			t.Errorf("%s: prompt should ask for ONLY a JSON object", tc.name)
		}
		if !strings.Contains(tc.prompt, tc.key) {
			t.Errorf("%s: prompt should name the %s array key", tc.name, tc.key)
		}
	}
}

// The parse side must accept the object-wrapped shape the prompts now ask for --
// both models return exactly this.
func TestParseAcceptsObjectWrappedArray(t *testing.T) {
	verdicts, err := parseVerdicts(
		`{"verdicts":[{"item_id":801,"score":5,"reason":"x"},{"item_id":802,"score":2}]}`)
	if err != nil {
		t.Fatalf("parseVerdicts: %v", err)
	}
	if len(verdicts) != 2 {
		t.Errorf("got %d verdicts, want 2", len(verdicts))
	}

	signals, err := parseComposed(
		`{"stories":[{"title":"T","reason":"R","item_ids":[801,802]}]}`)
	if err != nil {
		t.Fatalf("parseComposed: %v", err)
	}
	if len(signals) != 1 || len(signals[0].ItemIDs) != 2 {
		t.Errorf("got %+v, want one story over two items", signals)
	}
}

// A bare array must keep parsing: a provider that ignores response_format still
// answers the old way, and the repair path leans on this tolerance.
func TestParseStillAcceptsBareArray(t *testing.T) {
	if _, err := llm.ExtractArray(`[{"item_id":1,"score":4}]`); err != nil {
		t.Fatalf("bare array should still parse: %v", err)
	}
}

// keepAnswers is what stops `items_out` exceeding `items_in`, and the reason that
// matters is not tidiness. Completeness (items_answered / items_asked, grouped by
// provider/model/role) exists to catch a model silently answering fewer items than
// it was asked about. If an over-answer can push a batch above 1.0, it cancels out
// another batch that truncated inside the same group, and the metric stops
// detecting the thing it was built for. Observed in production before this:
// 3427 answered against 3423 asked.
//
// The second cost was the hit rate. A repeated id was appended twice, so
// MatchesKept counted an item twice while the matches table -- PRIMARY KEY
// (item_id, topic_id), upserted -- stored it once. That inflated the numerator of
// the diagnostic used to decide whether a topic definition is drawn too tight.
func TestKeepAnswersRejectsHallucinatedAndRepeatedIDs(t *testing.T) {
	batch := []store.ClassifiableItem{{ID: 1}, {ID: 2}, {ID: 3}}

	for _, tc := range []struct {
		name        string
		verdicts    []verdict
		wantKept    []int64
		wantDropped int
	}{
		{
			name:     "clean reply is untouched",
			verdicts: []verdict{{ItemID: 1, Score: 5}, {ItemID: 2, Score: 2}},
			wantKept: []int64{1, 2}, wantDropped: 0,
		},
		{
			name:     "an id that was not in the batch is dropped",
			verdicts: []verdict{{ItemID: 1, Score: 5}, {ItemID: 99, Score: 5}},
			wantKept: []int64{1}, wantDropped: 1,
		},
		{
			name:     "an id answered twice counts once",
			verdicts: []verdict{{ItemID: 1, Score: 5}, {ItemID: 1, Score: 5}},
			wantKept: []int64{1}, wantDropped: 1,
		},
		{
			name: "both faults at once, and the invariant still holds",
			verdicts: []verdict{
				{ItemID: 3, Score: 4}, {ItemID: 3, Score: 4},
				{ItemID: 99, Score: 4}, {ItemID: 1, Score: 4},
			},
			wantKept: []int64{3, 1}, wantDropped: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, dropped := keepAnswers(batch, tc.verdicts)
			if dropped != tc.wantDropped {
				t.Errorf("dropped = %d, want %d", dropped, tc.wantDropped)
			}
			var got []int64
			for _, v := range kept {
				got = append(got, v.ItemID)
			}
			if !slices.Equal(got, tc.wantKept) {
				t.Errorf("kept = %v, want %v", got, tc.wantKept)
			}
			// The invariant the completeness metric rests on.
			if len(kept) > len(batch) {
				t.Errorf("kept %d answers to a %d-item batch: completeness could exceed 1.0",
					len(kept), len(batch))
			}
		})
	}
}

// First verdict for an id wins. A model repeating itself with two different
// scores is not offering a considered second opinion, and taking the later one
// would make the stored score depend on reply ordering.
func TestKeepAnswersKeepsTheFirstVerdictForARepeatedID(t *testing.T) {
	batch := []store.ClassifiableItem{{ID: 7}}
	kept, dropped := keepAnswers(batch, []verdict{
		{ItemID: 7, Score: 5, Reason: "first"},
		{ItemID: 7, Score: 1, Reason: "second"},
	})
	if len(kept) != 1 || dropped != 1 {
		t.Fatalf("kept %d dropped %d, want 1 and 1", len(kept), dropped)
	}
	if kept[0].Reason != "first" || kept[0].Score != 5 {
		t.Errorf("kept %+v, want the first verdict", kept[0])
	}
}

// An uncovered matched item is the most expensive kind of silent loss in the
// pipeline: it was fetched, classified, scored >= 4 and paid for, then left
// without reaching the sky. The synthesizer prompt's rule is that every item_id
// given comes back in exactly one story, so a gap is the model breaking that
// rule -- or the write loop discarding a story and taking its items along.
//
// storedCoverage measures the second case too, which is why it reads what was
// written rather than what was replied.
func TestStoredCoverageCountsWhatTheSkyActuallyGot(t *testing.T) {
	matched := []store.MatchedItem{{ItemID: 1}, {ItemID: 2}, {ItemID: 3}, {ItemID: 4}}

	for _, tc := range []struct {
		name    string
		written []store.NewSignal
		want    int
	}{
		{
			name:    "every item placed",
			written: []store.NewSignal{{ItemIDs: []int64{1, 2}}, {ItemIDs: []int64{3, 4}}},
			want:    4,
		},
		{
			name:    "the model omitted two",
			written: []store.NewSignal{{ItemIDs: []int64{1, 2}}},
			want:    2,
		},
		{
			name: "an invented id does not count as coverage",
			// Counting it would let a reply that drops half the batch and makes up
			// the difference look complete -- the same reasoning as itemsCovered.
			written: []store.NewSignal{{ItemIDs: []int64{1, 999}}},
			want:    1,
		},
		{
			name:    "one item in two stories is still one item covered",
			written: []store.NewSignal{{ItemIDs: []int64{1}}, {ItemIDs: []int64{1}}},
			want:    1,
		},
		{
			name:    "nothing written loses everything",
			written: nil,
			want:    0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := storedCoverage(matched, tc.written)
			if got != tc.want {
				t.Errorf("storedCoverage = %d, want %d (uncovered would read %d)",
					got, tc.want, len(matched)-got)
			}
			if got > len(matched) {
				t.Errorf("coverage %d exceeds the %d items offered", got, len(matched))
			}
		})
	}
}
