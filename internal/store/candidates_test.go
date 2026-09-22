package store

import (
	"strings"
	"testing"
)

func sighting(name string, item, source int64) CandidateSighting {
	return CandidateSighting{Name: name, ItemID: item, SourceID: source}
}

// Promotion counts DISTINCT SOURCES, not mentions. One aggregator repeating
// itself is one outlet with an opinion; three outlets is the field noticing.
func TestPromotableCandidatesCountsDistinctSources(t *testing.T) {
	db := openTemp(t)

	loud := addSource(t, db, "hn_search", "Hacker News")
	a := addItem(t, db, loud, "https://hn.test/1")
	b := addItem(t, db, loud, "https://hn.test/2")
	c := addItem(t, db, loud, "https://hn.test/3")

	if err := db.RecordCandidates([]CandidateSighting{
		sighting("Loud Thing", a, loud),
		sighting("Loud Thing", b, loud),
		sighting("Loud Thing", c, loud),
	}); err != nil {
		t.Fatalf("RecordCandidates: %v", err)
	}

	got, err := db.PromotableCandidates(14, 3, 0)
	if err != nil {
		t.Fatalf("PromotableCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("three mentions from one source promoted %d; want 0", len(got))
	}

	// The same three mentions spread across three outlets do qualify.
	s2 := addSource(t, db, "rss", "The Verge")
	s3 := addSource(t, db, "rss", "TechCrunch")
	d := addItem(t, db, s2, "https://verge.test/1")
	e := addItem(t, db, s3, "https://tc.test/1")
	if err := db.RecordCandidates([]CandidateSighting{
		sighting("Loud Thing", d, s2),
		sighting("Loud Thing", e, s3),
	}); err != nil {
		t.Fatalf("RecordCandidates: %v", err)
	}

	got, _ = db.PromotableCandidates(14, 3, 2)
	if len(got) != 1 {
		t.Fatalf("promotable = %d, want 1", len(got))
	}
	if got[0].Sources != 3 {
		t.Errorf("sources = %d, want 3", got[0].Sources)
	}
	if got[0].Mentions != 5 {
		t.Errorf("mentions = %d, want 5", got[0].Mentions)
	}
	if len(got[0].Samples) != 2 {
		t.Errorf("samples = %v, want 2", got[0].Samples)
	}
}

// Re-extracting an item must not inflate a name toward the threshold. The
// threshold is the only thing between the model's vocabulary and the watchlist.
func TestRecordCandidatesIsIdempotent(t *testing.T) {
	db := openTemp(t)

	src := addSource(t, db, "rss", "a feed")
	item := addItem(t, db, src, "https://x.test/1")
	s := []CandidateSighting{sighting("New Thing", item, src)}

	for i := 0; i < 3; i++ {
		if err := db.RecordCandidates(s); err != nil {
			t.Fatalf("RecordCandidates: %v", err)
		}
	}
	if n := count(t, db, "SELECT count(*) FROM candidates"); n != 1 {
		t.Errorf("candidates = %d, want 1", n)
	}
	if n := count(t, db, "SELECT count(*) FROM candidate_sightings"); n != 1 {
		t.Errorf("sightings = %d, want 1", n)
	}
}

// Different spellings of one name are one candidate: the same fold entities use.
func TestRecordCandidatesFoldsSpellings(t *testing.T) {
	db := openTemp(t)

	src := addSource(t, db, "rss", "a feed")
	a := addItem(t, db, src, "https://x.test/1")
	b := addItem(t, db, src, "https://x.test/2")

	if err := db.RecordCandidates([]CandidateSighting{
		sighting("Brand New Thing", a, src),
		sighting("brand-new-thing", b, src),
	}); err != nil {
		t.Fatalf("RecordCandidates: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM candidates"); n != 1 {
		t.Errorf("candidates = %d, want 1", n)
	}
}

func TestPromoteCandidate(t *testing.T) {
	db := openTemp(t)

	src := addSource(t, db, "rss", "a feed")
	item := addItem(t, db, src, "https://x.test/1")
	if err := db.RecordCandidates([]CandidateSighting{sighting("Brand New Thing", item, src)}); err != nil {
		t.Fatalf("RecordCandidates: %v", err)
	}
	cands, _ := db.PromotableCandidates(14, 1, 0)
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}

	entityID, err := db.PromoteCandidate(cands[0].ID, "coding_agents", "")
	if err != nil {
		t.Fatalf("PromoteCandidate: %v", err)
	}

	e, err := db.EntityByID(entityID)
	if err != nil {
		t.Fatalf("EntityByID: %v", err)
	}
	if e.Name != "Brand New Thing" || e.Layer != "coding_agents" || e.Kind != "tool" {
		t.Errorf("promoted entity = %+v", e)
	}

	// The candidate row survives, carrying the link, so it is neither offered
	// again nor counted as pending -- and the sightings that justified the
	// decision stay with it.
	if n := count(t, db, "SELECT count(*) FROM candidates WHERE promoted_entity_id IS NOT NULL"); n != 1 {
		t.Errorf("promoted candidate rows = %d, want 1", n)
	}
	if n := count(t, db, "SELECT count(*) FROM candidate_sightings"); n != 1 {
		t.Error("promotion must not delete the evidence for it")
	}
	again, _ := db.PromotableCandidates(14, 1, 0)
	if len(again) != 0 {
		t.Errorf("promoted candidate still offered: %v", again)
	}
}

// A name that already resolves must not become a second row for a thing the map
// already answers. Same rule ImportWatchlist applies.
func TestPromoteCandidateRefusesExistingAlias(t *testing.T) {
	db := openTemp(t)

	if _, err := db.ImportWatchlist([]EntityImport{
		{Name: "Devin", Aliases: []string{"Windsurf"}, Layer: "coding_agents"},
	}); err != nil {
		t.Fatalf("ImportWatchlist: %v", err)
	}

	src := addSource(t, db, "rss", "a feed")
	item := addItem(t, db, src, "https://x.test/1")
	if err := db.RecordCandidates([]CandidateSighting{sighting("windsurf", item, src)}); err != nil {
		t.Fatalf("RecordCandidates: %v", err)
	}
	cands, _ := db.PromotableCandidates(14, 1, 0)

	if _, err := db.PromoteCandidate(cands[0].ID, "coding_agents", ""); err == nil {
		t.Fatal("promoting an existing alias should fail")
	} else if !strings.Contains(err.Error(), "already resolves") {
		t.Errorf("unhelpful error: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM entities"); n != 1 {
		t.Errorf("entities = %d, want 1", n)
	}
}

// Dismissed names are kept, not deleted: one rejected today will be seen again
// next week, and without the record the same rejection is offered back forever.
func TestDismissCandidateIsRemembered(t *testing.T) {
	db := openTemp(t)

	src := addSource(t, db, "rss", "a feed")
	item := addItem(t, db, src, "https://x.test/1")
	db.RecordCandidates([]CandidateSighting{sighting("Python", item, src)})
	cands, _ := db.PromotableCandidates(14, 1, 0)

	if err := db.DismissCandidate(cands[0].ID); err != nil {
		t.Fatalf("DismissCandidate: %v", err)
	}
	// Seen again, from a new source: still not offered.
	src2 := addSource(t, db, "rss", "another feed")
	item2 := addItem(t, db, src2, "https://y.test/1")
	db.RecordCandidates([]CandidateSighting{sighting("Python", item2, src2)})

	again, _ := db.PromotableCandidates(14, 1, 0)
	if len(again) != 0 {
		t.Errorf("dismissed name offered again: %v", again)
	}
	if n := count(t, db, "SELECT count(*) FROM candidates"); n != 1 {
		t.Errorf("candidate row should survive dismissal, got %d", n)
	}
}

// Idle names are pruned; decisions are not.
func TestPruneCandidatesKeepsDecisions(t *testing.T) {
	db := openTemp(t)

	src := addSource(t, db, "rss", "a feed")
	stale := addItem(t, db, src, "https://x.test/stale")
	kept := addItem(t, db, src, "https://x.test/kept")
	decided := addItem(t, db, src, "https://x.test/decided")

	db.RecordCandidates([]CandidateSighting{
		sighting("Stale Thing", stale, src),
		sighting("Fresh Thing", kept, src),
		sighting("Rejected Thing", decided, src),
	})
	if _, err := db.Exec(
		`UPDATE candidates SET last_seen_at = datetime('now','-40 days')
		  WHERE name_norm IN ('stale thing','rejected thing')`); err != nil {
		t.Fatalf("age candidates: %v", err)
	}
	cands, _ := db.PromotableCandidates(400, 1, 0)
	for _, c := range cands {
		if c.Name == "Rejected Thing" {
			db.DismissCandidate(c.ID)
		}
	}

	n, err := db.PruneCandidates(30)
	if err != nil {
		t.Fatalf("PruneCandidates: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1 (only the undecided idle one)", n)
	}
	if left := count(t, db, "SELECT count(*) FROM candidates"); left != 2 {
		t.Errorf("candidates left = %d, want 2", left)
	}
	if orphan := count(t, db, "SELECT count(*) FROM candidate_sightings"); orphan != 2 {
		t.Errorf("sightings left = %d, want 2 (the pruned one's went with it)", orphan)
	}
}

// candidate_sightings references BOTH items and sources, so DeleteSource has to
// clear it twice. The source clause is the one that is easy to miss: a sighting
// survives its item being re-attributed elsewhere, so clearing only the orphans
// leaves rows pointing at a source about to vanish and the delete rolls back.
func TestDeleteSourceRemovesCandidateSightings(t *testing.T) {
	db := openTemp(t)

	gone := addSource(t, db, "rss", "leaving")
	kept := addSource(t, db, "rss", "staying")
	shared := addItem(t, db, gone, "https://x.test/shared")
	if _, err := db.Exec(
		`INSERT INTO item_sources (item_id, source_id) VALUES (?, ?)`, shared, kept,
	); err != nil {
		t.Fatalf("second attribution: %v", err)
	}
	orphan := addItem(t, db, gone, "https://x.test/orphan")

	db.RecordCandidates([]CandidateSighting{
		sighting("Thing", shared, gone),
		sighting("Thing", orphan, gone),
	})

	if err := db.DeleteSource(gone); err != nil {
		t.Fatalf("DeleteSource: %v", err)
	}
	if n := count(t, db, "SELECT count(*) FROM candidate_sightings"); n != 0 {
		t.Errorf("sightings left = %d, want 0", n)
	}
	// The candidate itself survives with no sightings: it is a name someone may
	// still want to judge, and deleting a source is not a verdict on it.
	if n := count(t, db, "SELECT count(*) FROM candidates"); n != 1 {
		t.Errorf("candidates = %d, want 1", n)
	}
}
