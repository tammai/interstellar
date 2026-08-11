package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// fakeModel is an OpenAI-compatible server whose reply is chosen by a function
// of the prompt, so one server can play both roles in a full-pipeline test.
type fakeModel struct {
	*httptest.Server
	mu      sync.Mutex
	prompts []string
	reply   func(prompt string) string
}

func newFakeModel(t *testing.T, reply func(prompt string) string) *fakeModel {
	t.Helper()
	f := &fakeModel{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)

		var prompt string
		for _, m := range body.Messages {
			prompt += m.Content + "\n"
		}

		f.mu.Lock()
		f.prompts = append(f.prompts, prompt)
		f.mu.Unlock()

		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": f.reply(prompt)}}},
			"usage":   map[string]int{"prompt_tokens": 50, "completion_tokens": 20},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeModel) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

// clientFor builds an llm.Client pointed at a fake model for both roles.
func clientFor(t *testing.T, db *store.DB, srv *fakeModel) *llm.Client {
	t.Helper()
	cfg, err := llm.LoadConfig(map[string]string{
		store.KeyProviders:        fmt.Sprintf(`[{"name":"fake","base_url":%q,"api_key_env":""}]`, srv.URL),
		store.KeyClassifierModel:  "fake/classifier-model",
		store.KeySynthesizerModel: "fake/synth-model",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return llm.NewClient(cfg, db, store.RunDate(time.Now()), quietLogger())
}

// seedTopic adds a topic with a real definition, so it isn't skipped as unset.
func seedTopic(t *testing.T, db *store.DB, title, definition string) int64 {
	t.Helper()
	id, err := db.CreateTopic(store.Topic{
		Title: title, Definition: definition, Active: true, Priority: 1,
	})
	if err != nil {
		t.Fatalf("create topic: %v", err)
	}
	return id
}

// seedItem inserts an item attributed to a source.
func seedItem(t *testing.T, db *store.DB, sourceID int64, url, title string) int64 {
	t.Helper()
	id, _, err := db.UpsertItem(store.NewItem{
		SourceID: sourceID, CanonicalURL: url, Title: title, Summary: "Summary of " + title,
	})
	if err != nil {
		t.Fatalf("upsert item: %v", err)
	}
	return id
}

func seedSource(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO sources (kind, name, config, active) VALUES ('rss', ?, '{"url":"https://x.test/f"}', 1)`, name)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// scoreAllHigh replies with a 5 for every item_id in the prompt.
func scoreAllHigh(prompt string) string {
	ids := itemIDsIn(prompt)
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"item_id":%d,"score":5,"reason":"on topic"}`, id))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// oneStory clusters every item in the prompt into a single signal.
func oneStory(prompt string) string {
	ids := itemIDsIn(prompt)
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprint(id))
	}
	return fmt.Sprintf(`[{"title":"The one story","reason":"It matters.","item_ids":[%s]}]`,
		strings.Join(parts, ","))
}

func itemIDsIn(prompt string) []int64 {
	var out []int64
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "item_id: "); ok {
			var id int64
			if _, err := fmt.Sscanf(after, "%d", &id); err == nil {
				out = append(out, id)
			}
		}
	}
	return out
}

func routeByRole(prompt string) string {
	// The synthesizer prompt is the one that asks for stories.
	if strings.Contains(prompt, "MATCHED ITEMS") {
		return oneStory(prompt)
	}
	return scoreAllHigh(prompt)
}

func TestClassifyStoresMatchesAboveThreshold(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	topic := seedTopic(t, db, "Agents", "Anything about agent harnesses.")
	item1 := seedItem(t, db, src, "https://x.test/a", "Agent harness patterns")
	item2 := seedItem(t, db, src, "https://x.test/b", "Unrelated gardening post")

	model := newFakeModel(t, func(prompt string) string {
		return fmt.Sprintf(`[{"item_id":%d,"score":5,"reason":"squarely on topic"},
		                     {"item_id":%d,"score":2,"reason":"different field"}]`, item1, item2)
	})

	cfg := testConfig()
	result, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, quietLogger())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if result.MatchesKept != 1 {
		t.Errorf("kept %d matches, want 1 (threshold is %v)", result.MatchesKept, cfg.MatchThreshold)
	}
	if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ?", topic); n != 1 {
		t.Errorf("matches table has %d rows, want 1", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM matches WHERE item_id = ?", item2); n != 0 {
		t.Error("a score of 2 was stored; §5.2 keeps >= 4 only")
	}
}

