package store

import (
	"strings"
	"testing"

	"github.com/tammai/interstellar/seed"
)

// The default must always parse: it is what every fresh container boots on, and a
// broken one would stop a first boot with nobody watching.
func TestDefaultSeedParses(t *testing.T) {
	s, err := ParseSeed(seed.Default())
	if err != nil {
		t.Fatalf("embedded default seed: %v", err)
	}
	if len(s.Topics) == 0 || len(s.Sources) == 0 {
		t.Errorf("default seed has %d topics and %d sources; a fresh sky needs both", len(s.Topics), len(s.Sources))
	}
}

// Every problem is reported at once, so one edit fixes a seed rather than one
// boot per mistake.
func TestParseSeedReportsEveryProblem(t *testing.T) {
	_, err := ParseSeed([]byte(`{
	  "version": 1,
	  "topics": [
	    {"title": "Agents", "definition": "x"},
	    {"title": "agents", "definition": "y"},
	    {"title": "", "definition": "z"},
	    {"title": "Half placed", "pos_x": 0.5}
	  ],
	  "sources": [
	    {"kind": "rss", "name": "A", "config": {"url": "https://a.test/feed"}, "active": true},
	    {"kind": "rss", "name": "A again", "config": {"url": "https://A.test/feed/"}, "active": true},
	    {"kind": "atom", "name": "B", "config": {"url": "https://b.test"}, "active": true},
	    {"kind": "hn_search", "name": "HN 1", "config": {"queries": [{"query": "LLM"}]}, "active": true},
	    {"kind": "hn_search", "name": "HN 2", "config": {"queries": [{"query": "MCP"}]}, "active": true},
	    {"kind": "rss", "name": "C", "config": "not an object", "active": true}
	  ]
	}`))
	if err == nil {
		t.Fatal("a seed with eight problems parsed")
	}
	for _, want := range []string{
		`"agents" appears twice`, "topic 3 has no title", "only one of pos_x and pos_y",
		`fetches the same thing as "A"`, `kind "atom"`, "2 hn_search sources", "not a JSON object",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestParseSeedRejectsUnknownFields(t *testing.T) {
	_, err := ParseSeed([]byte(`{"version": 1, "topics": [{"title": "A", "defintion": "typo"}], "sources": []}`))
	if err == nil {
		t.Error("a misspelled field parsed silently; the definition would have been dropped")
	}
}

// A custom seed lands as written: definitions, activity, and a position for a
// topic that gave none.
func TestBootstrapWithACustomSeed(t *testing.T) {
	db := openTemp(t)
	custom, err := ParseSeed([]byte(`{
	  "version": 1,
	  "topics": [
	    {"title": "Robotics", "definition": "Embodied AI.", "priority": 2},
	    {"title": "Paused one", "definition": "Later.", "active": false}
	  ],
	  "sources": [
	    {"kind": "rss", "name": "Feed", "config": {"url": "https://f.test/rss"}, "active": true}
	  ]
	}`))
	if err != nil {
		t.Fatalf("ParseSeed: %v", err)
	}
	if err := db.BootstrapWith(func() (Seed, error) { return custom, nil }); err != nil {
		t.Fatalf("BootstrapWith: %v", err)
	}

	topics, _ := db.AllTopics()
	if len(topics) != 2 {
		t.Fatalf("got %d topics, want 2", len(topics))
	}
	if topics[0].Title != "Robotics" || topics[0].Definition != "Embodied AI." || !topics[0].Active {
		t.Errorf("first topic = %+v", topics[0])
	}
	if topics[1].Active {
		t.Error("a topic seeded inactive came out active")
	}
	if topics[0].PosX == 0 && topics[0].PosY == 0 {
		t.Error("a topic without a position was not placed")
	}
	if n := count(t, db, `SELECT count(*) FROM sources WHERE config = '{"url":"https://f.test/rss"}'`); n != 1 {
		t.Error("the source was not stored with a compact config")
	}
}

// Past the first boot the seed is never read, so a seed file broken later cannot
// stop an existing instance from starting.
func TestBootstrapDoesNotReadTheSeedOnceSeeded(t *testing.T) {
	db := openTemp(t)
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	called := false
	if err := db.BootstrapWith(func() (Seed, error) {
		called = true
		return Seed{}, nil
	}); err != nil {
		t.Fatalf("second BootstrapWith: %v", err)
	}
	if called {
		t.Error("the seed was loaded on a database that was already seeded")
	}
}
