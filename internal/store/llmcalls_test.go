package store

import (
	"testing"
	"time"
)

func TestRecordAndCountLLMCalls(t *testing.T) {
	db := openTemp(t)

	calls := []LLMCall{
		{RunDate: "2026-08-11", Role: "classifier", Provider: "deepseek", Model: "deepseek-chat", TokensIn: 100, TokensOut: 20, OK: true},
		{RunDate: "2026-08-11", Role: "classifier", Provider: "deepseek", Model: "deepseek-chat", TokensIn: 90, TokensOut: 18, OK: true},
		{RunDate: "2026-08-11", Role: "classifier", Provider: "google", Model: "flash", OK: false, Err: "429 rate limited"},
		{RunDate: "2026-08-10", Role: "synthesizer", Provider: "deepseek", Model: "deepseek-chat", TokensIn: 500, TokensOut: 200, OK: true},
	}
	for i, c := range calls {
		if _, err := db.RecordLLMCall(c); err != nil {
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
		recordCall(t, db, LLMCall{
			RunDate: "2026-08-17", Role: "classifier", Provider: "ollama",
			Model: "deepseek-v4-flash:cloud", OK: false,
			Err: "ollama returned 403: this model requires a subscription",
		})
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

	recordCall(t, db, LLMCall{RunDate: "2026-08-17", Role: "synthesizer", Provider: "down", Model: "a", OK: false, Err: "500"})
	recordCall(t, db, LLMCall{RunDate: "2026-08-17", Role: "synthesizer", Provider: "up", Model: "b", OK: true})

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

	recordCall(t, db, LLMCall{RunDate: "2026-08-16", Role: "classifier", Provider: "p", Model: "m", OK: false, Err: "403"})
	recordCall(t, db, LLMCall{RunDate: "2026-08-17", Role: "classifier", Provider: "p", Model: "m", OK: true})

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

	recordCall(t, db, LLMCall{RunDate: "2026-08-18", Role: "test", Provider: "p", Model: "m", OK: false, Err: "403"})

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

	recordCall(t, db, LLMCall{RunDate: "2026-07-01", Role: "classifier", Provider: "p", Model: "m", OK: false, Err: "403"})

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

// recordCall is must() for the cost log, which now returns the new row's id --
// needed by the quality tests, ignored by the rest.
func recordCall(t *testing.T, db *DB, c LLMCall) int64 {
	t.Helper()
	id, err := db.RecordLLMCall(c)
	if err != nil {
		t.Fatalf("record llm call: %v", err)
	}
	return id
}

// The measurement this whole column set exists for: a model that returns valid
// JSON, a 200, and one verdict for a batch of thirty.
//
// Nothing in the cost log could see that before. ok is 1, the tokens look normal,
// and the only downstream symptom is topics that mysteriously match almost
// nothing -- which reads as definitions drawn too tight. Asked-versus-answered is
// what names the model instead.
func TestModelReliabilitySurfacesASilentlyTruncatedBatch(t *testing.T) {
	db := openTemp(t)
	today := RunDate(time.Now())

	id := recordCall(t, db, LLMCall{
		RunDate: today, Role: "classifier", Provider: "openrouter",
		Model: "deepseek-v4-flash", TokensIn: 900, TokensOut: 40, OK: true, ItemsIn: 30,
	})
	must(t, db.RecordReplyQuality(id, ReplyQuality{ItemsOut: 1, ParseOK: true}))

	rows, err := db.ModelReliabilityOver(7)
	if err != nil {
		t.Fatalf("ModelReliabilityOver: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]

	if got.ParseRate != 1 {
		t.Errorf("parse rate %v, want 1: the reply was valid JSON and must not be blamed for parsing", got.ParseRate)
	}
	if got.ItemsAsked != 30 || got.ItemsAnswered != 1 {
		t.Errorf("asked/answered = %d/%d, want 30/1", got.ItemsAsked, got.ItemsAnswered)
	}
	if got.Completeness > 0.05 {
		t.Errorf("completeness %v; 29 of 30 items were dropped and the number has to say so", got.Completeness)
	}
	if got.Ref() != "openrouter/deepseek-v4-flash" {
		t.Errorf("ref %q; provider and model are one identity here", got.Ref())
	}
}

// A transport failure asked for items and never got a reply. Counting its
// items_in would charge the model for incompleteness that was really a 500 -- and
// on a bad afternoon that is most of the denominator.
func TestModelReliabilityExcludesUnansweredAttemptsFromCompleteness(t *testing.T) {
	db := openTemp(t)
	today := RunDate(time.Now())

	// Attempt one: asked for 30, died in transport.
	recordCall(t, db, LLMCall{
		RunDate: today, Role: "classifier", Provider: "p", Model: "m",
		OK: false, Err: "500 upstream", ItemsIn: 30,
	})
	// Attempt two: same question, answered in full.
	id := recordCall(t, db, LLMCall{
		RunDate: today, Role: "classifier", Provider: "p", Model: "m", OK: true, ItemsIn: 30,
	})
	must(t, db.RecordReplyQuality(id, ReplyQuality{ItemsOut: 30, ParseOK: true}))

	rows, err := db.ModelReliabilityOver(7)
	if err != nil {
		t.Fatalf("ModelReliabilityOver: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]

	if got.Calls != 2 || got.Failed != 1 {
		t.Errorf("calls/failed = %d/%d, want 2/1", got.Calls, got.Failed)
	}
	if got.Replies != 1 {
		t.Errorf("replies %d, want 1: only one attempt produced something to assess", got.Replies)
	}
	if got.Completeness != 1 {
		t.Errorf("completeness %v, want 1; the reply that arrived was complete and the 500 must not dilute it",
			got.Completeness)
	}
}

// A repair is its own row, on whichever model answered it -- which may not be the
// model that mangled the first reply, since the repair walks the chain again.
func TestModelReliabilityAttributesParseFailureAndRepairSeparately(t *testing.T) {
	db := openTemp(t)
	today := RunDate(time.Now())

	bad := recordCall(t, db, LLMCall{
		RunDate: today, Role: "classifier", Provider: "cheap", Model: "a", OK: true, ItemsIn: 12,
	})
	must(t, db.RecordReplyQuality(bad, ReplyQuality{ItemsOut: 0, ParseOK: false}))

	fixed := recordCall(t, db, LLMCall{
		RunDate: today, Role: "classifier", Provider: "solid", Model: "b",
		OK: true, ItemsIn: 12, Repair: true,
	})
	must(t, db.RecordReplyQuality(fixed, ReplyQuality{ItemsOut: 12, ParseOK: true}))

	rows, err := db.ModelReliabilityOver(7)
	if err != nil {
		t.Fatalf("ModelReliabilityOver: %v", err)
	}
	byRef := map[string]ModelReliability{}
	for _, r := range rows {
		byRef[r.Ref()] = r
	}

	cheap, ok := byRef["cheap/a"]
	if !ok {
		t.Fatalf("no row for cheap/a; got %v", byRef)
	}
	if cheap.ParseFailed != 1 || cheap.ParseRate != 0 {
		t.Errorf("cheap/a parse_failed=%d rate=%v, want 1 and 0", cheap.ParseFailed, cheap.ParseRate)
	}
	if cheap.Repairs != 0 {
		t.Errorf("cheap/a repairs=%d; it CAUSED a repair, it did not answer one", cheap.Repairs)
	}

	solid, ok := byRef["solid/b"]
	if !ok {
		t.Fatalf("no row for solid/b; got %v", byRef)
	}
	if solid.Repairs != 1 {
		t.Errorf("solid/b repairs=%d, want 1", solid.Repairs)
	}
	if solid.Completeness != 1 {
		t.Errorf("solid/b completeness=%v, want 1", solid.Completeness)
	}
}

// A call with no record count is not a list of anything, and must not acquire a
// completeness rate by having its zero counted. The sentinel is what lets the
// screen print an em dash rather than a confident 0%.
func TestModelReliabilityLeavesUnmeasuredCallsWithoutARate(t *testing.T) {
	db := openTemp(t)
	today := RunDate(time.Now())

	recordCall(t, db, LLMCall{RunDate: today, Role: "chat", Provider: "p", Model: "m", OK: true})

	rows, err := db.ModelReliabilityOver(7)
	if err != nil {
		t.Fatalf("ModelReliabilityOver: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].ParseRate != -1 || rows[0].Completeness != -1 {
		t.Errorf("rates = %v/%v, want -1/-1 for a call nobody measured",
			rows[0].ParseRate, rows[0].Completeness)
	}
	if rows[0].Calls != 1 {
		t.Errorf("calls %d, want 1: the cost half is still recorded", rows[0].Calls)
	}
}
