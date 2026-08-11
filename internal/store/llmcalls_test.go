package store

import "testing"

func TestRecordAndCountLLMCalls(t *testing.T) {
	db := openTemp(t)

	calls := []LLMCall{
		{RunDate: "2026-08-11", Role: "classifier", Provider: "deepseek", Model: "deepseek-chat", TokensIn: 100, TokensOut: 20, OK: true},
		{RunDate: "2026-08-11", Role: "classifier", Provider: "deepseek", Model: "deepseek-chat", TokensIn: 90, TokensOut: 18, OK: true},
		{RunDate: "2026-08-11", Role: "classifier", Provider: "google", Model: "flash", OK: false, Err: "429 rate limited"},
		{RunDate: "2026-08-10", Role: "synthesizer", Provider: "deepseek", Model: "deepseek-chat", TokensIn: 500, TokensOut: 200, OK: true},
	}
	for i, c := range calls {
		if err := db.RecordLLMCall(c); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	today, err := db.LLMCallsOn("2026-08-11")
	if err != nil {
		t.Fatalf("LLMCallsOn: %v", err)
	}
	if today != 3 {
		t.Errorf("calls today = %d, want 3 (failures count toward the cap)", today)
	}

	if n, _ := db.LLMCallsOn("2026-08-09"); n != 0 {
		t.Errorf("calls on an untouched day = %d, want 0", n)
	}

	usage, err := db.LLMUsageOn("2026-08-11")
	if err != nil {
		t.Fatalf("LLMUsageOn: %v", err)
	}

	// Grouped by provider, so a fallback firing shows as its own line rather
	// than blending into the primary's numbers.
	byProvider := map[string]LLMUsage{}
	for _, u := range usage {
		byProvider[u.Provider] = u
	}
	if len(byProvider) != 2 {
		t.Fatalf("got %d provider groups, want 2: %+v", len(byProvider), usage)
	}
	if got := byProvider["deepseek"]; got.Calls != 2 || got.TokensIn != 190 || got.Failed != 0 {
		t.Errorf("deepseek usage = %+v", got)
	}
	if got := byProvider["google"]; got.Calls != 1 || got.Failed != 1 {
		t.Errorf("google usage = %+v; the failed fallback attempt must be visible", got)
	}
}
