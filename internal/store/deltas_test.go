package store

import (
	"testing"
)

// seedClaim inserts a claim and returns its id.
func seedClaim(t *testing.T, db *DB, entityID, itemID int64, changeType, sourceType, url string, conf float64) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(
		`INSERT INTO claims (entity_id, item_id, change_type, source_type, summary, url, confidence, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		entityID, itemID, changeType, sourceType, "summary from "+sourceType, url, conf,
		changeType+url,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	return id
}

func seedEntity(t *testing.T, db *DB, name, layer string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(
		`INSERT INTO entities (name, name_norm, layer) VALUES (?, ?, ?) RETURNING id`,
		name, NormalizeEntityName(name), layer,
	).Scan(&id); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return id
}

// Two outlets covering one launch must produce ONE delta carrying both, not two
// notifications. This is the whole reason the window applies to a group rather
// than to claimHash, which includes the url and so differs per outlet.
func TestPendingDeltasGroupsAcrossSources(t *testing.T) {
	db := openTemp(t)

	entity := seedEntity(t, db, "Cursor", "coding_agents")
	srcA := addSource(t, db, "rss", "TechCrunch")
	srcB := addSource(t, db, "rss", "The Verge")
	itemA := addItem(t, db, srcA, "https://techcrunch.test/cursor")
	itemB := addItem(t, db, srcB, "https://verge.test/cursor")

	seedClaim(t, db, entity, itemA, "launch", "independent", "https://techcrunch.test/cursor", 0.7)
	seedClaim(t, db, entity, itemB, "launch", "independent", "https://verge.test/cursor", 0.8)

	deltas, err := db.PendingDeltas(14, 0)
	if err != nil {
		t.Fatalf("PendingDeltas: %v", err)
	}
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want 1 (one launch, two outlets)", len(deltas))
	}
	d := deltas[0]
	if d.Sources != 2 {
		t.Errorf("sources = %d, want 2", d.Sources)
	}
	if len(d.Claims) != 2 {
		t.Errorf("claim ids = %v, want 2", d.Claims)
	}
	if len(d.AllURLs) != 2 {
		t.Errorf("urls = %v, want 2", d.AllURLs)
	}
}

// Two different kinds of change about one entity are two deltas: a launch and a
// price rise on the same day are separate facts a reader acts on differently.
func TestPendingDeltasSeparatesChangeTypes(t *testing.T) {
	db := openTemp(t)

	entity := seedEntity(t, db, "Cursor", "coding_agents")
	src := addSource(t, db, "rss", "a feed")
	item := addItem(t, db, src, "https://x.test/one")

	seedClaim(t, db, entity, item, "launch", "independent", "https://x.test/one", 0.7)
	seedClaim(t, db, entity, item, "pricing", "independent", "https://x.test/one", 0.7)

	deltas, _ := db.PendingDeltas(14, 0)
	if len(deltas) != 2 {
		t.Fatalf("deltas = %d, want 2", len(deltas))
	}
}

// The most authoritative source writes the headline. A vendor announcing its own
// launch is the fact; an aggregator mentioning it is an echo.
func TestPendingDeltasPrefersAuthoritativeSummary(t *testing.T) {
	db := openTemp(t)

	entity := seedEntity(t, db, "Cursor", "coding_agents")
	srcA := addSource(t, db, "hn_search", "Hacker News")
	srcB := addSource(t, db, "rss", "Cursor changelog")
	itemA := addItem(t, db, srcA, "https://news.test/a")
	itemB := addItem(t, db, srcB, "https://cursor.com/blog")

	// Aggregator has the higher confidence, to prove rank beats it.
	seedClaim(t, db, entity, itemA, "launch", "aggregator", "https://news.test/a", 0.99)
	seedClaim(t, db, entity, itemB, "launch", "first_party", "https://cursor.com/blog", 0.5)

	deltas, _ := db.PendingDeltas(14, 0)
	if len(deltas) != 1 {
		t.Fatalf("deltas = %d, want 1", len(deltas))
	}
	if deltas[0].URL != "https://cursor.com/blog" {
		t.Errorf("headline url = %q, want the first-party one", deltas[0].URL)
	}
	if !deltas[0].Verified {
		t.Error("a first-party claim should mark the delta verified")
	}
}

// Once pushed, the same entity and change type stay quiet for the window -- even
// arriving from a new outlet, which is the case that would otherwise re-announce
// one launch all week as coverage trickles in.
func TestPendingDeltasSuppressesInsideWindow(t *testing.T) {
	db := openTemp(t)

	entity := seedEntity(t, db, "Cursor", "coding_agents")
	srcA := addSource(t, db, "rss", "first outlet")
	itemA := addItem(t, db, srcA, "https://a.test/x")
	first := seedClaim(t, db, entity, itemA, "launch", "independent", "https://a.test/x", 0.7)

	if err := db.MarkDeltasPushed([]int64{first}); err != nil {
		t.Fatalf("MarkDeltasPushed: %v", err)
	}

	srcB := addSource(t, db, "rss", "late outlet")
	itemB := addItem(t, db, srcB, "https://b.test/x")
	seedClaim(t, db, entity, itemB, "launch", "independent", "https://b.test/x", 0.7)

	deltas, _ := db.PendingDeltas(14, 0)
	if len(deltas) != 0 {
		t.Errorf("deltas = %d, want 0 inside the window", len(deltas))
	}

	// Age the push past the window: a change announced 20 days ago and happening
	// again is a second event, not an echo, and must be sent again. This is the
	// boundary the window exists to draw, and testing it with a zero-day window
	// would not -- a claim pushed this same second is still inside a window of
	// zero days, because the comparison is >= now.
	if _, err := db.Exec(
		`UPDATE claims SET pushed_at = datetime('now', '-20 days') WHERE id = ?`, first,
	); err != nil {
		t.Fatalf("age the push: %v", err)
	}
	deltas, _ = db.PendingDeltas(14, 0)
	if len(deltas) != 1 {
		t.Errorf("deltas after the window lapsed = %d, want 1", len(deltas))
	}
}

func TestMarkDeltasPushedIsScoped(t *testing.T) {
	db := openTemp(t)

	entity := seedEntity(t, db, "Cursor", "coding_agents")
	src := addSource(t, db, "rss", "a feed")
	item := addItem(t, db, src, "https://x.test/one")
	a := seedClaim(t, db, entity, item, "launch", "independent", "https://x.test/one", 0.7)
	seedClaim(t, db, entity, item, "pricing", "independent", "https://x.test/one", 0.7)

	if err := db.MarkDeltasPushed([]int64{a}); err != nil {
		t.Fatalf("MarkDeltasPushed: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM claims WHERE pushed_at IS NOT NULL"); n != 1 {
		t.Errorf("pushed = %d, want exactly the one claim passed in", n)
	}
}