// A topic with no definition has nothing to match against. Classifying it would
// burn tokens to produce noise.
func TestClassifySkipsTopicsWithoutDefinitions(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	seedItem(t, db, src, "https://x.test/a", "Something")
	if _, err := db.CreateTopic(store.Topic{Title: "Unset", Definition: "", Active: true}); err != nil {
		t.Fatal(err)
	}

	model := newFakeModel(t, scoreAllHigh)
	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if model.callCount() != 0 {
		t.Errorf("model called %d times for a topic with no definition", model.callCount())
	}
	if len(result.Problems) == 0 {
		t.Error("no problem reported; the setup hint has nothing to show")
	}
}

// The watermark is what makes a second refresh cheap.
func TestClassifyWatermarkMakesRerunFree(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	seedTopic(t, db, "Agents", "Agent harnesses.")
	seedItem(t, db, src, "https://x.test/a", "First post")

	model := newFakeModel(t, scoreAllHigh)
	client := clientFor(t, db, model)

	cfg := testConfig()
	if _, err := Classify(context.Background(), db, client, cfg, quietLogger()); err != nil {
		t.Fatalf("first classify: %v", err)
	}
	afterFirst := model.callCount()
	if afterFirst == 0 {
		t.Fatal("first pass made no calls")
	}

	// Re-read config so the advanced watermark is picked up, exactly as a second
	// refresh would.
	cfg2, err := configFromDB(db)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Classify(context.Background(), db, client, cfg2, quietLogger())
	if err != nil {
		t.Fatalf("second classify: %v", err)
	}

	if result.ItemsConsidered != 0 {
		t.Errorf("second pass considered %d items, want 0", result.ItemsConsidered)
	}
	if model.callCount() != afterFirst {
		t.Errorf("second pass made %d extra calls; the watermark is not holding",
			model.callCount()-afterFirst)
	}
}

// A verdict naming an id that wasn't in the batch must be dropped: storing it
// would attach a reason to an unrelated article.
func TestClassifyDropsHallucinatedItemIDs(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	seedTopic(t, db, "Agents", "Agent harnesses.")
	real := seedItem(t, db, src, "https://x.test/a", "Real post")

	model := newFakeModel(t, func(string) string {
		return fmt.Sprintf(`[{"item_id":%d,"score":5,"reason":"real"},
		                     {"item_id":999999,"score":5,"reason":"invented"}]`, real)
	})

	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if result.VerdictsDropped != 1 {
		t.Errorf("dropped %d verdicts, want 1", result.VerdictsDropped)
	}
	if n := countRows(t, db, "SELECT count(*) FROM matches"); n != 1 {
		t.Errorf("matches = %d, want 1", n)
	}
}

// The repair round-trip: unparseable first response, valid second.
func TestClassifyRepairsUnparseableOutput(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	seedTopic(t, db, "Agents", "Agent harnesses.")
	item := seedItem(t, db, src, "https://x.test/a", "A post")

	var calls int
	model := newFakeModel(t, func(string) string {
		calls++
		if calls == 1 {
			return "I'd be happy to help! Let me analyze these items for you."
		}
		return fmt.Sprintf(`[{"item_id":%d,"score":5,"reason":"fixed"}]`, item)
	})

	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if result.Repairs != 1 {
		t.Errorf("Repairs = %d, want 1", result.Repairs)
	}
	if result.MatchesKept != 1 {
		t.Errorf("MatchesKept = %d, want 1 -- the repair should have recovered the batch", result.MatchesKept)
	}
}

// A model that never produces JSON must cost a bounded number of calls.
func TestClassifyGivesUpAfterOneRepair(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	seedTopic(t, db, "Agents", "Agent harnesses.")
	seedItem(t, db, src, "https://x.test/a", "A post")

	model := newFakeModel(t, func(string) string { return "Sorry, I cannot do that." })

	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), quietLogger())
	if err != nil {
		t.Fatalf("Classify should not fail the run: %v", err)
	}
	if model.callCount() != 2 {
		t.Errorf("model called %d times, want 2 (attempt + one repair)", model.callCount())
	}
	if len(result.Problems) == 0 {
		t.Error("no problem recorded for a topic that never parsed")
	}
}

