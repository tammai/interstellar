package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/sources"
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
		store.KeyChatModel:        "fake/chat-model",
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return llm.NewClient(cfg, db, store.RunDate(time.Now()), quietLogger())
}

// clientForCapped is clientFor with a per-run call cap, for testing what an
// aborted pass leaves behind.
func clientForCapped(t *testing.T, db *store.DB, srv *fakeModel, runCap int) *llm.Client {
	t.Helper()
	cfg, err := llm.LoadConfig(map[string]string{
		store.KeyProviders:        fmt.Sprintf(`[{"name":"fake","base_url":%q,"api_key_env":""}]`, srv.URL),
		store.KeyClassifierModel:  "fake/classifier-model",
		store.KeySynthesizerModel: "fake/synth-model",
		store.KeyMaxCallsPerRun:   fmt.Sprint(runCap),
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
	result, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger())
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
	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), testRunDate, quietLogger())
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
	if _, err := Classify(context.Background(), db, client, cfg, testRunDate, quietLogger()); err != nil {
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
	result, err := Classify(context.Background(), db, client, cfg2, testRunDate, quietLogger())
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

	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), testRunDate, quietLogger())
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

	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), testRunDate, quietLogger())
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

	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), testRunDate, quietLogger())
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
	if _, err := Classify(context.Background(), db, client, cfg, testRunDate, quietLogger()); err != nil {
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

// How many matches exist is decided by the classifier's generosity, so the
// synthesizer needs its own bound or one compose call becomes an arbitrarily
// large prompt. Found by running the real pipeline: a lenient classifier
// produced a single signal with 113 members.
func TestSynthesizeBoundsTheComposePrompt(t *testing.T) {
	db := testDB(t)
	topic := seedTopic(t, db, "Busy topic", "Everything matches this.")
	src := seedSource(t, db, "Blog")

	const matchCount = maxMatchesPerTopic + 25
	matches := make([]store.Match, 0, matchCount)
	for i := 0; i < matchCount; i++ {
		url := fmt.Sprintf("https://x.test/post-%d", i)
		id := seedItem(t, db, src, url, fmt.Sprintf("Post %d", i))
		// Descending scores, so the cap should keep the highest.
		matches = append(matches, store.Match{
			ItemID: id, TopicID: topic, Score: 5 - float64(i)*0.01,
		})
	}
	if err := db.SaveMatches(matches); err != nil {
		t.Fatal(err)
	}

	var sawItems int
	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "MATCHED ITEMS") {
			sawItems = len(itemIDsIn(prompt))
		}
		return oneStory(prompt)
	})

	if _, err := Synthesize(context.Background(), db, clientFor(t, db, model),
		testConfig(), store.RunDate(time.Now()), quietLogger()); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	if sawItems > maxMatchesPerTopic {
		t.Errorf("compose prompt carried %d items, want at most %d", sawItems, maxMatchesPerTopic)
	}
	if sawItems == 0 {
		t.Fatal("compose prompt carried no items")
	}

	// And a signal cannot end up with more members than the prompt had.
	signals, _ := db.RecentSignals(7)
	if len(signals) != 1 {
		t.Fatalf("got %d signals", len(signals))
	}
	members := countRows(t, db, "SELECT count(*) FROM signal_items WHERE signal_id = ?", signals[0].ID)
	if members > maxMatchesPerTopic {
		t.Errorf("signal has %d members, want at most %d", members, maxMatchesPerTopic)
	}
}

// Classification batches must run concurrently. Sequentially, a real run of
// 7 topics x 12 batches at ~25s per call takes half an hour — measured against a
// live provider, and several times over §10's three-minute budget.
func TestClassifyRunsBatchesConcurrently(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	for i := 0; i < 90; i++ {
		seedItem(t, db, src, fmt.Sprintf("https://x.test/p-%d", i), fmt.Sprintf("Post %d", i))
	}
	seedTopic(t, db, "A", "A definition.")
	seedTopic(t, db, "B", "Another definition.")

	var mu sync.Mutex
	inFlight, peak := 0, 0

	model := newFakeModel(t, func(prompt string) string {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		time.Sleep(60 * time.Millisecond) // long enough for overlap to be observable
		mu.Lock()
		inFlight--
		mu.Unlock()
		return scoreAllHigh(prompt)
	})

	if _, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), testRunDate, quietLogger()); err != nil {
		t.Fatalf("Classify: %v", err)
	}

	mu.Lock()
	got := peak
	mu.Unlock()
	if got < 2 {
		t.Errorf("peak concurrent model calls = %d; batches are running one at a time", got)
	}
	if got > classifyWorkers {
		t.Errorf("peak concurrent model calls = %d, above the %d cap; a provider would rate-limit",
			got, classifyWorkers)
	}
}

