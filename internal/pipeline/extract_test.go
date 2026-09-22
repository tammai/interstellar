package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

func extractCfg() config.Runtime {
	return config.Runtime{ExtractEnabled: true, ExtractBatchSize: 30}
}

// seedWatchlist imports a small watchlist and returns name -> id.
func seedWatchlist(t *testing.T, db *store.DB, rows ...store.EntityImport) map[string]int64 {
	t.Helper()
	res, err := db.ImportWatchlist(rows)
	if err != nil {
		t.Fatalf("ImportWatchlist: %v", err)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", res.Conflicts)
	}
	all, err := db.Entities()
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	out := map[string]int64{}
	for _, e := range all {
		out[e.Name] = e.ID
	}
	return out
}

// claimsFor replies with one entry per item, attaching the given claims to the
// first item and nothing to the rest.
func claimsFor(claims string) func(string) string {
	return func(prompt string) string {
		ids := itemIDsIn(prompt)
		var parts []string
		for i, id := range ids {
			body := "[]"
			if i == 0 {
				body = claims
			}
			parts = append(parts, fmt.Sprintf(`{"item_id":%d,"claims":%s}`, id, body))
		}
		return `{"items":[` + strings.Join(parts, ",") + `]}`
	}
}

func TestExtractWritesClaimsAndAdvancesWatermark(t *testing.T) {
	db := testDB(t)
	ids := seedWatchlist(t, db,
		store.EntityImport{Name: "Cursor", Aliases: []string{"Cursor IDE"},
			Layer: "coding_agents", FirstPartyURL: "https://cursor.com"},
	)

	src := seedSource(t, db, "a feed")
	seedItem(t, db, src, "https://techcrunch.test/cursor-3", "Cursor 3.0 ships")

	srv := newFakeModel(t, claimsFor(
		`[{"entity":"Cursor IDE","change_type":"launch","summary":"Cursor 3.0 is out.","confidence":0.9}]`))
	client := clientFor(t, db, srv)

	res, err := Extract(context.Background(), db, client, extractCfg(), quietLogger())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.ClaimsWritten != 1 {
		t.Fatalf("claims = %d, want 1", res.ClaimsWritten)
	}
	if res.Mentions != 1 {
		t.Errorf("mentions = %d, want 1", res.Mentions)
	}

	claims, err := db.ClaimsSince("", 10)
	if err != nil {
		t.Fatalf("ClaimsSince: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("stored claims = %d, want 1", len(claims))
	}
	c := claims[0]
	// The alias resolved to the entity: the model never saw an id.
	if c.EntityID != ids["Cursor"] {
		t.Errorf("entity = %d, want Cursor (%d)", c.EntityID, ids["Cursor"])
	}
	if c.ChangeType != "launch" {
		t.Errorf("change_type = %q, want launch", c.ChangeType)
	}
	// techcrunch.test is not cursor.com, so this is not the vendor speaking.
	if c.SourceType != "independent" {
		t.Errorf("source_type = %q, want independent", c.SourceType)
	}

	// A mention moves last_seen_at; an independent one must NOT move
	// last_verified_at, or a staleness job measures press attention.
	e, err := db.EntityByID(ids["Cursor"])
	if err != nil {
		t.Fatalf("EntityByID: %v", err)
	}
	if e.LastSeenAt == "" {
		t.Error("last_seen_at not set")
	}
	if e.LastVerifiedAt != "" {
		t.Errorf("last_verified_at = %q, want empty for an independent source", e.LastVerifiedAt)
	}

	mark, _, err := db.Setting(store.KeyExtractWatermark)
	if err != nil {
		t.Fatalf("Setting: %v", err)
	}
	if mark == "" {
		t.Error("watermark did not advance after a clean pass")
	}
}

// A claim from the vendor's own domain is first_party, and only that advances
// last_verified_at.
func TestExtractFirstPartyVerifies(t *testing.T) {
	db := testDB(t)
	ids := seedWatchlist(t, db,
		store.EntityImport{Name: "Cursor", Layer: "coding_agents",
			FirstPartyURL: "https://cursor.com"},
	)

	src := seedSource(t, db, "cursor changelog")
	// A subdomain of the vendor is still the vendor: changelogs live at
	// changelog. and docs. constantly.
	seedItem(t, db, src, "https://changelog.cursor.com/3-0", "Cursor 3.0")

	srv := newFakeModel(t, claimsFor(
		`[{"entity":"Cursor","change_type":"launch","summary":"3.0 is out.","confidence":1}]`))

	res, err := Extract(context.Background(), db, clientFor(t, db, srv), extractCfg(), quietLogger())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.ClaimsWritten != 1 {
		t.Fatalf("claims = %d, want 1", res.ClaimsWritten)
	}

	claims, _ := db.ClaimsSince("", 10)
	if claims[0].SourceType != "first_party" {
		t.Errorf("source_type = %q, want first_party", claims[0].SourceType)
	}
	e, _ := db.EntityByID(ids["Cursor"])
	if e.LastVerifiedAt == "" {
		t.Error("last_verified_at not advanced by a first-party claim")
	}
}

// change_type "none" is a sighting, not a claim: it moves last_seen_at and
// writes no row. Storing them would be one row per mention per run and would
// make `claims` mean two things.
func TestExtractNoneIsSightingNotClaim(t *testing.T) {
	db := testDB(t)
	ids := seedWatchlist(t, db,
		store.EntityImport{Name: "Cursor", Layer: "coding_agents"},
	)

	src := seedSource(t, db, "a feed")
	seedItem(t, db, src, "https://blog.test/opinion", "Why I switched editors")

	srv := newFakeModel(t, claimsFor(
		`[{"entity":"Cursor","change_type":"none","summary":"","confidence":0.4}]`))

	res, err := Extract(context.Background(), db, clientFor(t, db, srv), extractCfg(), quietLogger())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.ClaimsWritten != 0 {
		t.Errorf("claims = %d, want 0", res.ClaimsWritten)
	}
	if res.Mentions != 1 {
		t.Errorf("mentions = %d, want 1", res.Mentions)
	}
	e, _ := db.EntityByID(ids["Cursor"])
	if e.LastSeenAt == "" {
		t.Error("a 'none' mention must still advance last_seen_at")
	}
}

// An entity the watchlist does not know is counted, not stored. This is the
// candidate table's input; until that exists, the count is what says the
// watchlist has drifted behind the field.
func TestExtractCountsUnresolvedEntities(t *testing.T) {
	db := testDB(t)
	seedWatchlist(t, db, store.EntityImport{Name: "Cursor", Layer: "coding_agents"})

	src := seedSource(t, db, "a feed")
	seedItem(t, db, src, "https://blog.test/new", "Brand New Thing launches")

	srv := newFakeModel(t, claimsFor(
		`[{"entity":"Brand New Thing","change_type":"launch","summary":"It exists.","confidence":0.8}]`))

	res, err := Extract(context.Background(), db, clientFor(t, db, srv), extractCfg(), quietLogger())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Unresolved != 1 {
		t.Errorf("unresolved = %d, want 1", res.Unresolved)
	}
	if res.ClaimsWritten != 0 {
		t.Errorf("claims = %d, want 0", res.ClaimsWritten)
	}
	if len(res.UnresolvedNames) != 1 || res.UnresolvedNames[0] != "Brand New Thing" {
		t.Errorf("unresolved names = %v, want [Brand New Thing]", res.UnresolvedNames)
	}

	// Recorded as a candidate, not dropped: in a field this young the highest-value
	// signal is routinely the thing not on the watchlist yet.
	cands, err := db.PromotableCandidates(14, 1, 3)
	if err != nil {
		t.Fatalf("PromotableCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Name != "Brand New Thing" {
		t.Fatalf("candidates = %+v, want one named Brand New Thing", cands)
	}
	if cands[0].Sources != 1 {
		t.Errorf("sources = %d, want 1", cands[0].Sources)
	}
	if len(cands[0].Samples) != 1 {
		t.Errorf("samples = %v, want the item title that mentioned it", cands[0].Samples)
	}
}

// Re-running over the same items must not duplicate claims or restamp them.
// The watermark normally prevents a second pass, so this pins the guarantee the
// UNIQUE constraint gives when the watermark stalls.
func TestExtractIsIdempotentOverSameItems(t *testing.T) {
	db := testDB(t)
	seedWatchlist(t, db, store.EntityImport{Name: "Cursor", Layer: "coding_agents"})

	src := seedSource(t, db, "a feed")
	seedItem(t, db, src, "https://blog.test/cursor", "Cursor ships 3.0")

	srv := newFakeModel(t, claimsFor(
		`[{"entity":"Cursor","change_type":"launch","summary":"3.0.","confidence":0.9}]`))

	cfg := extractCfg()
	if _, err := Extract(context.Background(), db, clientFor(t, db, srv), cfg, quietLogger()); err != nil {
		t.Fatalf("first Extract: %v", err)
	}
	// Watermark deliberately left where it was, simulating a stalled pass.
	if _, err := Extract(context.Background(), db, clientFor(t, db, srv), cfg, quietLogger()); err != nil {
		t.Fatalf("second Extract: %v", err)
	}

	claims, _ := db.ClaimsSince("", 10)
	if len(claims) != 1 {
		t.Errorf("claims after two passes = %d, want 1", len(claims))
	}
}

// An unparseable batch must not advance the watermark past itself, or those
// items are skipped forever.
func TestExtractStallsWatermarkOnUnparseableBatch(t *testing.T) {
	db := testDB(t)
	seedWatchlist(t, db, store.EntityImport{Name: "Cursor", Layer: "coding_agents"})

	src := seedSource(t, db, "a feed")
	seedItem(t, db, src, "https://blog.test/one", "Cursor ships")

	srv := newFakeModel(t, func(string) string { return "not json at all" })

	res, err := Extract(context.Background(), db, clientFor(t, db, srv), extractCfg(), quietLogger())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.BatchesCommitted != 0 {
		t.Errorf("committed = %d, want 0", res.BatchesCommitted)
	}
	mark, _, _ := db.Setting(store.KeyExtractWatermark)
	if mark != "" {
		t.Errorf("watermark = %q, want unchanged after a failed batch", mark)
	}
	if len(res.Problems) == 0 {
		t.Error("a failed batch should be reported as a problem")
	}
}

// Extraction with no watchlist is a no-op, not an error: a fresh instance must
// not report a problem every morning for a feature it has not set up.
func TestExtractWithoutWatchlistDoesNothing(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "a feed")
	seedItem(t, db, src, "https://blog.test/one", "Something happened")

	srv := newFakeModel(t, func(string) string {
		t.Error("model called with no watchlist")
		return "{}"
	})

	res, err := Extract(context.Background(), db, clientFor(t, db, srv), extractCfg(), quietLogger())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Batches != 0 || srv.callCount() != 0 {
		t.Errorf("batches = %d, calls = %d, want 0 and 0", res.Batches, srv.callCount())
	}
}

// extract_enabled: 0 switches the stage off entirely, like transcripts_per_run.
func TestExtractDisabled(t *testing.T) {
	db := testDB(t)
	seedWatchlist(t, db, store.EntityImport{Name: "Cursor", Layer: "coding_agents"})
	src := seedSource(t, db, "a feed")
	seedItem(t, db, src, "https://blog.test/one", "Cursor ships")

	srv := newFakeModel(t, func(string) string {
		t.Error("model called while extraction is disabled")
		return "{}"
	})

	cfg := extractCfg()
	cfg.ExtractEnabled = false
	res, err := Extract(context.Background(), db, clientFor(t, db, srv), cfg, quietLogger())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Batches != 0 || srv.callCount() != 0 {
		t.Error("disabled extraction still did work")
	}
}

// The prompt must ask for an OBJECT wrapping the array. Asking for a top-level
// array under response_format {"type":"json_object"} does not fail loudly --
// several providers return one element and silently drop the rest of the batch,
// which cost seven days of empty skies when the classifier did it.
func TestExtractPromptIsObjectShaped(t *testing.T) {
	if !strings.Contains(extractSystemPrompt, `{"items":`) {
		t.Error("extract prompt does not ask for an object wrapping the array")
	}
	var probe any
	if err := json.Unmarshal([]byte(`{"items":[{"item_id":1,"claims":[]}]}`), &probe); err != nil {
		t.Fatalf("documented shape is not valid JSON: %v", err)
	}
}

func TestClaimHashIgnoresSummary(t *testing.T) {
	// The hash exists for "unseen in 14 days". Hashing the summary would let a
	// reworded summary defeat the check it exists to serve.
	a := claimHash(1, "launch", "https://x.test/a")
	b := claimHash(1, "launch", "https://x.test/a")
	if a != b {
		t.Error("same entity, type and url produced different hashes")
	}
	if a == claimHash(1, "pricing", "https://x.test/a") {
		t.Error("change type does not affect the hash")
	}
	if a == claimHash(2, "launch", "https://x.test/a") {
		t.Error("entity does not affect the hash")
	}
}

func TestClassifySourceKinds(t *testing.T) {
	entity := store.Entity{FirstPartyURL: "https://cursor.com"}
	vendors := map[string]bool{"cursor.com": true, "anthropic.com": true}

	cases := []struct {
		url, kind, want string
	}{
		{"https://cursor.com/blog/3-0", "rss", "first_party"},
		{"https://changelog.cursor.com/3-0", "rss", "first_party"},
		{"https://www.cursor.com/x", "rss", "first_party"},
		{"https://anthropic.com/news/x", "rss", "vendor_blog"},
		{"https://news.ycombinator.com/item?id=1", "hn_search", "aggregator"},
		{"https://techcrunch.test/x", "rss", "independent"},
	}
	for _, c := range cases {
		item := store.ExtractableItem{CanonicalURL: c.url, SourceKind: c.kind}
		if got := classifySource(item, entity, vendors); got != c.want {
			t.Errorf("classifySource(%q, %q) = %q, want %q", c.url, c.kind, got, c.want)
		}
	}
}

// Extraction runs on the classifier's CHAIN but is logged under its own role.
//
// Pinned because getting it wrong is silent in the worst way: Complete() with a
// role that has no chain fails before it reserves a call, so there is no
// llm_calls row to notice, no failing-role banner, and the only symptom is a
// Problems entry and a world model that never fills. The cost-log name is the
// other half -- extraction and classification now read the same items every run,
// and telling their spend apart is the whole evidence base for deciding later
// whether one can replace the other.
func TestExtractIsLoggedUnderItsOwnRole(t *testing.T) {
	db := testDB(t)
	seedWatchlist(t, db, store.EntityImport{Name: "Cursor", Layer: "coding_agents"})
	src := seedSource(t, db, "a feed")
	seedItem(t, db, src, "https://blog.test/cursor", "Cursor ships 3.0")

	srv := newFakeModel(t, claimsFor(
		`[{"entity":"Cursor","change_type":"launch","summary":"3.0.","confidence":0.9}]`))

	res, err := Extract(context.Background(), db, clientFor(t, db, srv), extractCfg(), quietLogger())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Problems) != 0 {
		t.Fatalf("problems = %v, want none", res.Problems)
	}

	runDate := store.RunDate(time.Now())
	n, err := db.LLMCallsByRoleOn(runDate, string(llm.RoleExtract))
	if err != nil {
		t.Fatalf("LLMCallsByRoleOn: %v", err)
	}
	if n != 1 {
		t.Errorf("calls logged as %q = %d, want 1", llm.RoleExtract, n)
	}
	// It must NOT be billed to the classifier: that would hide extraction's cost
	// inside the stage it may one day replace.
	if n, _ := db.LLMCallsByRoleOn(runDate, string(llm.RoleClassifier)); n != 0 {
		t.Errorf("classifier calls = %d, want 0", n)
	}
}