func TestSynthesizeComposesSignalsAndComputesSourceCount(t *testing.T) {
	db := testDB(t)
	topic := seedTopic(t, db, "Open weights", "Open-weight model releases.")

	// One story carried by three sources: the shape that must render gold.
	s1 := seedSource(t, db, "Hugging Face")
	s2 := seedSource(t, db, "Hacker News")
	s3 := seedSource(t, db, "Simon Willison")
	item := seedItem(t, db, s1, "https://x.test/qwen", "Qwen 72B released")
	for _, s := range []int64{s2, s3} {
		if _, _, err := db.UpsertItem(store.NewItem{
			SourceID: s, CanonicalURL: "https://x.test/qwen", Title: "Qwen 72B released",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.SaveMatches([]store.Match{
		{ItemID: item, TopicID: topic, Score: 5, Reason: "direct hit"},
	}); err != nil {
		t.Fatal(err)
	}

	model := newFakeModel(t, oneStory)
	runDate := store.RunDate(time.Now())
	cfg := testConfig()

	result, err := Synthesize(context.Background(), db, clientFor(t, db, model), cfg, runDate, quietLogger())
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if result.SignalsWritten != 1 {
		t.Fatalf("wrote %d signals, want 1", result.SignalsWritten)
	}

	signals, err := db.RecentSignals(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) != 1 {
		t.Fatalf("got %d signals, want 1", len(signals))
	}
	sig := signals[0]

	// The whole reason item_sources exists.
	if sig.SourceCount != 3 {
		t.Errorf("source_count = %d, want 3 -- this star should be gold", sig.SourceCount)
	}
	if sig.SourceCount < cfg.HotThreshold {
		t.Error("signal is not hot; the gold flag will not fire")
	}
	if sig.Score != 5 {
		t.Errorf("score = %v, want 5 (max over members)", sig.Score)
	}
	if sig.PrimaryURL != "https://x.test/qwen" {
		t.Errorf("primary_url = %q", sig.PrimaryURL)
	}
	if result.HotSignals != 1 {
		t.Errorf("HotSignals = %d, want 1", result.HotSignals)
	}
}

// §5.3 caps titles at 120 chars, and the cap is enforced by us, not trusted from
// the model -- the sky has no room to reflow.
func TestSynthesizeTruncatesLongTitles(t *testing.T) {
	db := testDB(t)
	topic := seedTopic(t, db, "T", "A definition.")
	src := seedSource(t, db, "Blog")
	item := seedItem(t, db, src, "https://x.test/a", "A post")
	db.SaveMatches([]store.Match{{ItemID: item, TopicID: topic, Score: 5}})

	long := strings.Repeat("very long title ", 20)
	model := newFakeModel(t, func(string) string {
		return fmt.Sprintf(`[{"title":%q,"reason":"r","item_ids":[%d]}]`, long, item)
	})

	if _, err := Synthesize(context.Background(), db, clientFor(t, db, model),
		testConfig(), store.RunDate(time.Now()), quietLogger()); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	signals, _ := db.RecentSignals(7)
	if len(signals) != 1 {
		t.Fatalf("got %d signals", len(signals))
	}
	if len(signals[0].Title) > maxSignalTitle {
		t.Errorf("title is %d chars, want <= %d", len(signals[0].Title), maxSignalTitle)
	}
}

// A cluster referencing an invented item would produce a star whose detail panel
// contradicts itself.
func TestSynthesizeDropsInventedMembers(t *testing.T) {
	db := testDB(t)
	topic := seedTopic(t, db, "T", "A definition.")
	src := seedSource(t, db, "Blog")
	item := seedItem(t, db, src, "https://x.test/a", "A post")
	db.SaveMatches([]store.Match{{ItemID: item, TopicID: topic, Score: 5}})

	model := newFakeModel(t, func(string) string {
		return fmt.Sprintf(`[{"title":"Real","reason":"r","item_ids":[%d, 424242]},
		                     {"title":"Entirely invented","reason":"r","item_ids":[999999]}]`, item)
	})

	if _, err := Synthesize(context.Background(), db, clientFor(t, db, model),
		testConfig(), store.RunDate(time.Now()), quietLogger()); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	signals, _ := db.RecentSignals(7)
	if len(signals) != 1 {
		t.Fatalf("got %d signals, want 1 (the fully invented cluster must be dropped)", len(signals))
	}
	if n := countRows(t, db, "SELECT count(*) FROM signal_items WHERE signal_id = ?", signals[0].ID); n != 1 {
		t.Errorf("signal has %d members, want 1 (the invented id must be dropped)", n)
	}
}

// §10 relies on compose being idempotent: a second run for the same day must not
// duplicate stars.
func TestSynthesizeIsIdempotentForARunDate(t *testing.T) {
	db := testDB(t)
	topic := seedTopic(t, db, "T", "A definition.")
	src := seedSource(t, db, "Blog")
	item := seedItem(t, db, src, "https://x.test/a", "A post")
	db.SaveMatches([]store.Match{{ItemID: item, TopicID: topic, Score: 5}})

	model := newFakeModel(t, oneStory)
	client := clientFor(t, db, model)
	runDate := store.RunDate(time.Now())

	for i := 0; i < 3; i++ {
		if _, err := Synthesize(context.Background(), db, client, testConfig(), runDate, quietLogger()); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	if n := countRows(t, db, "SELECT count(*) FROM signals"); n != 1 {
		t.Errorf("signals = %d after three runs, want 1", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM signal_items"); n != 1 {
		t.Errorf("signal_items = %d after three runs, want 1", n)
	}
}

// A topic that matched nothing must end the day with no stars, and must clear a
// stale group from an earlier run the same day.
func TestSynthesizeClearsTopicsWithNoMatches(t *testing.T) {
	db := testDB(t)
	topic := seedTopic(t, db, "Quiet topic", "Something that runs dry.")
	src := seedSource(t, db, "Blog")
	item := seedItem(t, db, src, "https://x.test/a", "A post")
	db.SaveMatches([]store.Match{{ItemID: item, TopicID: topic, Score: 5}})

	model := newFakeModel(t, oneStory)
	client := clientFor(t, db, model)
	runDate := store.RunDate(time.Now())

	if _, err := Synthesize(context.Background(), db, client, testConfig(), runDate, quietLogger()); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM signals"); n != 1 {
		t.Fatalf("setup failed: %d signals", n)
	}

	// The match goes away (the owner narrowed the topic definition, say).
	if _, err := db.Exec("DELETE FROM matches"); err != nil {
		t.Fatal(err)
	}
	if _, err := Synthesize(context.Background(), db, client, testConfig(), runDate, quietLogger()); err != nil {
		t.Fatal(err)
	}

	if n := countRows(t, db, "SELECT count(*) FROM signals"); n != 0 {
		t.Errorf("signals = %d, want 0 -- a stale star from an earlier run today is still showing", n)
	}
}

// End to end on one database: ingest, classify, compose, then read the sky.
func TestFullPipelineProducesASky(t *testing.T) {
	db := testDB(t)

	feed := feedServer(t, "Verification loops in agent harnesses", "https://example.test/loops")
	addSource(t, db, "Test Blog", feed.URL)
	seedTopic(t, db, "Agents & Harness Engineering", "Agent harnesses, verification loops, tool design.")

	model := newFakeModel(t, routeByRole)
	client := clientFor(t, db, model)
	cfg := testConfig()
	runDate := store.RunDate(time.Now())

	if _, err := Ingest(context.Background(), db, cfg, quietLogger()); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := Classify(context.Background(), db, client, cfg, quietLogger()); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if _, err := Synthesize(context.Background(), db, client, cfg, runDate, quietLogger()); err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	signals, err := db.RecentSignals(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(signals) == 0 {
		t.Fatal("the sky is empty after a full run")
	}

	// The detail panel must be able to render what the run produced.
	detail, err := db.SignalByID(signals[0].ID)
	if err != nil {
		t.Fatalf("SignalByID: %v", err)
	}
	if detail.Title == "" || detail.TopicTitle == "" {
		t.Errorf("detail incomplete: %+v", detail)
	}
	if len(detail.Members) == 0 {
		t.Error("signal has no members")
	}
	if len(detail.SeenOn) == 0 {
		t.Error(`no "Seen on" attribution`)
	}
	if detail.AgeDays != 0 {
		t.Errorf("age_days = %d for today's signal, want 0", detail.AgeDays)
	}
}

// configFromDB re-reads runtime config, as each run does. Config is never cached
// across runs, which is what makes a settings change take effect without a
// restart -- and here it is what lets the second pass see the advanced watermark.
func configFromDB(db *store.DB) (config.Runtime, error) {
	return config.Load(db)
}
