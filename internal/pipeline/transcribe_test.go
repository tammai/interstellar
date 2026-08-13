package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tammai/interstellar/internal/store"
)

// seedVideo adds a youtube_rss source carrying one video item.
func seedVideo(t *testing.T, db *store.DB, url, title string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO sources (kind, name, config, active)
		 VALUES ('youtube_rss', ?, '{"channel_id":"UCtest"}', 1)`, "Channel "+title)
	if err != nil {
		t.Fatalf("insert youtube source: %v", err)
	}
	sourceID, _ := res.LastInsertId()

	id, _, err := db.UpsertItem(store.NewItem{
		SourceID: sourceID, CanonicalURL: url, Title: title, Summary: "Channel blurb.",
	})
	if err != nil {
		t.Fatalf("upsert video item: %v", err)
	}
	return id
}

// Only items from a youtube_rss source are candidates. Matching on URL shape would
// drag in every ordinary article that happens to link to YouTube.
func TestVideosNeedingTranscriptsPicksOnlyVideoSources(t *testing.T) {
	db := testDB(t)

	video := seedVideo(t, db, "https://www.youtube.com/watch?v=aaaaaaaaaaa", "A talk")

	// An article that merely links to YouTube, from an rss source.
	blog := seedSource(t, db, "Blog")
	seedItem(t, db, blog, "https://www.youtube.com/watch?v=bbbbbbbbbbb", "A blog post about a video")

	pending, err := db.VideosNeedingTranscripts(10)
	if err != nil {
		t.Fatalf("VideosNeedingTranscripts: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(pending), pending)
	}
	if pending[0].ItemID != video {
		t.Errorf("picked item %d, want the video %d", pending[0].ItemID, video)
	}
}

// A failure row counts as attempted. Without that, a video with captions disabled
// would be retried on every run forever — and there are a lot of those.
func TestTranscriptFailureStopsItBeingRetried(t *testing.T) {
	db := testDB(t)
	video := seedVideo(t, db, "https://www.youtube.com/watch?v=aaaaaaaaaaa", "A talk")

	if err := db.SaveTranscript(store.TranscriptRecord{
		ItemID: video, Status: store.TranscriptNone, Err: "no captions advertised",
	}); err != nil {
		t.Fatalf("SaveTranscript: %v", err)
	}

	pending, err := db.VideosNeedingTranscripts(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("a video with a recorded failure is still pending: %+v", pending)
	}

	counts, err := db.TranscriptCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[store.TranscriptNone] != 1 {
		t.Errorf("counts = %+v", counts)
	}
}

// The whole point of the stage: the summary has to reach items.summary, because
// that is the only field the classifier reads.
func TestSaveTranscriptFoldsTheSummaryIntoTheItem(t *testing.T) {
	db := testDB(t)
	video := seedVideo(t, db, "https://www.youtube.com/watch?v=aaaaaaaaaaa", "A talk")

	if err := db.SaveTranscript(store.TranscriptRecord{
		ItemID: video, Status: store.TranscriptOK,
		Text: "the full transcript text", Lang: "en",
		Summary: "The speaker argues retrieval beat context length.",
	}); err != nil {
		t.Fatalf("SaveTranscript: %v", err)
	}

	items, err := db.ItemsSince("", 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range items {
		if it.ID != video {
			continue
		}
		found = true
		if !strings.HasPrefix(it.Summary, "The speaker argues retrieval") {
			t.Errorf("summary = %q; the transcript summary must lead", it.Summary)
		}
		// The feed's own blurb is kept after it: it sometimes carries links and
		// chapter markers worth having.
		if !strings.Contains(it.Summary, "Channel blurb.") {
			t.Errorf("summary = %q; the feed description was discarded", it.Summary)
		}
	}
	if !found {
		t.Fatal("the video item is not in the classifier's queue")
	}

	// And it is searchable, which proves the items FTS update trigger fired.
	hits, err := db.Search(store.SearchQuery{Text: "retrieval context length", Kind: "items"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("the new summary is not in the search index; the items update trigger did not fire")
	}
}

// Re-saving must not accumulate the summary onto the item over and over.
func TestSaveTranscriptIsIdempotent(t *testing.T) {
	db := testDB(t)
	video := seedVideo(t, db, "https://www.youtube.com/watch?v=aaaaaaaaaaa", "A talk")

	rec := store.TranscriptRecord{
		ItemID: video, Status: store.TranscriptOK, Text: "text",
		Summary: "One summary sentence.",
	}
	if err := db.SaveTranscript(rec); err != nil {
		t.Fatal(err)
	}
	first := itemSummary(t, db, video)

	if err := db.SaveTranscript(rec); err != nil {
		t.Fatal(err)
	}
	second := itemSummary(t, db, video)

	if second != first {
		t.Errorf("saving the same record twice changed the item summary:\n  %q\n  %q", first, second)
	}
	if strings.Count(second, "One summary sentence.") != 1 {
		t.Errorf("summary accumulated across saves: %q", second)
	}

	// The harder case: a BETTER summary must replace the old one, not stack in
	// front of it. This is what re-running with an upgraded model does.
	rec.Summary = "A sharper summary from a better model."
	if err := db.SaveTranscript(rec); err != nil {
		t.Fatal(err)
	}
	third := itemSummary(t, db, video)
	if strings.Contains(third, "One summary sentence.") {
		t.Errorf("the superseded summary is still in the item: %q", third)
	}
	if !strings.HasPrefix(third, "A sharper summary") {
		t.Errorf("the new summary does not lead: %q", third)
	}
	// And the channel's own blurb survived all three writes.
	if !strings.Contains(third, "Channel blurb.") {
		t.Errorf("the original feed description was lost: %q", third)
	}

	if n := countRows(t, db, "SELECT count(*) FROM item_transcripts WHERE item_id = ?", video); n != 1 {
		t.Errorf("%d transcript rows for one item", n)
	}
}

func itemSummary(t *testing.T, db *store.DB, id int64) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT COALESCE(summary,'') FROM items WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The stage is off when the per-run budget is zero, and must not call anything.
func TestTranscribeIsOffAtZero(t *testing.T) {
	db := testDB(t)
	seedVideo(t, db, "https://www.youtube.com/watch?v=aaaaaaaaaaa", "A talk")

	model := newFakeModel(t, func(string) string { return "unused" })
	cfg := testConfig()
	cfg.TranscriptsPerRun = 0

	result, err := Transcribe(context.Background(), db, clientFor(t, db, model), cfg, quietLogger())
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if result.Considered != 0 {
		t.Errorf("considered %d videos with the stage off", result.Considered)
	}
	if model.callCount() != 0 {
		t.Errorf("model called %d times with the stage off", model.callCount())
	}
}

// A URL attributed to a YouTube source that is not a video is recorded permanently
// rather than retried, and must not spend a model call.
func TestTranscribeRecordsUnrecognisableURLsAsPermanent(t *testing.T) {
	db := testDB(t)
	seedVideo(t, db, "https://www.youtube.com/@somechannel", "A channel page")

	model := newFakeModel(t, func(string) string { return "unused" })
	cfg := testConfig()
	cfg.TranscriptsPerRun = 5

	result, err := Transcribe(context.Background(), db, clientFor(t, db, model), cfg, quietLogger())
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if result.NoCaptions != 1 {
		t.Errorf("NoCaptions = %d, want 1", result.NoCaptions)
	}
	if model.callCount() != 0 {
		t.Errorf("model called %d times for a non-video URL", model.callCount())
	}

	pending, _ := db.VideosNeedingTranscripts(10)
	if len(pending) != 0 {
		t.Error("a non-video URL will be reconsidered on the next run")
	}
}

func TestIsMissingCaptionsSeparatesPermanentFromRetryable(t *testing.T) {
	permanent := []string{
		"no captions advertised on the watch page",
		"no caption tracks",
		"no usable caption track",
		"caption track was empty",
	}
	for _, msg := range permanent {
		if !isMissingCaptions(fmt.Errorf("%s", msg)) {
			t.Errorf("%q should be permanent", msg)
		}
	}
	retryable := []string{
		"watch page returned 429 Too Many Requests",
		"caption track list did not parse: unexpected end of JSON",
		"dial tcp: connection refused",
	}
	for _, msg := range retryable {
		if isMissingCaptions(fmt.Errorf("%s", msg)) {
			t.Errorf("%q should be retryable, not permanent", msg)
		}
	}
}

// The full stage against a stubbed caption path: fetch, summarize, fold into the
// item, and make it searchable — with the transcript summary reaching the exact
// field the classifier reads.
func TestTranscribeEndToEndWithStubbedCaptions(t *testing.T) {
	db := testDB(t)

	// Point the caption fetch at a local stub by overriding the package client's
	// transport, which is the only seam that does not require reaching youtube.com.
	stub := newCaptionStub(t)
	defer stub.restore()

	video := seedVideo(t, db, "https://www.youtube.com/watch?v=aaaaaaaaaaa",
		"How retrieval replaced the context window")

	model := newFakeModel(t, func(prompt string) string {
		if !strings.Contains(prompt, "TRANSCRIPT:") {
			t.Errorf("the summarizer prompt carries no transcript:\n%s", prompt)
		}
		if !strings.Contains(prompt, "retrieval beats a bigger window") {
			t.Error("the summarizer prompt does not contain the fetched caption text")
		}
		return "The talk argues retrieval beats a bigger context window for long documents."
	})

	cfg := testConfig()
	cfg.TranscriptsPerRun = 5

	result, err := Transcribe(context.Background(), db, clientFor(t, db, model), cfg, quietLogger())
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if result.Fetched != 1 || result.Summarized != 1 {
		t.Fatalf("fetched=%d summarized=%d, want 1 and 1 (problems: %v)",
			result.Fetched, result.Summarized, result.Problems)
	}

	// The summary reached items.summary, which is the only field the classifier sees.
	summary := itemSummary(t, db, video)
	if !strings.HasPrefix(summary, "The talk argues retrieval") {
		t.Errorf("item summary = %q", summary)
	}

	// Logged under its own role, so the cost is visible rather than blended into
	// classification's line in the cost log.
	if n := countRows(t, db, `SELECT count(*) FROM llm_calls WHERE role = 'transcript'`); n != 1 {
		t.Errorf("%d calls logged as transcript, want 1", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM llm_calls WHERE role = 'classifier'`); n != 0 {
		t.Errorf("%d calls billed to the classifier; the transcript role must own its cost", n)
	}

	// The transcript text itself is stored, so a re-run never refetches.
	if n := countRows(t, db,
		`SELECT count(*) FROM item_transcripts WHERE item_id = ? AND status = 'ok' AND chars > 0`,
		video); n != 1 {
		t.Error("the transcript text was not stored")
	}
	pending, _ := db.VideosNeedingTranscripts(10)
	if len(pending) != 0 {
		t.Error("the video is still pending after a successful transcript")
	}
}