// Concurrency must not cost idempotency: the watermark still has to hold.
func TestConcurrentClassifyStillHonorsWatermark(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	for i := 0; i < 70; i++ {
		seedItem(t, db, src, fmt.Sprintf("https://x.test/w-%d", i), fmt.Sprintf("Post %d", i))
	}
	seedTopic(t, db, "A", "A definition.")
	seedTopic(t, db, "B", "Another definition.")

	model := newFakeModel(t, scoreAllHigh)
	client := clientFor(t, db, model)

	if _, err := Classify(context.Background(), db, client, testConfig(), testRunDate, quietLogger()); err != nil {
		t.Fatal(err)
	}
	first := model.callCount()

	cfg2, err := configFromDB(db)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Classify(context.Background(), db, client, cfg2, testRunDate, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if result.ItemsConsidered != 0 {
		t.Errorf("second pass considered %d items, want 0", result.ItemsConsidered)
	}
	if model.callCount() != first {
		t.Errorf("second pass made %d extra calls", model.callCount()-first)
	}
}

// --- per-topic stats ---

// statsFor reads back what a pass recorded, so the assertions below talk about
// the stored row rather than the in-memory result they were derived from.
func statsFor(t *testing.T, db *store.DB, topicID int64) store.TopicHitRate {
	t.Helper()
	rates, err := db.TopicHitRates(7)
	if err != nil {
		t.Fatalf("TopicHitRates: %v", err)
	}
	for _, r := range rates {
		if r.TopicID == topicID {
			return r
		}
	}
	t.Fatalf("no stats row for topic %d", topicID)
	return store.TopicHitRate{}
}

func TestClassifyRecordsPerTopicStats(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	agents := seedTopic(t, db, "Agents", "Agent harnesses.")
	weights := seedTopic(t, db, "Open weights", "Open-weight releases.")

	hit := seedItem(t, db, src, "https://x.test/a", "Agent harness patterns")
	miss := seedItem(t, db, src, "https://x.test/b", "Gardening in August")

	// Only the Agents topic matches, and only one of its two items.
	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "TOPIC: Agents") {
			return fmt.Sprintf(`[{"item_id":%d,"score":5,"reason":"on topic"},
			                     {"item_id":%d,"score":1}]`, hit, miss)
		}
		return fmt.Sprintf(`[{"item_id":%d,"score":2},{"item_id":%d,"score":1}]`, hit, miss)
	})

	if _, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), testRunDate, quietLogger()); err != nil {
		t.Fatalf("Classify: %v", err)
	}

	// Both topics saw both items: the denominator is per topic, not shared.
	a := statsFor(t, db, agents)
	if a.ItemsConsidered != 2 || a.MatchesKept != 1 || a.Batches != 1 {
		t.Errorf("Agents: considered=%d matched=%d batches=%d, want 2/1/1",
			a.ItemsConsidered, a.MatchesKept, a.Batches)
	}
	if a.HitRate != 0.5 {
		t.Errorf("Agents hit rate = %v, want 0.5", a.HitRate)
	}

	w := statsFor(t, db, weights)
	if w.ItemsConsidered != 2 || w.MatchesKept != 0 {
		t.Errorf("Open weights: considered=%d matched=%d, want 2/0",
			w.ItemsConsidered, w.MatchesKept)
	}
	// A real zero, not the never-classified sentinel: this definition was given
	// two items and rejected both.
	if w.HitRate != 0 {
		t.Errorf("Open weights hit rate = %v, want exactly 0", w.HitRate)
	}
}

