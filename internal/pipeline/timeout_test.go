package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/llm"
)

// One topic whose model is too slow must cost that topic, not the run. The stages
// used to read a per-call timeout as the run being cancelled, so a single slow
// compose -- 38 items on a reasoning model, twice over two minutes -- stopped
// composition for every topic after it, raised the banner, and ended the catch-up
// chain with it.
func TestASlowTopicDoesNotStopCompose(t *testing.T) {
	prev := llm.CallTimeout
	llm.CallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { llm.CallTimeout = prev })

	db := testDB(t)
	src := seedSource(t, db, "Blog")
	seedTopic(t, db, "Slow", "Takes forever.") // first by id, so compose meets it before Fast
	fast := seedTopic(t, db, "Fast", "Answers at once.")
	seedItem(t, db, src, "https://x.test/a", "A post")

	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "MATCHED ITEMS") && strings.Contains(prompt, "TOPIC: Slow") {
			time.Sleep(600 * time.Millisecond) // past the timeout
		}
		return routeByRole(prompt)
	})
	client := clientFor(t, db, model)
	cfg := testConfig()

	if _, err := Classify(context.Background(), db, client, cfg, testRunDate, quietLogger()); err != nil {
		t.Fatalf("Classify: %v", err)
	}
	res, err := Synthesize(context.Background(), db, client, cfg, testRunDate, quietLogger())
	if err != nil {
		t.Fatalf("Synthesize = %v; a slow topic stopped compose for the rest", err)
	}

	if n := countRows(t, db, "SELECT count(*) FROM signals WHERE topic_id = ?", fast); n == 0 {
		t.Error("the fast topic got no signal")
	}
	var named bool
	for _, p := range res.Problems {
		if strings.Contains(p, "Slow") && strings.Contains(p, "did not answer within") {
			named = true
		}
	}
	if !named {
		t.Errorf("problems = %v, want the slow topic named with the timeout in words", res.Problems)
	}
}