// A transcript fetched but not summarized — which is what a cap breach leaves
// behind — must still be picked up next run. The anti-join on item_transcripts
// excludes any item with a row at all, so a capped run would lose that video's
// summary permanently.
func TestVideosNeedingTranscriptsPicksUpUnsummarizedTranscripts(t *testing.T) {
	db := testDB(t)
	video := seedVideo(t, db, "https://www.youtube.com/watch?v=aaaaaaaaaaa", "A talk")

	// Exactly the state Transcribe leaves on a cap breach: text stored, no summary.
	if err := db.SaveTranscript(store.TranscriptRecord{
		ItemID: video, Status: store.TranscriptOK, Text: "the captions", Lang: "en",
	}); err != nil {
		t.Fatalf("SaveTranscript: %v", err)
	}

	pending, err := db.VideosNeedingTranscripts(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want 1 — a fetched-but-unsummarized video is unfinished work",
			len(pending))
	}

	// And once it has a summary it is genuinely done.
	if err := db.SaveTranscript(store.TranscriptRecord{
		ItemID: video, Status: store.TranscriptOK, Text: "the captions",
		Lang: "en", Summary: "A summary.",
	}); err != nil {
		t.Fatal(err)
	}
	pending, _ = db.VideosNeedingTranscripts(10)
	if len(pending) != 0 {
		t.Errorf("a summarized video is still pending: %+v", pending)
	}

	// A permanent 'none' must NOT come back, or captions-disabled videos retry forever.
	other := seedVideo(t, db, "https://www.youtube.com/watch?v=bbbbbbbbbbb", "No captions here")
	if err := db.SaveTranscript(store.TranscriptRecord{
		ItemID: other, Status: store.TranscriptNone, Err: "no captions advertised",
	}); err != nil {
		t.Fatal(err)
	}
	pending, _ = db.VideosNeedingTranscripts(10)
	for _, p := range pending {
		if p.ItemID == other {
			t.Error("a video with no captions is pending again; it will be refetched forever")
		}
	}
}

// The inbox is identified by its config marker, not its name. Renaming it in
// Settings — which the owner is free to do — must not create a second one, leaving
// the first holding every link saved before the rename.
func TestManualSourceSurvivesARename(t *testing.T) {
	db := testDB(t)

	first, err := db.ManualSourceID()
	if err != nil {
		t.Fatalf("ManualSourceID: %v", err)
	}

	src, err := db.SourceByID(first)
	if err != nil {
		t.Fatal(err)
	}
	src.Name = "My saved bits"
	if err := db.UpdateSource(src); err != nil {
		t.Fatalf("UpdateSource: %v", err)
	}

	again, err := db.ManualSourceID()
	if err != nil {
		t.Fatalf("ManualSourceID after rename: %v", err)
	}
	if again != first {
		t.Errorf("got source %d after a rename, want the original %d — a second inbox was created",
			again, first)
	}

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sources WHERE config = '{"manual":true}'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d inbox sources exist, want 1", n)
	}
}