// A batch the model never returned parseable JSON for had its items looked at in
// no meaningful sense. Counting them would blame the topic's definition for a
// provider's bad afternoon.
func TestClassifyStatsExcludeFailedBatches(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	good := seedTopic(t, db, "Agents", "Agent harnesses.")
	broken := seedTopic(t, db, "Broken", "A definition the model chokes on.")

	for i := range 4 {
		seedItem(t, db, src, fmt.Sprintf("https://x.test/%d", i), fmt.Sprintf("Post %d", i))
	}

	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "TOPIC: Broken") {
			return "I'm afraid I can't do that."
		}
		return scoreAllHigh(prompt)
	})

	result, err := Classify(context.Background(), db, clientFor(t, db, model), testConfig(), testRunDate, quietLogger())
	if err != nil {
		t.Fatalf("Classify should survive one topic failing: %v", err)
	}
	if len(result.Problems) == 0 {
		t.Error("no problem recorded for the topic that never parsed")
	}

	g := statsFor(t, db, good)
	if g.ItemsConsidered != 4 || g.MatchesKept != 4 {
		t.Errorf("Agents: considered=%d matched=%d, want 4/4", g.ItemsConsidered, g.MatchesKept)
	}

	b := statsFor(t, db, broken)
	if b.ItemsConsidered != 0 || b.Batches != 0 {
		t.Errorf("Broken: considered=%d batches=%d, want 0/0 — a failed batch is not a denominator",
			b.ItemsConsidered, b.Batches)
	}
	if b.HitRate != -1 {
		t.Errorf("Broken hit rate = %v, want the -1 unknown sentinel", b.HitRate)
	}
}

// An aborted pass leaves its items unclaimed -- the watermark does not advance, so
// the next run classifies them again. Recording this pass's denominator would count
// those same items twice and understate every hit rate, so stats and watermark are
// committed together or not at all.
func TestClassifyRecordsNoStatsWhenTheCapAborts(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	first := seedTopic(t, db, "Agents", "Agent harnesses.")
	second := seedTopic(t, db, "Open weights", "Open-weight releases.")
	seedItem(t, db, src, "https://x.test/a", "A post")

	model := newFakeModel(t, scoreAllHigh)

	// A run cap of one against two topics: one batch lands cleanly and the other
	// is refused. The successful one is exactly the work that must not be
	// recorded, because its items go back into the next run's window.
	client := clientForCapped(t, db, model, 1)

	if _, err := Classify(context.Background(), db, client, testConfig(), testRunDate, quietLogger()); !errors.Is(err, llm.ErrCapReached) {
		t.Fatalf("Classify error = %v, want ErrCapReached", err)
	}

	for _, topic := range []int64{first, second} {
		s := statsFor(t, db, topic)
		if s.ItemsConsidered != 0 || s.Batches != 0 {
			t.Errorf("topic %d: considered=%d batches=%d, want 0/0 — an aborted pass records nothing, "+
				"including the batch that succeeded", topic, s.ItemsConsidered, s.Batches)
		}
	}

	// And the watermark stayed put, which is what makes the re-run correct.
	if mark, ok, _ := db.Setting(store.KeyClassifyWatermark); ok && mark != "" {
		t.Errorf("watermark = %q, want unset after an aborted pass", mark)
	}
}

