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

// The failure this exists to catch: a keyless provider whose entitlement lapsed
// answers 403 to everything, readiness reports it ready because there is no key
// to be missing, and the only symptom is an empty sky.
func TestFailingRolesReportsAWhollyBrokenRole(t *testing.T) {
	db := openTemp(t)

	for i := 0; i < 63; i++ {
		must(t, db.RecordLLMCall(LLMCall{
			RunDate: "2026-08-17", Role: "classifier", Provider: "ollama",
			Model: "deepseek-v4-flash:cloud", OK: false,
			Err: "ollama returned 403: this model requires a subscription",
		}))
	}

	got, err := db.FailingRoles("2026-08-11")
	if err != nil {
		t.Fatalf("FailingRoles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d failing roles, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Role != "classifier" || f.Failed != 63 || f.RunDate != "2026-08-17" {
		t.Errorf("got %+v, want the classifier with 63 failures on 2026-08-17", f)
	}
	// The message has to name what to change, not only what broke.
	if f.Ref != "ollama/deepseek-v4-flash:cloud" {
		t.Errorf("ref = %q, want the provider/model that failed", f.Ref)
	}
	if f.Error == "" {
		t.Error("no error text carried, so the banner cannot say what the provider said")
	}
}

// A fallback chain doing its job is failures followed by a success. That is the
// chain working, it is already in the cost log, and a banner for it would cry
// wolf on every transient 500.
func TestFailingRolesIgnoresARescuedRun(t *testing.T) {
	db := openTemp(t)

	must(t, db.RecordLLMCall(LLMCall{RunDate: "2026-08-17", Role: "synthesizer", Provider: "down", Model: "a", OK: false, Err: "500"}))
	must(t, db.RecordLLMCall(LLMCall{RunDate: "2026-08-17", Role: "synthesizer", Provider: "up", Model: "b", OK: true}))

	got, err := db.FailingRoles("2026-08-11")
	if err != nil {
		t.Fatalf("FailingRoles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a rescued run was reported as a failure: %+v", got)
	}
}

// Only the most recent day counts. Yesterday's outage that has since been fixed
// is not something to keep interrupting about.
func TestFailingRolesReadsOnlyTheLatestDay(t *testing.T) {
	db := openTemp(t)

	must(t, db.RecordLLMCall(LLMCall{RunDate: "2026-08-16", Role: "classifier", Provider: "p", Model: "m", OK: false, Err: "403"}))
	must(t, db.RecordLLMCall(LLMCall{RunDate: "2026-08-17", Role: "classifier", Provider: "p", Model: "m", OK: true}))

	got, err := db.FailingRoles("2026-08-11")
	if err != nil {
		t.Fatalf("FailingRoles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a fixed role was still reported: %+v", got)
	}
}

// A failed "test" call is shown inline the moment the button is pressed. Raising
// a banner for it would nag about a diagnosis the owner just ran on purpose.
func TestFailingRolesIgnoresTestCalls(t *testing.T) {
	db := openTemp(t)

	must(t, db.RecordLLMCall(LLMCall{RunDate: "2026-08-18", Role: "test", Provider: "p", Model: "m", OK: false, Err: "403"}))

	got, err := db.FailingRoles("2026-08-11")
	if err != nil {
		t.Fatalf("FailingRoles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a deliberate test call was reported as a failure: %+v", got)
	}
}

// Outside the window, a long-abandoned provider stops nagging.
func TestFailingRolesRespectsTheWindow(t *testing.T) {
	db := openTemp(t)

	must(t, db.RecordLLMCall(LLMCall{RunDate: "2026-07-01", Role: "classifier", Provider: "p", Model: "m", OK: false, Err: "403"}))

	got, err := db.FailingRoles("2026-08-11")
	if err != nil {
		t.Fatalf("FailingRoles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a failure older than the window was reported: %+v", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("record: %v", err)
	}
}
