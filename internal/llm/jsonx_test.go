package llm

import "testing"

type verdict struct {
	ItemID int64   `json:"item_id"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// The §10 gate: real model output is only approximately JSON. Every row here is
// a shape a provider has been observed to emit.
func TestExtractArrayRecoversRealWorldOutput(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		count int
	}{
		{
			"clean array",
			`[{"item_id":1,"score":5,"reason":"direct hit"}]`,
			1,
		},
		{
			"json fence",
			"```json\n[{\"item_id\":1,\"score\":5,\"reason\":\"a\"}]\n```",
			1,
		},
		{
			"bare fence",
			"```\n[{\"item_id\":1,\"score\":4,\"reason\":\"a\"}]\n```",
			1,
		},
		{
			"unclosed fence",
			"```json\n[{\"item_id\":1,\"score\":4,\"reason\":\"a\"}]",
			1,
		},
		{
			"prose preamble",
			`Here are the verdicts you asked for:
			[{"item_id":1,"score":4,"reason":"a"},{"item_id":2,"score":5,"reason":"b"}]`,
			2,
		},
		{
			"prose on both sides",
			`Sure! [{"item_id":1,"score":4,"reason":"a"}] Let me know if you need more.`,
			1,
		},
		{
			"trailing comma in array",
			`[{"item_id":1,"score":4,"reason":"a"},]`,
			1,
		},
		{
			"trailing comma in object",
			`[{"item_id":1,"score":4,"reason":"a",}]`,
			1,
		},
		{
			// json_object mode cannot return a bare array on some providers, so
			// they wrap it.
			"wrapped in an object",
			`{"verdicts":[{"item_id":1,"score":4,"reason":"a"},{"item_id":2,"score":4,"reason":"b"}]}`,
			2,
		},
		{
			"wrapped and fenced",
			"```json\n{\"results\": [{\"item_id\":7,\"score\":4,\"reason\":\"a\"}]}\n```",
			1,
		},
		{
			"empty array is valid",
			`[]`,
			0,
		},
		{
			// A bracket inside a title breaks any naive first-[-to-last-] scan.
			"brackets inside strings",
			`[{"item_id":1,"score":4,"reason":"the paper [2026] argues x"}]`,
			1,
		},
		{
			"nested arrays inside elements",
			`[{"item_id":1,"score":4,"reason":"a","tags":["x","y"]}]`,
			1,
		},
		{
			"newlines and indentation",
			"[\n  {\n    \"item_id\": 1,\n    \"score\": 4,\n    \"reason\": \"a\"\n  }\n]",
			1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			elems, err := ExtractArray(tc.raw)
			if err != nil {
				t.Fatalf("ExtractArray: %v\ninput: %s", err, tc.raw)
			}
			if len(elems) != tc.count {
				t.Errorf("got %d elements, want %d", len(elems), tc.count)
			}
			decoded, dropped := DecodeElements[verdict](elems, nil)
			if dropped != 0 {
				t.Errorf("dropped %d valid elements", dropped)
			}
			if len(decoded) != tc.count {
				t.Errorf("decoded %d, want %d", len(decoded), tc.count)
			}
		})
	}
}

func TestExtractArrayRejectsUnrecoverable(t *testing.T) {
	for _, raw := range []string{
		"",
		"I'm sorry, I can't help with that.",
		"The answer is 42.",
		`{"item_id":1,"score":4}`, // a bare object, no array anywhere
	} {
		if _, err := ExtractArray(raw); err == nil {
			t.Errorf("ExtractArray(%q) succeeded, want error", raw)
		}
	}
}

// One bad element must cost one verdict, not the batch. Losing thirty
// classifications to a single malformed entry would make the pipeline
// pathologically expensive.
func TestDecodeElementsDropsOnlyBadElements(t *testing.T) {
	raw := `[
	  {"item_id":1,"score":5,"reason":"good"},
	  {"item_id":"not a number","score":4,"reason":"bad type"},
	  {"item_id":3,"score":4,"reason":"good"}
	]`

	elems, err := ExtractArray(raw)
	if err != nil {
		t.Fatalf("ExtractArray: %v", err)
	}
	decoded, dropped := DecodeElements[verdict](elems, nil)

	if len(decoded) != 2 {
		t.Errorf("kept %d elements, want 2", len(decoded))
	}
	if dropped != 1 {
		t.Errorf("dropped %d, want 1", dropped)
	}
	if decoded[0].ItemID != 1 || decoded[1].ItemID != 3 {
		t.Errorf("kept the wrong elements: %+v", decoded)
	}
}

func TestDecodeElementsAppliesValidation(t *testing.T) {
	raw := `[{"item_id":1,"score":5,"reason":"a"},{"item_id":0,"score":5,"reason":"no id"}]`
	elems, _ := ExtractArray(raw)

	decoded, dropped := DecodeElements(elems, func(v verdict) error {
		if v.ItemID == 0 {
			return errNoID
		}
		return nil
	})
	if len(decoded) != 1 || dropped != 1 {
		t.Errorf("validation not applied: kept %d dropped %d", len(decoded), dropped)
	}
}

var errNoID = errString("missing item_id")

type errString string

func (e errString) Error() string { return string(e) }

// A comma inside a string must survive; only structural trailing commas go.
func TestDropTrailingCommasRespectsStrings(t *testing.T) {
	in := `[{"reason":"a, b, c"},]`
	got := dropTrailingCommas(in)
	want := `[{"reason":"a, b, c"}]`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripFencesLeavesUnfencedTextAlone(t *testing.T) {
	in := `[{"item_id":1}]`
	if got := stripFences(in); got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}
