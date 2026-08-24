package pipeline

import (
	"strings"
	"testing"

	"github.com/tammai/interstellar/internal/llm"
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