// A day holds the scheduled run plus any manual refresh. Each pass sees only what
// the watermark had not covered, so the day's totals have to add up.
func TestClassifyStatsAccumulateAcrossPassesInADay(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	topic := seedTopic(t, db, "Agents", "Agent harnesses.")
	seedItem(t, db, src, "https://x.test/a", "First post")

	model := newFakeModel(t, scoreAllHigh)
	cfg := testConfig()

	if _, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger()); err != nil {
		t.Fatalf("first Classify: %v", err)
	}

	// A second item arrives, and a second pass picks up only that one. fetched_at
	// has second resolution and ItemsSince is a strict >, so the arrival has to be
	// pushed past the first item's timestamp explicitly -- seeded back to back they
	// land in the same second and the second pass would see nothing.
	second := seedItem(t, db, src, "https://x.test/b", "Second post")
	if _, err := db.Exec(
		`UPDATE items SET fetched_at = datetime('now', '+1 minute') WHERE id = ?`, second); err != nil {
		t.Fatalf("age the second item: %v", err)
	}

	cfg2 := cfg
	mark, _, _ := db.Setting(store.KeyClassifyWatermark)
	cfg2.ClassifyWatermark = mark

	if _, err := Classify(context.Background(), db, clientFor(t, db, model), cfg2, testRunDate, quietLogger()); err != nil {
		t.Fatalf("second Classify: %v", err)
	}

	s := statsFor(t, db, topic)
	if s.ItemsConsidered != 2 || s.MatchesKept != 2 || s.Batches != 2 {
		t.Errorf("considered=%d matched=%d batches=%d, want 2/2/2 across two passes",
			s.ItemsConsidered, s.MatchesKept, s.Batches)
	}
	if s.RunDays != 1 {
		t.Errorf("run days = %d, want 1: both passes ran on the same date", s.RunDays)
	}
}

// clientForChatCapped builds a client with a chat-specific daily cap, for testing
// that chat's allowance is separate from the pipeline's.
func clientForChatCapped(t *testing.T, db *store.DB, srv *fakeModel, runCap, chatDayCap int) *llm.Client {
	t.Helper()
	settings := map[string]string{
		store.KeyProviders:          fmt.Sprintf(`[{"name":"fake","base_url":%q,"api_key_env":""}]`, srv.URL),
		store.KeyClassifierModel:    "fake/classifier-model",
		store.KeySynthesizerModel:   "fake/synth-model",
		store.KeyChatModel:          "fake/chat-model",
		store.KeyMaxChatCallsPerDay: fmt.Sprint(chatDayCap),
	}
	if runCap > 0 {
		settings[store.KeyMaxCallsPerRun] = fmt.Sprint(runCap)
	}
	cfg, err := llm.LoadConfig(settings)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return llm.NewClient(cfg, db, store.RunDate(time.Now()), quietLogger())
}

// captionStub redirects the sources package's HTTP client at a local server that
// impersonates YouTube's watch page and caption endpoint.
//
// Swapping the shared client is heavy-handed, but it is the only seam: the caption
// path builds its own youtube.com URLs by design, because the video id is all the
// caller has. The alternative is a test that reaches the real YouTube, which would
// break the rule that the suite never makes live calls.
type captionStub struct {
	srv  *httptest.Server
	prev http.RoundTripper
}

func newCaptionStub(t *testing.T) *captionStub {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/watch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><script>var x = {"captionTracks":[`+
			`{"baseUrl":"%s/track","languageCode":"en","kind":""}]};</script></html>`,
			"http://"+r.Host)
	})
	mux.HandleFunc("/track", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"events":[
			{"segs":[{"utf8":"So the claim is that "}]},
			{"segs":[{"utf8":"retrieval beats a bigger window"}]},
			{"segs":[{"utf8":" for long documents."}]}
		]}`)
	})
	srv := httptest.NewServer(mux)

	stub := &captionStub{srv: srv, prev: sources.DefaultClient.Transport}
	// Rewrite youtube.com to the stub, leaving every other host alone so this cannot
	// silently swallow an unrelated request.
	sources.DefaultClient.Transport = rewriteHost{to: srv.Listener.Addr().String(), base: stub.prev}
	return stub
}

func (c *captionStub) restore() {
	sources.DefaultClient.Transport = c.prev
	c.srv.Close()
}

type rewriteHost struct {
	to   string
	base http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Host, "youtube.com") {
		req = req.Clone(req.Context())
		req.URL.Scheme = "http"
		req.URL.Host = r.to
	}
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// Items a topic never actually got a verdict on must not be abandoned.
//
// A batch fails when the model returns unparseable JSON twice. The watermark used to
// advance regardless, so those items were never offered to that topic again -- a
// silent, permanent hole in one topic's coverage, invisible except as a topic that
// mysteriously matches nothing. §10 gates the classifier on 100% JSON parsing
// precisely because this path is meant to be rare, which is exactly why losing data
// down it is the wrong trade.
func TestClassifyRetriesItemsAFailedTopicNeverJudged(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	good := seedTopic(t, db, "Agents", "Agent harnesses.")
	broken := seedTopic(t, db, "Broken", "A definition the model chokes on.")
	seedItem(t, db, src, "https://x.test/a", "A post about harnesses")

	failing := true
	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "TOPIC: Broken") && failing {
			return "I cannot do that."
		}
		return scoreAllHigh(prompt)
	})

	cfg := testConfig()
	first, err := Classify(context.Background(), db, clientFor(t, db, model), cfg, testRunDate, quietLogger())
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(first.Problems) == 0 {
		t.Fatal("the failing topic reported no problem")
	}

	// The good topic got its verdict, so its work is done.
	if statsFor(t, db, good).ItemsConsidered != 1 {
		t.Error("the healthy topic did not record its item")
	}

	// The item was never judged for Broken, so a second pass must reconsider it.
	cfg2, err := configFromDB(db)
	if err != nil {
		t.Fatal(err)
	}
	failing = false
	second, err := Classify(context.Background(), db, clientFor(t, db, model), cfg2, testRunDate, quietLogger())
	if err != nil {
		t.Fatalf("second Classify: %v", err)
	}
	if second.ItemsConsidered == 0 {
		t.Fatal("the second pass considered nothing; the item was abandoned for the topic that never judged it")
	}
	if statsFor(t, db, broken).ItemsConsidered == 0 {
		t.Error("the previously failing topic still has no verdict on that item")
	}
	if n := countRows(t, db, "SELECT count(*) FROM matches WHERE topic_id = ?", broken); n == 0 {
		t.Error("the recovered topic produced no match despite the model now working")
	}
}

// One failing topic must not blank the hit-rate view for the others.
//
// The first version of the watermark fix gated stats on the same prefix as the
// watermark, which meant a topic failing on the first slice erased every topic's
// numbers -- destroying the one diagnostic that would identify the failing topic.
func TestClassifyStillRecordsHealthyTopicsWhenOneFails(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	good := seedTopic(t, db, "Agents", "Agent harnesses.")
	alsoGood := seedTopic(t, db, "Open weights", "Open-weight releases.")
	broken := seedTopic(t, db, "Broken", "Chokes.")
	for i := range 3 {
		seedItem(t, db, src, fmt.Sprintf("https://x.test/%d", i), fmt.Sprintf("A post about harnesses %d", i))
	}

	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "TOPIC: Broken") {
			return "no."
		}
		return scoreAllHigh(prompt)
	})

	if _, err := Classify(context.Background(), db, clientFor(t, db, model),
		testConfig(), testRunDate, quietLogger()); err != nil {
		t.Fatalf("Classify: %v", err)
	}

	for _, id := range []int64{good, alsoGood} {
		if s := statsFor(t, db, id); s.ItemsConsidered != 3 {
			t.Errorf("topic %d considered %d, want 3 — a sibling's failure erased its numbers",
				id, s.ItemsConsidered)
		}
	}
	// And the failing one reads zero, which is what identifies it.
	if s := statsFor(t, db, broken); s.ItemsConsidered != 0 {
		t.Errorf("the failing topic considered %d, want 0", s.ItemsConsidered)
	}
}

// The committed-range counters are what make a stalled watermark visible.
func TestClassifyReportsHowMuchOfTheWindowItClaimed(t *testing.T) {
	db := testDB(t)
	src := seedSource(t, db, "Blog")
	seedTopic(t, db, "Agents", "Agent harnesses.")
	seedTopic(t, db, "Broken", "Chokes.")
	for i := range 3 {
		seedItem(t, db, src, fmt.Sprintf("https://x.test/%d", i), fmt.Sprintf("A post %d", i))
	}

	clean := newFakeModel(t, scoreAllHigh)
	full, err := Classify(context.Background(), db, clientFor(t, db, clean),
		testConfig(), testRunDate, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if full.RangesTotal == 0 || full.RangesCommitted != full.RangesTotal {
		t.Errorf("a clean pass committed %d of %d ranges, want all",
			full.RangesCommitted, full.RangesTotal)
	}
}
