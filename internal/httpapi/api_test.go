package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/pipeline"
	"github.com/tammai/interstellar/internal/store"
	"github.com/tammai/interstellar/seed"
)

// providerKeyEnvs is every environment variable the shipped presets reference.
var providerKeyEnvs = []string{
	"DEEPSEEK_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY",
	"OPENROUTER_API_KEY", "LITELLM_API_KEY",
}

// isolateFromRealKeys clears provider credentials for the duration of a test.
//
// Without this the suite is not hermetic: a developer with DEEPSEEK_API_KEY
// exported in their shell would have these tests resolve a real provider and, in
// the case of the test-call endpoint, actually bill their account. Tests must
// never depend on -- or spend -- the ambient environment. The only thing allowed
// to make a live call is cmd/verify-classifier, which is opt-in.
func isolateFromRealKeys(t *testing.T) {
	t.Helper()
	for _, name := range providerKeyEnvs {
		t.Setenv(name, "")
	}
}

func testServer(t *testing.T) (*Server, http.Handler, *store.DB) {
	t.Helper()
	isolateFromRealKeys(t)

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	env := config.Env{Port: "8422", DBPath: db.Path, Location: time.UTC}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := pipeline.NewRunner(db, env, log)
	// A run a test starts may schedule a catch-up; it must not outlive the test's
	// database.
	t.Cleanup(runner.Stop)
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>sky</html>")}}

	srv := New(db, env, web, runner, nil, log)
	return srv, srv.Routes(), db
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	return v
}

// The single most important assertion in this package. A provider key must never
// appear in any response, on any endpoint, under any shape.
func TestNoEndpointEverLeaksAKey(t *testing.T) {
	_, h, _ := testServer(t)

	// Set AFTER testServer: it clears provider keys, and a secret set before it
	// would be wiped -- making this test pass without proving anything.
	const secret = "sk-super-secret-value-do-not-leak"
	t.Setenv("DEEPSEEK_API_KEY", secret)

	paths := []struct{ method, path, body string }{
		{"GET", "/api/health", ""},
		{"GET", "/api/settings", ""},
		{"GET", "/api/status", ""},
		{"GET", "/api/sky", ""},
		{"GET", "/api/topics", ""},
		{"GET", "/api/topics/stats", ""},
		{"GET", "/api/models/stats", ""},
		{"GET", "/api/trending", ""},
		{"GET", "/api/weekly", ""},
		{"GET", "/api/search?q=agents", ""},
		{"GET", "/api/chat/history", ""},
		{"POST", "/api/ask", `{"question":"anything?"}`},
		{"GET", "/api/sources", ""},
		{"GET", "/api/sources/export", ""},
		{"PUT", "/api/settings", `{"classifier_model":"deepseek/deepseek-chat"}`},
	}

	for _, p := range paths {
		w := do(t, h, p.method, p.path, p.body)
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("%s %s leaked the API key value", p.method, p.path)
		}
		for name, values := range w.Header() {
			for _, v := range values {
				if strings.Contains(v, secret) {
					t.Errorf("%s %s leaked the key in header %s", p.method, p.path, name)
				}
			}
		}
	}
}

// Settings must carry the env var NAME and whether it resolves -- that is all the
// UI badge needs, and it is safe.
func TestSettingsExposesEnvNameAndResolvedFlagOnly(t *testing.T) {
	_, h, _ := testServer(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-present") // after testServer; see above

	w := do(t, h, "GET", "/api/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := decode[settingsResponse](t, w)

	var deepseek, ollama *providerDTO
	for i := range got.Providers {
		switch got.Providers[i].Name {
		case "deepseek":
			deepseek = &got.Providers[i]
		case "ollama":
			ollama = &got.Providers[i]
		}
	}
	if deepseek == nil || ollama == nil {
		t.Fatalf("shipped presets missing: %+v", got.Providers)
	}

	if deepseek.APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Errorf("api_key_env = %q", deepseek.APIKeyEnv)
	}
	if !deepseek.Resolved {
		t.Error("deepseek should resolve; the variable is set")
	}
	if ollama.Keyless != true || ollama.Resolved != true {
		t.Errorf("ollama should be keyless and considered resolved: %+v", ollama)
	}

	// An unset provider must report unresolved rather than silently look fine.
	for _, p := range got.Providers {
		if p.Name == "anthropic" && p.Resolved {
			t.Error("anthropic reported resolved with no ANTHROPIC_API_KEY set")
		}
	}
}

// A settings edit that would break the next run must be rejected at save time,
// not discovered three minutes into a refresh.
func TestSettingsRejectsBreakingEdits(t *testing.T) {
	_, h, db := testServer(t)

	before := do(t, h, "GET", "/api/settings", "")
	original := decode[settingsResponse](t, before)

	for name, body := range map[string]string{
		"model ref with no provider": `{"classifier_model":"just-a-model-name"}`,
		"unparseable fallback":       `{"classifier_fallback":["no-slash"]}`,
		"bad schedule":               `{"schedule_ingest":"9am"}`,
		"impossible hour":            `{"schedule_ingest":"25:00"}`,
		"provider with no base url":  `{"providers":[{"name":"x","base_url":"","api_key_env":"K"}]}`,
		"duplicate provider":         `{"providers":[{"name":"x","base_url":"http://a"},{"name":"x","base_url":"http://b"}]}`,
	} {
		w := do(t, h, "PUT", "/api/settings", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
	}

	// And nothing was persisted.
	after := decode[settingsResponse](t, do(t, h, "GET", "/api/settings", ""))
	if after.ClassifierModel != original.ClassifierModel {
		t.Errorf("a rejected edit still changed the classifier model: %q", after.ClassifierModel)
	}
	if v, _, _ := db.Setting(store.KeyScheduleIngest); v != "09:30" {
		t.Errorf("schedule changed to %q despite rejection", v)
	}
}

func TestSettingsAcceptsValidEdit(t *testing.T) {
	_, h, db := testServer(t)

	w := do(t, h, "PUT", "/api/settings",
		`{"classifier_model":"ollama/llama3","max_calls_per_run":42,"schedule_ingest":"07:15"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	got := decode[settingsResponse](t, w)
	if got.ClassifierModel != "ollama/llama3" {
		t.Errorf("classifier_model = %q", got.ClassifierModel)
	}
	if got.MaxCallsPerRun != 42 {
		t.Errorf("max_calls_per_run = %d", got.MaxCallsPerRun)
	}

	// A partial update must not clear the fields it didn't mention.
	if got.SynthesizerModel == "" {
		t.Error("a partial update wiped the synthesizer model")
	}

	if v, _, _ := db.Setting(store.KeyScheduleIngest); v != "07:15" {
		t.Errorf("schedule not persisted: %q", v)
	}
}

// A typo'd field name should be an error, not a silently ignored save.
func TestSettingsRejectsUnknownFields(t *testing.T) {
	_, h, _ := testServer(t)
	w := do(t, h, "POST", "/api/topics", `{"titel":"typo"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field", w.Code)
	}
}

func TestSkyReturnsTopicsAndSetupState(t *testing.T) {
	_, h, _ := testServer(t)

	w := do(t, h, "GET", "/api/sky", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := decode[skyResponse](t, w)

	want, err := store.ParseSeed(seed.Default())
	if err != nil {
		t.Fatalf("default seed: %v", err)
	}
	if len(got.Topics) != len(want.Topics) {
		t.Errorf("got %d topics, want the %d seeded", len(got.Topics), len(want.Topics))
	}
	undefined := map[string]bool{}
	for _, st := range want.Topics {
		undefined[st.Title] = strings.TrimSpace(st.Definition) == ""
	}
	if got.Signals == nil {
		t.Error("signals should be an empty array, not null -- the UI iterates it")
	}
	if got.HotThreshold != 3 {
		t.Errorf("hot_threshold = %d, want 3", got.HotThreshold)
	}
	// Every seeded topic has a place on the sky, and needs_setup says exactly
	// whether the seed gave it a definition.
	for _, tp := range got.Topics {
		if tp.PosX == 0 && tp.PosY == 0 {
			t.Errorf("topic %q has no constellation position", tp.Title)
		}
		if tp.NeedsSetup != undefined[tp.Title] {
			t.Errorf("topic %q needs_setup = %v, want %v", tp.Title, tp.NeedsSetup, undefined[tp.Title])
		}
	}
}

func TestStatusReportsReadinessAndSetupHint(t *testing.T) {
	_, h, _ := testServer(t)

	got := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))

	if got.Ready {
		t.Error("a fresh instance with no API key should not be ready")
	}
	if got.SetupHint == "" {
		t.Error("no setup hint on a fresh instance")
	}
	// The hint must name the specific blocker.
	if !strings.Contains(got.SetupHint, "OPENROUTER_API_KEY") {
		t.Errorf("setup hint should name the missing variable, got %q", got.SetupHint)
	}
	if got.Run.Stage != "idle" {
		t.Errorf("stage = %q, want idle", got.Run.Stage)
	}
}

// The settings popover lists Topics and Sources with a count beside each. It
// used to render an em dash on both rows forever: the rows were hardcoded, and
// the field the JS reached for was never in this payload to begin with.
func TestStatusReportsTopicAndSourceCounts(t *testing.T) {
	_, h, db := testServer(t)

	topics, err := db.AllTopics()
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := db.AllSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) == 0 || len(srcs) == 0 {
		t.Fatal("precondition: bootstrap should seed both topics and sources")
	}

	got := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))

	if got.TopicsCount != len(topics) {
		t.Errorf("topics_count = %d, want %d", got.TopicsCount, len(topics))
	}
	if got.SourcesCount != len(srcs) {
		t.Errorf("sources_count = %d, want %d", got.SourcesCount, len(srcs))
	}

	// Zero has to survive serialization: an omitted field reads as unknown in
	// the UI and renders as the same em dash this replaced.
	raw := do(t, h, "GET", "/api/status", "").Body.String()
	for _, key := range []string{`"topics_count"`, `"sources_count"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("%s missing from the payload: %s", key, raw)
		}
	}
}

// A count that does not move when the underlying list does is worse than no
// count, because it looks authoritative.
func TestStatusCountsFollowTopicChanges(t *testing.T) {
	_, h, _ := testServer(t)

	before := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))

	do(t, h, "POST", "/api/topics", `{"title":"Extra topic","definition":"something"}`)

	after := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))
	if after.TopicsCount != before.TopicsCount+1 {
		t.Errorf("topics_count = %d after adding one to %d", after.TopicsCount, before.TopicsCount)
	}
	if after.SourcesCount != before.SourcesCount {
		t.Errorf("sources_count moved on a topic change: %d -> %d", before.SourcesCount, after.SourcesCount)
	}
}

// With the key set, the remaining blocker is topic definitions -- and the hint
// should move on to that rather than repeat itself.
func TestStatusHintAdvancesToTopics(t *testing.T) {
	_, h, db := testServer(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-present") // after testServer; see above
	// One topic without a definition, whatever the seed ships, so the hint has
	// something to advance to.
	if _, err := db.Exec(`UPDATE topics SET definition = '' WHERE id = (SELECT min(id) FROM topics)`); err != nil {
		t.Fatal(err)
	}

	got := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))
	if !got.Ready {
		t.Fatalf("should be model-ready with the key set: %+v", got.Readiness)
	}
	if !strings.Contains(got.SetupHint, "definition") {
		t.Errorf("hint should now be about topic definitions, got %q", got.SetupHint)
	}
}

// §10 wants the cap breach visible. Derived from llm_calls, so it survives a
// restart that clears in-memory run state.
func TestStatusBannerSurvivesRestart(t *testing.T) {
	_, h, db := testServer(t)

	if err := db.SetSetting(store.KeyMaxCallsPerDay, "2"); err != nil {
		t.Fatal(err)
	}
	runDate := store.RunDate(time.Now().In(time.UTC))
	for i := 0; i < 2; i++ {
		if _, err := db.RecordLLMCall(store.LLMCall{
			RunDate: runDate, Role: "classifier", Provider: "deepseek", Model: "m", OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A brand-new server, as after a restart: nothing in memory.
	got := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))
	if got.Banner == "" {
		t.Error("no banner after the daily cap was reached")
	}
	if !strings.Contains(got.Banner, "Daily cap") {
		t.Errorf("banner = %q", got.Banner)
	}
	if got.CallsToday != 2 {
		t.Errorf("calls_today = %d, want 2", got.CallsToday)
	}
}

func TestTopicCRUD(t *testing.T) {
	_, h, _ := testServer(t)

	created := do(t, h, "POST", "/api/topics",
		`{"title":"Voice AI","definition":"Streaming STT and TTS latency and cost."}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", created.Code, created.Body.String())
	}
	topic := decode[topicDTO](t, created)
	if topic.ID == 0 {
		t.Fatal("created topic has no id")
	}
	if topic.NeedsSetup {
		t.Error("a topic created with a definition should not need setup")
	}
	// A new topic must get its own patch of sky.
	if topic.PosX == 0 && topic.PosY == 0 {
		t.Error("created topic has no constellation position")
	}

	updated := do(t, h, "PATCH", "/api/topics/"+itoa(topic.ID), `{"definition":"Revised."}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update: status = %d", updated.Code)
	}
	if got := decode[topicDTO](t, updated); got.Definition != "Revised." {
		t.Errorf("definition = %q", got.Definition)
	}

	if w := do(t, h, "PATCH", "/api/topics/"+itoa(topic.ID), `{"title":"  "}`); w.Code != http.StatusBadRequest {
		t.Errorf("blank title: status = %d, want 400", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/topics/"+itoa(topic.ID), ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/topics/"+itoa(topic.ID), ""); w.Code != http.StatusNotFound {
		t.Errorf("second delete: status = %d, want 404", w.Code)
	}
}

// Adding a topic must not move the constellations already in the sky (§6).
func TestNewTopicDoesNotMoveExistingConstellations(t *testing.T) {
	_, h, _ := testServer(t)

	before := decode[skyResponse](t, do(t, h, "GET", "/api/sky", ""))
	positions := map[int64][2]float64{}
	for _, tp := range before.Topics {
		positions[tp.ID] = [2]float64{tp.PosX, tp.PosY}
	}

	if w := do(t, h, "POST", "/api/topics", `{"title":"New","definition":"d"}`); w.Code != http.StatusCreated {
		t.Fatalf("create: %d", w.Code)
	}

	after := decode[skyResponse](t, do(t, h, "GET", "/api/sky", ""))
	for _, tp := range after.Topics {
		want, existed := positions[tp.ID]
		if !existed {
			continue
		}
		if tp.PosX != want[0] || tp.PosY != want[1] {
			t.Errorf("topic %q moved from (%v,%v) to (%v,%v); spatial memory is broken",
				tp.Title, want[0], want[1], tp.PosX, tp.PosY)
		}
	}
}

func TestSourceCRUDValidatesKind(t *testing.T) {
	_, h, _ := testServer(t)

	w := do(t, h, "POST", "/api/sources",
		`{"kind":"carrier_pigeon","name":"Nope","config":{}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown kind: status = %d, want 400", w.Code)
	}

	created := do(t, h, "POST", "/api/sources",
		`{"kind":"rss","name":"My Blog","config":{"url":"https://example.test/feed"}}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", created.Code, created.Body.String())
	}
	src := decode[sourceDTO](t, created)

	if w := do(t, h, "PATCH", "/api/sources/"+itoa(src.ID), `{"active":false}`); w.Code != http.StatusOK {
		t.Errorf("deactivate: status = %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/sources/"+itoa(src.ID), ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: status = %d", w.Code)
	}
}

func TestSignalNotFound(t *testing.T) {
	_, h, _ := testServer(t)
	if w := do(t, h, "GET", "/api/signals/999", ""); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if w := do(t, h, "GET", "/api/signals/abc", ""); w.Code != http.StatusBadRequest {
		t.Errorf("non-numeric id: status = %d, want 400", w.Code)
	}
}

// A second refresh while one is running is 409, not an error the UI has to
// explain -- it keeps showing progress instead.
func TestRefreshRejectsConcurrentRuns(t *testing.T) {
	srv, h, db := testServer(t)

	// Point every source at a server that never answers, so the run stays in
	// flight while the second request arrives.
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() { close(block); slow.Close() }()

	if _, err := db.Exec(`UPDATE sources SET config = ?, kind = 'rss'`,
		`{"url":"`+slow.URL+`"}`); err != nil {
		t.Fatal(err)
	}

	if w := do(t, h, "POST", "/api/refresh", ""); w.Code != http.StatusAccepted {
		t.Fatalf("first refresh: status = %d", w.Code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Runner.State().Running() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !srv.Runner.State().Running() {
		t.Skip("run finished before the second request could race it")
	}

	if w := do(t, h, "POST", "/api/refresh", ""); w.Code != http.StatusConflict {
		t.Errorf("second refresh: status = %d, want 409", w.Code)
	}

	srv.Runner.Stop()
}

func TestAPIResponsesAreNotCached(t *testing.T) {
	_, h, _ := testServer(t)

	w := do(t, h, "GET", "/api/sky", "")
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store; a cached sky looks like a broken pipeline", got)
	}

	// Static assets stay cacheable.
	if w := do(t, h, "GET", "/", ""); w.Header().Get("Cache-Control") == "no-store" {
		t.Error("static assets should not be no-store")
	}
}

func TestModelTestRejectsUnknownRole(t *testing.T) {
	_, h, _ := testServer(t)
	if w := do(t, h, "POST", "/api/model/test", `{"role":"nonsense"}`); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// A failing provider is a successful diagnosis: 200 with ok=false and a reason,
// because the UI wants to render the reason inline.
func TestModelTestReportsFailureAsDiagnosis(t *testing.T) {
	_, h, _ := testServer(t)

	// No key is set (see isolateFromRealKeys), so the call cannot be attempted
	// and the reason should name the variable rather than the network.
	w := do(t, h, "POST", "/api/model/test", `{"role":"classifier"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := decode[testResponse](t, w)
	if got.OK {
		t.Error("test call reported success with no API key set")
	}
	if got.Error == "" {
		t.Error("no reason given for the failure")
	}
	if got.Provider != "openrouter" {
		t.Errorf("provider = %q, want the configured one", got.Provider)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// --- per-topic hit rates ---

func TestTopicStatsReportsHitRatePerTopic(t *testing.T) {
	_, h, db := testServer(t)

	topics, err := db.AllTopics()
	if err != nil || len(topics) < 2 {
		t.Fatalf("need seeded topics: %d, %v", len(topics), err)
	}
	day := store.RunDate(time.Now())
	if err := db.CommitClassifyPass(day, []store.TopicStat{
		{TopicID: topics[0].ID, ItemsConsidered: 40, MatchesKept: 10, Batches: 2},
	}, nil); err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}

	w := do(t, h, "GET", "/api/topics/stats", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := decode[topicStatsResponse](t, w)

	if got.Days != 7 {
		t.Errorf("days = %d, want the 7-day default", got.Days)
	}
	if len(got.Rows) != len(topics) {
		t.Errorf("got %d rows, want %d — every topic appears, classified or not",
			len(got.Rows), len(topics))
	}
	if got.ItemsConsidered != 40 || got.MatchesKept != 10 {
		t.Errorf("totals considered=%d matched=%d, want 40/10", got.ItemsConsidered, got.MatchesKept)
	}
	if got.HitRate != 0.25 {
		t.Errorf("overall hit rate = %v, want 0.25", got.HitRate)
	}
	if !got.Ready {
		t.Error("ready should be true once anything has been classified")
	}

	var classified, untouched int
	for _, row := range got.Rows {
		if row.HitRate < 0 {
			untouched++
			continue
		}
		classified++
		if row.TopicID != topics[0].ID {
			t.Errorf("topic %d reports a hit rate but was never classified", row.TopicID)
		}
	}
	if classified != 1 {
		t.Errorf("%d topics report a hit rate, want 1", classified)
	}
	if untouched != len(topics)-1 {
		t.Errorf("%d topics report unknown, want %d", untouched, len(topics)-1)
	}
}

// A fresh instance has classified nothing. The view must say so rather than
// showing an instance-wide 0%, which reads as "the classifier matches nothing".
func TestTopicStatsSaysUnknownBeforeAnyRun(t *testing.T) {
	_, h, _ := testServer(t)

	got := decode[topicStatsResponse](t, do(t, h, "GET", "/api/topics/stats", ""))
	if got.Ready {
		t.Error("ready is true on an instance that has never classified anything")
	}
	if got.HitRate != -1 {
		t.Errorf("hit rate = %v, want the -1 unknown sentinel", got.HitRate)
	}
	if len(got.Rows) == 0 {
		t.Error("no rows: the seeded topics should still be listed")
	}
}

func TestTopicStatsWindowIsSelectableAndValidated(t *testing.T) {
	_, h, db := testServer(t)

	topics, _ := db.AllTopics()
	old := store.RunDate(time.Now().AddDate(0, 0, -20))
	if err := db.CommitClassifyPass(old, []store.TopicStat{
		{TopicID: topics[0].ID, ItemsConsidered: 100, MatchesKept: 50, Batches: 4},
	}, nil); err != nil {
		t.Fatalf("CommitClassifyPass: %v", err)
	}

	week := decode[topicStatsResponse](t, do(t, h, "GET", "/api/topics/stats?days=7", ""))
	if week.ItemsConsidered != 0 {
		t.Errorf("7-day window considered %d, want 0: a 20-day-old pass leaked in",
			week.ItemsConsidered)
	}

	month := decode[topicStatsResponse](t, do(t, h, "GET", "/api/topics/stats?days=30", ""))
	if month.Days != 30 || month.ItemsConsidered != 100 {
		t.Errorf("30-day window: days=%d considered=%d, want 30/100",
			month.Days, month.ItemsConsidered)
	}

	for _, bad := range []string{"0", "-3", "400", "seven", "7.5"} {
		w := do(t, h, "GET", "/api/topics/stats?days="+bad, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("days=%s returned %d, want 400", bad, w.Code)
		}
	}
}

// --- search ---

// seedSearchable gives the test server something to find. Signals go in directly
// rather than through the pipeline: this endpoint has nothing to do with models,
// and a fixture that needed one would be testing the wrong thing.
func seedSearchable(t *testing.T, db *store.DB) int64 {
	t.Helper()
	topics, err := db.AllTopics()
	if err != nil || len(topics) == 0 {
		t.Fatalf("need seeded topics: %v", err)
	}
	topic := topics[0].ID
	for _, s := range []struct{ date, title, reason string }{
		{"2026-08-01", "Agent harness patterns converge", "Three outlets described the same shift."},
		{"2026-08-02", "Tiếng Việt trong bản ghi", "Bản ghi có dấu."},
	} {
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (?, ?, ?, ?, 5, 3, 'https://x.test/a')`, s.date, topic, s.title, s.reason,
		); err != nil {
			t.Fatalf("seed signal: %v", err)
		}
	}
	return topic
}

func TestSearchFindsSignalsWithSnippets(t *testing.T) {
	_, h, db := testServer(t)
	seedSearchable(t, db)

	got := decode[searchResponse](t, do(t, h, "GET", "/api/search?q=harness&kind=signals", ""))
	if got.Count == 0 {
		t.Fatal("no hits for a term that is present")
	}
	if got.Hits[0].Kind != "signal" {
		t.Errorf("kind = %q", got.Hits[0].Kind)
	}
	if got.Hits[0].Snippet == "" {
		t.Error("no snippet on a text match")
	}
	if got.Hits[0].TopicTitle == "" {
		t.Error("hit is missing its topic title")
	}
}

// The endpoint takes free prose straight from a text box, so the FTS5 operator
// grammar must never reach the query planner.
func TestSearchSurvivesHostileQueryStrings(t *testing.T) {
	_, h, db := testServer(t)
	seedSearchable(t, db)

	for _, q := range []string{
		"what's+happening+with+agents%3F", "AND+OR+NOT", "%22unterminated", "harness*",
		"NEAR(a+b%2C+3)", "%5Eanchored", "c%2B%2B", "'%3B+DROP+TABLE+signals%3B+--", "",
	} {
		w := do(t, h, "GET", "/api/search?q="+q, "")
		if w.Code != http.StatusOK {
			t.Errorf("q=%q returned %d: %s", q, w.Code, w.Body.String())
		}
	}

	// And the data is still there.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM signals`).Scan(&n); err != nil || n == 0 {
		t.Errorf("signals table damaged: n=%d err=%v", n, err)
	}
}

func TestSearchValidatesItsParameters(t *testing.T) {
	_, h, _ := testServer(t)

	for name, path := range map[string]string{
		"bad kind":   "/api/search?kind=everything",
		"zero limit": "/api/search?limit=0",
		"neg limit":  "/api/search?limit=-5",
		"word limit": "/api/search?limit=ten",
		"bad from":   "/api/search?from=2026-8-1",
		"bad to":     "/api/search?to=yesterday",
		"bad topic":  "/api/search?topic_id=abc",
	} {
		w := do(t, h, "GET", path, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s (%s): status %d, want 400", name, path, w.Code)
		}
	}
}

func TestSearchFiltersAndDiacritics(t *testing.T) {
	_, h, db := testServer(t)
	topic := seedSearchable(t, db)

	// Plain-keyboard query against accented text.
	viet := decode[searchResponse](t, do(t, h, "GET", "/api/search?q=tieng+viet&kind=signals", ""))
	if viet.Count == 0 {
		t.Error("diacritic-insensitive search found nothing")
	}

	// Date window.
	day := decode[searchResponse](t, do(t, h, "GET", "/api/search?kind=signals&from=2026-08-02&to=2026-08-02", ""))
	if day.Count == 0 {
		t.Fatal("date-filtered search found nothing")
	}
	for _, hit := range day.Hits {
		if hit.Date != "2026-08-02" {
			t.Errorf("date filter leaked %s", hit.Date)
		}
	}

	// Topic filter.
	byTopic := decode[searchResponse](t, do(t, h, "GET",
		fmt.Sprintf("/api/search?kind=signals&topic_id=%d", topic), ""))
	if byTopic.Count == 0 {
		t.Fatal("topic-filtered search found nothing")
	}
	for _, hit := range byTopic.Hits {
		if hit.TopicID != topic {
			t.Errorf("topic filter leaked topic %d", hit.TopicID)
		}
	}
}

// --- chat ---

// chatModelServer plays both prompts a question makes.
func chatModelServer(t *testing.T, plan, answer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		var prompt string
		for _, m := range body.Messages {
			prompt += m.Content
		}
		reply := answer
		if strings.Contains(prompt, "search plan") {
			reply = plan
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
			"usage":   map[string]int{"prompt_tokens": 40, "completion_tokens": 15},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useFakeChatModel points every role at a local fake, so nothing real is called.
func useFakeChatModel(t *testing.T, db *store.DB, srv *httptest.Server) {
	t.Helper()
	if err := db.SetSettings(map[string]string{
		store.KeyProviders:        fmt.Sprintf(`[{"name":"fake","base_url":%q,"api_key_env":""}]`, srv.URL),
		store.KeyClassifierModel:  "fake/classifier",
		store.KeySynthesizerModel: "fake/synth",
		store.KeyChatModel:        "fake/chat",
	}); err != nil {
		t.Fatalf("settings: %v", err)
	}
}

func TestAskAnswersAndPersistsTheTurn(t *testing.T) {
	_, h, db := testServer(t)
	seedSearchable(t, db)
	useFakeChatModel(t, db, chatModelServer(t,
		`{"text":"agent harness","kind":"signals","rationale":"the harness signal"}`,
		"Harness patterns converged across three outlets [1]."))

	w := do(t, h, "POST", "/api/ask", `{"question":"what happened with agent harnesses?"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := decode[askResponse](t, w)

	if got.Answer == "" {
		t.Error("no answer")
	}
	if len(got.Hits) == 0 {
		t.Error("no hits returned; the page has nothing to cite")
	}
	if !got.Planned {
		t.Error("planned is false despite a good plan")
	}
	if got.Query.Text != "agent harness" {
		t.Errorf("plan not echoed back: %+v", got.Query)
	}
	if got.Calls != 2 {
		t.Errorf("calls = %d, want 2", got.Calls)
	}
	if got.TurnID == 0 {
		t.Error("turn was not persisted")
	}
	if got.ChatCapDay == 0 {
		t.Error("chat cap not reported; the screen cannot show what is left")
	}
	if got.ChatCallsToday != 2 {
		t.Errorf("chat calls today = %d, want 2", got.ChatCallsToday)
	}

	// And it comes back in history.
	hist := decode[chatHistoryResponse](t, do(t, h, "GET", "/api/chat/history", ""))
	if len(hist.Turns) != 1 {
		t.Fatalf("history has %d turns, want 1", len(hist.Turns))
	}
	if hist.Turns[0].Question != "what happened with agent harnesses?" {
		t.Errorf("history question = %q", hist.Turns[0].Question)
	}
	if hist.Turns[0].HitCount == 0 {
		t.Error("history turn records no hits")
	}
	if !hist.Ready {
		t.Error("history reports chat as not ready despite a configured model")
	}
}

// The single most important interaction between chat and the rest of the app: a
// chat role that cannot run must not stop the pipeline. run.go refuses to start a
// run when Ready() is false, so folding chat into it would mean an owner who
// cleared chat_model silently broke their morning ingest.
func TestUnreadyChatDoesNotBlockThePipeline(t *testing.T) {
	_, h, db := testServer(t)
	if err := db.SetSettings(map[string]string{store.KeyChatModel: ""}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "sk-present") // after testServer; the pipeline roles resolve

	st := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))
	if !st.Ready {
		t.Errorf("instance reports not ready with only chat unconfigured; readiness = %+v", st.Readiness)
	}
	for _, role := range st.Readiness {
		if role.Role == "chat" {
			t.Error("chat appears in the pipeline's readiness list, which gates runs")
		}
	}
}

func TestAskRefusesWhenTheChatModelIsUnset(t *testing.T) {
	_, h, db := testServer(t)
	if err := db.SetSettings(map[string]string{store.KeyChatModel: ""}); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "POST", "/api/ask", `{"question":"anything?"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when chat has no model", w.Code)
	}

	// And history says why, so the page can show a hint instead of failing blind.
	hist := decode[chatHistoryResponse](t, do(t, h, "GET", "/api/chat/history", ""))
	if hist.Ready {
		t.Error("history claims chat is ready with no model set")
	}
	if hist.Problem == "" {
		t.Error("history gives no reason chat cannot run")
	}
}

func TestAskValidatesTheQuestion(t *testing.T) {
	_, h, db := testServer(t)
	useFakeChatModel(t, db, chatModelServer(t, `{"text":"x"}`, "answer"))

	for name, body := range map[string]string{
		"empty":     `{"question":""}`,
		"missing":   `{}`,
		"not json":  `not json`,
		"bad field": `{"questoin":"typo"}`,
	} {
		w := do(t, h, "POST", "/api/ask", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
	}
}

// The cap is the guard working, not a server fault, and the message has to say
// what to do about it.
func TestAskReportsTheChatCapAsARateLimit(t *testing.T) {
	_, h, db := testServer(t)
	seedSearchable(t, db)
	useFakeChatModel(t, db, chatModelServer(t, `{"text":"harness"}`, "answer [1]"))
	if err := db.SetSettings(map[string]string{store.KeyMaxChatCallsPerDay: "1"}); err != nil {
		t.Fatal(err)
	}

	// Spend the allowance.
	if _, err := db.RecordLLMCall(store.LLMCall{
		RunDate: store.RunDate(time.Now().UTC()), Role: "chat", Model: "m", OK: true,
	}); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "POST", "/api/ask", `{"question":"anything?"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "chat") || !strings.Contains(body, "Settings") {
		t.Errorf("message should name the chat cap and where to change it: %s", body)
	}
}

// Chat's cap must not be reachable by the pipeline's spending, and vice versa.
func TestChatCapIsSeparateFromTheRunCap(t *testing.T) {
	_, h, db := testServer(t)
	seedSearchable(t, db)
	useFakeChatModel(t, db, chatModelServer(t, `{"text":"harness"}`, "answer [1]"))

	// A run's worth of classifier calls, well past a small per-run cap.
	if err := db.SetSettings(map[string]string{store.KeyMaxCallsPerRun: "1"}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := db.RecordLLMCall(store.LLMCall{
			RunDate: store.RunDate(time.Now().UTC()), Role: "classifier", Model: "m", OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	w := do(t, h, "POST", "/api/ask", `{"question":"harness?"}`)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d; a spent run budget must not block a question: %s", w.Code, w.Body.String())
	}
}

func TestSettingsCarriesTheChatRole(t *testing.T) {
	_, h, _ := testServer(t)

	got := decode[settingsResponse](t, do(t, h, "GET", "/api/settings", ""))
	if got.ChatModel == "" {
		t.Error("chat_model is not seeded; a fresh instance cannot chat")
	}
	if got.MaxChatCallsPerDay == 0 {
		t.Error("max_chat_calls_per_day is not exposed")
	}

	// It is switchable without a restart, like every other role (§3.1).
	saved := decode[settingsResponse](t, do(t, h, "PUT", "/api/settings", `{"chat_model":"ollama/llama3"}`))
	if saved.ChatModel != "ollama/llama3" {
		t.Errorf("chat_model = %q after save", saved.ChatModel)
	}

	// And a malformed ref is refused rather than breaking the next question.
	if w := do(t, h, "PUT", "/api/settings", `{"chat_model":"no-slash"}`); w.Code != http.StatusBadRequest {
		t.Errorf("malformed chat_model accepted: status %d", w.Code)
	}
}

// --- source import / export ---

func TestExportSourcesRoundTripsThroughImport(t *testing.T) {
	_, h, db := testServer(t)

	before, err := db.AllSources()
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "GET", "/api/sources/export", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q; an export is a file to keep", cd)
	}
	bundle := decode[store.SourceBundle](t, w)
	if bundle.Version != store.SourceBundleVersion {
		t.Errorf("version = %d", bundle.Version)
	}
	if len(bundle.Sources) != len(before) {
		t.Errorf("exported %d sources, have %d", len(bundle.Sources), len(before))
	}
	// Instance-local state must not travel.
	raw := w.Body.String()
	for _, field := range []string{`"id"`, `"last_run_at"`} {
		if strings.Contains(raw, field) {
			t.Errorf("export carries %s, which means nothing on another instance", field)
		}
	}

	// Re-importing its own export must be a no-op, not a duplication.
	body, _ := json.Marshal(map[string]any{
		"version": bundle.Version, "sources": bundle.Sources, "verify": false,
	})
	res := decode[importResponse](t, do(t, h, "POST", "/api/sources/import", string(body)))
	if res.Added != 0 {
		t.Errorf("re-importing its own export added %d sources", res.Added)
	}
	if res.Skipped != len(bundle.Sources) {
		t.Errorf("skipped %d of %d", res.Skipped, len(bundle.Sources))
	}

	after, _ := db.AllSources()
	if len(after) != len(before) {
		t.Errorf("source count changed from %d to %d", len(before), len(after))
	}
}

// A renamed feed is still that feed. Matching on name would import it again and
// inflate every source_count that touches it.
func TestImportMatchesOnTargetNotName(t *testing.T) {
	_, h, db := testServer(t)

	before, _ := db.AllSources()
	var existing store.Source
	for _, s := range before {
		if s.Kind == "rss" {
			existing = s
			break
		}
	}
	if existing.ID == 0 {
		t.Fatal("no seeded rss source to work with")
	}

	body, _ := json.Marshal(map[string]any{
		"version": store.SourceBundleVersion,
		"verify":  false,
		"sources": []any{map[string]any{
			"kind": existing.Kind, "name": "Totally Different Name",
			"config": json.RawMessage(existing.Config), "active": true,
		}},
	})
	res := decode[importResponse](t, do(t, h, "POST", "/api/sources/import", string(body)))
	if res.Added != 0 {
		t.Errorf("added %d: a renamed feed pointing at the same URL is the same source", res.Added)
	}
	if res.Skipped != 1 {
		t.Errorf("skipped %d, want 1", res.Skipped)
	}
}

// Import is additive. A stale export must not delete the pruning done since.
func TestImportNeverDeletesExistingSources(t *testing.T) {
	_, h, db := testServer(t)
	before, _ := db.AllSources()

	body, _ := json.Marshal(map[string]any{
		"version": store.SourceBundleVersion, "verify": false,
		"sources": []any{map[string]any{
			"kind": "rss", "name": "Something New",
			"config": json.RawMessage(`{"url":"https://brand-new.test/feed"}`), "active": true,
		}},
	})
	if w := do(t, h, "POST", "/api/sources/import", string(body)); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	after, _ := db.AllSources()
	if len(after) != len(before)+1 {
		t.Errorf("have %d sources, want %d: import must only add", len(after), len(before)+1)
	}
}

func TestImportValidatesTheBundle(t *testing.T) {
	_, h, _ := testServer(t)

	for name, body := range map[string]string{
		"wrong version": `{"version":99,"sources":[{"kind":"rss","name":"x","config":{}}]}`,
		"no sources":    `{"version":1,"sources":[]}`,
		"not json":      `nope`,
	} {
		if w := do(t, h, "POST", "/api/sources/import", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
	}
}

func TestImportRejectsBadRowsButKeepsGoing(t *testing.T) {
	_, h, db := testServer(t)
	before, _ := db.AllSources()

	body, _ := json.Marshal(map[string]any{
		"version": store.SourceBundleVersion, "verify": false,
		"sources": []any{
			map[string]any{"kind": "carrier_pigeon", "name": "Nope", "config": json.RawMessage(`{"url":"x"}`)},
			map[string]any{"kind": "rss", "name": "", "config": json.RawMessage(`{"url":"y"}`)},
			map[string]any{"kind": "rss", "name": "Good One",
				"config": json.RawMessage(`{"url":"https://good.test/feed"}`), "active": true},
		},
	})
	res := decode[importResponse](t, do(t, h, "POST", "/api/sources/import", string(body)))

	if res.Rejected != 2 {
		t.Errorf("rejected %d, want 2", res.Rejected)
	}
	if res.Added != 1 {
		t.Errorf("added %d, want 1 — one bad row must not abort the file", res.Added)
	}
	if len(res.Results) != 3 {
		t.Fatalf("got %d results, want one per row", len(res.Results))
	}
	for _, r := range res.Results {
		if r.Status == "rejected" && r.Reason == "" {
			t.Error("a rejected row gives no reason")
		}
	}

	after, _ := db.AllSources()
	if len(after) != len(before)+1 {
		t.Errorf("have %d sources, want %d", len(after), len(before)+1)
	}
}

// The convention in CLAUDE.md: a source URL that has not been through the real
// fetcher is not trusted. A feed that answers but carries nothing is the exact
// failure it exists for, so it lands inactive rather than live -- and rather than
// being silently dropped, which would lose part of the owner's list.
func TestImportVerifiesThroughTheRealFetcher(t *testing.T) {
	_, h, db := testServer(t)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>F</title>
		  <item><title>A real post</title><link>https://good.test/a</link></item>
		</channel></rss>`)
	}))
	defer good.Close()

	// 200, well-formed, and zero items. This is the trap.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>F</title></channel></rss>`)
	}))
	defer empty.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer dead.Close()

	body, _ := json.Marshal(map[string]any{
		"version": store.SourceBundleVersion, "verify": true,
		"sources": []any{
			map[string]any{"kind": "rss", "name": "Good", "active": true,
				"config": json.RawMessage(fmt.Sprintf(`{"url":%q}`, good.URL))},
			map[string]any{"kind": "rss", "name": "Answers But Empty", "active": true,
				"config": json.RawMessage(fmt.Sprintf(`{"url":%q}`, empty.URL))},
			map[string]any{"kind": "rss", "name": "Dead", "active": true,
				"config": json.RawMessage(fmt.Sprintf(`{"url":%q}`, dead.URL))},
		},
	})
	res := decode[importResponse](t, do(t, h, "POST", "/api/sources/import", string(body)))

	if !res.Verified {
		t.Error("response does not report that verification ran")
	}
	byName := map[string]store.ImportOutcome{}
	for _, r := range res.Results {
		byName[r.Name] = r
	}

	if got := byName["Good"]; got.Status != "added" || got.Items == 0 {
		t.Errorf("Good: %+v — a working feed should be added active with an item count", got)
	}
	if got := byName["Answers But Empty"]; got.Status != "added_inactive" {
		t.Errorf("Answers But Empty: status %q, want added_inactive — 200 is not evidence", got.Status)
	}
	if got := byName["Dead"]; got.Status != "added_inactive" {
		t.Errorf("Dead: status %q, want added_inactive", got.Status)
	}

	// And the database agrees: only the verified one is live.
	all, _ := db.AllSources()
	for _, s := range all {
		switch s.Name {
		case "Good":
			if !s.Active {
				t.Error("the verified feed was saved inactive")
			}
		case "Answers But Empty", "Dead":
			if s.Active {
				t.Errorf("%s was saved active despite failing verification", s.Name)
			}
		}
	}
}

// --- manual-forward inbox ---

func pageServer(t *testing.T, title, desc string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><title>%s</title>
			<meta property="og:description" content="%s"></head></html>`, title, desc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInboxSavesAPastedLink(t *testing.T) {
	_, h, db := testServer(t)
	page := pageServer(t, "A post worth keeping", "Why it matters.")

	got := decode[inboxResponse](t, do(t, h, "POST", "/api/inbox",
		fmt.Sprintf(`{"url":%q}`, page.URL)))

	if got.Title != "A post worth keeping" {
		t.Errorf("title = %q — the page was not really read", got.Title)
	}
	if !got.New {
		t.Error("first save reports not-new")
	}
	if got.Note == "" {
		t.Error("no note; the reader is not told when it gets classified")
	}

	// It must be a real item, reachable by the classifier.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM items WHERE title = ?`, got.Title).Scan(&n); err != nil || n != 1 {
		t.Errorf("item rows = %d, err = %v", n, err)
	}
	items, err := db.ItemsSince("", 100)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range items {
		if it.Title == got.Title {
			found = true
		}
	}
	if !found {
		t.Error("the saved link is not in the classifier's work queue")
	}
}

// Pasting the same thing twice is normal. It is "already saved", not an error.
func TestInboxIsIdempotent(t *testing.T) {
	_, h, db := testServer(t)
	page := pageServer(t, "Same link", "Body.")
	body := fmt.Sprintf(`{"url":%q}`, page.URL)

	first := decode[inboxResponse](t, do(t, h, "POST", "/api/inbox", body))
	second := decode[inboxResponse](t, do(t, h, "POST", "/api/inbox", body))

	if !first.New {
		t.Error("first save should be new")
	}
	if second.New {
		t.Error("second save of the same link reports new")
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM items WHERE title = 'Same link'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d item rows for one link", n)
	}
}

// The inbox source exists to attribute items to and has no feed. It must be
// created paused, or ingest would try to fetch it on every run.
func TestInboxSourceIsPausedAndSkippedByIngest(t *testing.T) {
	_, h, db := testServer(t)
	page := pageServer(t, "Anything", "Body.")
	do(t, h, "POST", "/api/inbox", fmt.Sprintf(`{"url":%q}`, page.URL))

	sources, err := db.AllSources()
	if err != nil {
		t.Fatal(err)
	}
	var manual *store.Source
	for i := range sources {
		if store.IsManualSource(sources[i]) {
			manual = &sources[i]
		}
	}
	if manual == nil {
		t.Fatal("no manual source was created")
	}
	if manual.Active {
		t.Error("the manual source is active; ingest would try to fetch a feed that does not exist")
	}
	if manual.Name != store.ManualSourceName {
		t.Errorf("name = %q", manual.Name)
	}

	// And only one, however many links are saved.
	do(t, h, "POST", "/api/inbox", fmt.Sprintf(`{"url":%q}`, pageServer(t, "Second", "b").URL))
	after, _ := db.AllSources()
	count := 0
	for _, s := range after {
		if store.IsManualSource(s) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("%d manual sources, want exactly 1", count)
	}
}

func TestInboxValidatesAndReportsFailures(t *testing.T) {
	_, h, _ := testServer(t)

	for name, body := range map[string]string{
		"no url":   `{"url":""}`,
		"missing":  `{}`,
		"not json": `nope`,
		"garbage":  `{"url":"::::"}`,
	} {
		if w := do(t, h, "POST", "/api/inbox", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
	}

	// A page that cannot be read is a 502: the request was fine, the page was not.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer dead.Close()
	if w := do(t, h, "POST", "/api/inbox", fmt.Sprintf(`{"url":%q}`, dead.URL)); w.Code != http.StatusBadGateway {
		t.Errorf("dead page: status %d, want 502", w.Code)
	}
}

// --- trending ---

func TestTrendingReportsHistoryAndDirection(t *testing.T) {
	_, h, db := testServer(t)

	topics, _ := db.AllTopics()
	topic := topics[0].ID
	for _, back := range []int{1, 2, 3} {
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (date('now','localtime',?), ?, 'Agent harness patterns converge', 'Because.', 5, 4, 'https://x.test/a')`,
			fmt.Sprintf("-%d days", back), topic,
		); err != nil {
			t.Fatal(err)
		}
	}

	got := decode[trendResponse](t, do(t, h, "GET", "/api/trending", ""))

	if got.Days != 30 {
		t.Errorf("days = %d, want the 30-day default", got.Days)
	}
	if got.TotalSignals != 3 {
		t.Errorf("total signals = %d, want 3", got.TotalSignals)
	}
	if got.TotalHot != 3 {
		t.Errorf("total hot = %d, want 3 (source_count 4 is over the threshold)", got.TotalHot)
	}
	if got.DaysWithSignals != 3 {
		t.Errorf("days with signals = %d, want 3 — this is what makes the totals readable", got.DaysWithSignals)
	}
	if len(got.Topics) != len(topics) {
		t.Errorf("got %d topic rows, want %d — silent topics must still appear",
			len(got.Topics), len(topics))
	}
	if len(got.Daily) != 3 {
		t.Errorf("got %d daily rows, want 3", len(got.Daily))
	}
	// The same story on three days is the thing a daily sky cannot show.
	if len(got.Recurring) != 1 {
		t.Errorf("got %d recurring stories, want 1: %+v", len(got.Recurring), got.Recurring)
	}

	// Direction is served, so the UI cannot derive a different answer.
	var seen bool
	for _, tr := range got.Topics {
		if tr.Direction == "" {
			t.Errorf("topic %d has no direction", tr.TopicID)
		}
		if tr.TopicID == topic && tr.Signals == 3 {
			seen = true
		}
	}
	if !seen {
		t.Error("the seeded topic is missing its counts")
	}
}

func TestTrendingValidatesTheWindow(t *testing.T) {
	_, h, _ := testServer(t)

	for _, bad := range []string{"0", "1", "-4", "400", "thirty"} {
		if w := do(t, h, "GET", "/api/trending?days="+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("days=%s returned %d, want 400", bad, w.Code)
		}
	}
	for _, ok := range []string{"2", "7", "365"} {
		if w := do(t, h, "GET", "/api/trending?days="+ok, ""); w.Code != http.StatusOK {
			t.Errorf("days=%s returned %d, want 200", ok, w.Code)
		}
	}
}

// A fresh instance has no history. The endpoint must answer, not fail.
func TestTrendingOnAFreshInstance(t *testing.T) {
	_, h, _ := testServer(t)

	got := decode[trendResponse](t, do(t, h, "GET", "/api/trending", ""))
	if got.TotalSignals != 0 || got.DaysWithSignals != 0 {
		t.Errorf("fresh instance reports signals: %+v", got)
	}
	if len(got.Topics) == 0 {
		t.Error("seeded topics should still be listed with zero counts")
	}
	for _, tr := range got.Topics {
		if tr.Direction != "quiet" {
			t.Errorf("topic %d reports %q on a fresh instance, want quiet", tr.TopicID, tr.Direction)
		}
	}
}

// --- weekly round-up and delivery ---

func TestWeeklyReportsDeliveryStatusOnAFreshInstance(t *testing.T) {
	_, h, _ := testServer(t)

	got := decode[weeklyResponse](t, do(t, h, "GET", "/api/weekly", ""))
	if got.Latest != nil {
		t.Error("a fresh instance has a round-up")
	}
	// Delivery is off by default — decision #10 dropped email from Phase 0 on
	// purpose, and this is the switch, not the default.
	if got.Delivery.EmailEnabled || got.Delivery.PingEnabled {
		t.Errorf("delivery is on by default: %+v", got.Delivery)
	}
	if got.Delivery.Schedule == "" {
		t.Error("no weekly schedule reported; the screen cannot say when it is due")
	}
}

// weekdaysInCoveredWeek returns n consecutive dates starting at the Monday of the
// week a default POST /api/weekly/run covers — the week that just finished.
//
// It resolves the week the same way the handler does, in the same location
// testServer configures, so a seeded signal is inside the window on every day of
// the week rather than on four of them. The bounds arithmetic itself is pinned
// against fixed dates in pipeline.TestWeekBoundsStartOnMonday, so leaning on it
// here does not leave that logic unchecked.
//
// n must stay ≤ 7; past that the dates run out of the week they are meant to be in.
func weekdaysInCoveredWeek(t *testing.T, n int) []string {
	t.Helper()
	if n < 1 || n > 7 {
		t.Fatalf("weekdaysInCoveredWeek(%d): want 1..7", n)
	}

	weekStart, weekEnd := store.PreviousWeekBounds(time.Now().In(time.UTC))
	monday, err := time.ParseInLocation("2006-01-02", weekStart, time.UTC)
	if err != nil {
		t.Fatalf("parse week start %q: %v", weekStart, err)
	}

	days := make([]string, n)
	for i := range days {
		days[i] = monday.AddDate(0, 0, i).Format("2006-01-02")
		if days[i] > weekEnd {
			t.Fatalf("day %s is past the covered week ending %s", days[i], weekEnd)
		}
	}
	return days
}

func TestWeeklyRunWritesAndStores(t *testing.T) {
	_, h, db := testServer(t)
	useFakeChatModel(t, db, chatModelServer(t, `{}`, "The week converged on harnesses."))

	topics, _ := db.AllTopics()
	// Signals inside last week, which is what a scheduled run covers.
	//
	// Anchored to the week the handler will actually resolve, not to a fixed day
	// offset. "-8 days" is only inside last week on some weekdays: run on a
	// Monday it lands on the Sunday *before* the covered week, so the run found
	// zero signals and this failed — every Monday. It was quietly weaker on
	// Tuesday and Wednesday too, where only one and two of the three rows fell in
	// range while the test still passed.
	//
	// The dates go in as literals for a second reason: the server resolves the
	// week in Env.Location, which is UTC here, while date('now','localtime')
	// follows the machine — and for part of every day those disagree.
	for i, day := range weekdaysInCoveredWeek(t, 3) {
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (?, ?, ?, 'Because.', 5, 3, 'https://x.test/a')`,
			day, topics[0].ID, fmt.Sprintf("Story %d", i),
		); err != nil {
			t.Fatal(err)
		}
	}

	w := do(t, h, "POST", "/api/weekly/run", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	got := decode[pipeline.WeeklyRunResult](t, w)

	if !got.Written {
		t.Fatalf("nothing written: %+v", got)
	}
	if got.Signals == 0 {
		t.Error("written from zero signals")
	}
	// Delivery is off, so nothing was sent and the response says why.
	if got.EmailSent || got.PingSent {
		t.Error("something was delivered with delivery switched off")
	}
	if got.DeliverNote == "" {
		t.Error("no note explaining that delivery is off")
	}

	list := decode[weeklyResponse](t, do(t, h, "GET", "/api/weekly", ""))
	if list.Latest == nil || list.Latest.Body == "" {
		t.Fatal("the round-up is not in the history")
	}
}

// Pressing "write it now" to read the prose must not mail anyone.
func TestWeeklyRunDoesNotDeliverUnlessAsked(t *testing.T) {
	_, h, db := testServer(t)
	useFakeChatModel(t, db, chatModelServer(t, `{}`, "Body."))

	var pinged int
	ping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pinged++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ping.Close()

	if err := db.SetSettings(map[string]string{
		store.KeyPingEnabled: "1", store.KeyPingURL: ping.URL,
	}); err != nil {
		t.Fatal(err)
	}

	topics, _ := db.AllTopics()
	// Inside the covered week on any weekday — see weekdaysInCoveredWeek.
	if _, err := db.Exec(
		`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
		 VALUES (?, ?, 'A story', 'Because.', 5, 3, 'https://x.test/a')`,
		weekdaysInCoveredWeek(t, 1)[0], topics[0].ID); err != nil {
		t.Fatal(err)
	}

	// Default: no deliver flag.
	do(t, h, "POST", "/api/weekly/run", `{}`)
	if pinged != 0 {
		t.Errorf("pinged %d times without being asked to deliver", pinged)
	}

	// Explicitly asked.
	got := decode[pipeline.WeeklyRunResult](t, do(t, h, "POST", "/api/weekly/run", `{"deliver":true}`))
	if pinged != 1 {
		t.Errorf("pinged %d times when delivery was requested, want 1", pinged)
	}
	if !got.PingSent {
		t.Errorf("ping not reported as sent: %+v", got)
	}
}

func TestWeeklyRunValidatesTheWeek(t *testing.T) {
	_, h, _ := testServer(t)
	for _, bad := range []string{`{"week":"last-monday"}`, `{"week":"2026-8-1"}`, `nope`} {
		if w := do(t, h, "POST", "/api/weekly/run", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, w.Code)
		}
	}
}

// Enabling delivery with nowhere to deliver to is the one combination that fails
// silently a week later, so it must be refused at save time.
func TestSettingsRefusesDeliveryWithNoDestination(t *testing.T) {
	_, h, _ := testServer(t)

	if w := do(t, h, "PUT", "/api/settings", `{"digest_enabled":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("enabling email with no recipient: status %d, want 400", w.Code)
	}
	if w := do(t, h, "PUT", "/api/settings", `{"ping_enabled":true}`); w.Code != http.StatusBadRequest {
		t.Errorf("enabling ping with no URL: status %d, want 400", w.Code)
	}
	// Together they are fine.
	if w := do(t, h, "PUT", "/api/settings",
		`{"digest_enabled":true,"digest_to":"owner@example.test"}`); w.Code != http.StatusOK {
		t.Errorf("enabling email with a recipient: status %d: %s", w.Code, w.Body.String())
	}
}

func TestSettingsValidatesTheWeeklyScheduleAndPort(t *testing.T) {
	_, h, _ := testServer(t)

	for name, body := range map[string]string{
		"bad day":   `{"schedule_weekly_day":"someday"}`,
		"bad time":  `{"schedule_weekly_time":"25:00"}`,
		"bad port":  `{"smtp_port":0}`,
		"huge port": `{"smtp_port":70000}`,
	} {
		if w := do(t, h, "PUT", "/api/settings", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, w.Code)
		}
	}

	// Empty day switches the round-up off, which is legal.
	if w := do(t, h, "PUT", "/api/settings", `{"schedule_weekly_day":""}`); w.Code != http.StatusOK {
		t.Errorf("switching the round-up off: status %d: %s", w.Code, w.Body.String())
	}
}

// The SMTP credential follows the provider-key contract exactly: the settings row
// holds the env var NAME, the endpoint returns the name plus whether it resolves,
// and the value never appears anywhere.
func TestSettingsNeverReturnsTheSMTPPassword(t *testing.T) {
	_, h, db := testServer(t)

	const secret = "re_super_secret_resend_key"
	t.Setenv("TEST_RESEND_KEY", secret)
	if err := db.SetSettings(map[string]string{
		store.KeySMTPPasswordEnv: "TEST_RESEND_KEY",
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/settings", "/api/weekly", "/api/status"} {
		w := do(t, h, "GET", path, "")
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("%s leaked the SMTP password", path)
		}
	}

	got := decode[settingsResponse](t, do(t, h, "GET", "/api/settings", ""))
	if got.SMTPPasswordEnv != "TEST_RESEND_KEY" {
		t.Errorf("smtp_password_env = %q; the NAME should be returned", got.SMTPPasswordEnv)
	}
	if !got.SMTPPasswordResolved {
		t.Error("resolved is false though the variable is set")
	}

	// And unresolved is reported honestly.
	if err := db.SetSettings(map[string]string{
		store.KeySMTPPasswordEnv: "DEFINITELY_NOT_SET",
	}); err != nil {
		t.Fatal(err)
	}
	got = decode[settingsResponse](t, do(t, h, "GET", "/api/settings", ""))
	if got.SMTPPasswordResolved {
		t.Error("resolved is true for an unset variable")
	}
}

// The Resend defaults ship configured except for the recipient, so switching the
// digest on is one field plus one environment variable.
func TestResendDefaultsAreSeeded(t *testing.T) {
	_, h, _ := testServer(t)
	got := decode[settingsResponse](t, do(t, h, "GET", "/api/settings", ""))

	if got.SMTPHost != "smtp.resend.com" {
		t.Errorf("smtp_host = %q", got.SMTPHost)
	}
	if got.SMTPPort != 587 {
		t.Errorf("smtp_port = %d", got.SMTPPort)
	}
	if got.SMTPUsername != "resend" {
		t.Errorf("smtp_username = %q; Resend authenticates with the literal \"resend\"", got.SMTPUsername)
	}
	if got.SMTPPasswordEnv != "RESEND_API_KEY" {
		t.Errorf("smtp_password_env = %q", got.SMTPPasswordEnv)
	}
	if got.DigestEnabled {
		t.Error("the digest is on by default")
	}
	if got.DigestTo != "" {
		t.Error("a recipient is seeded; that is the owner's to fill in")
	}
}

// Both date validators must agree on what a date is. The search endpoint used to
// count digits, so 2026-99-99 got through and became a query bound that silently
// matched nothing instead of being refused.
func TestSearchRejectsImpossibleDates(t *testing.T) {
	_, h, db := testServer(t)
	seedSearchable(t, db)

	for _, bad := range []string{
		"2026-99-99", "2026-13-01", "2026-02-30", "0000-00-00", "2026-1-01", "26-01-01",
	} {
		if w := do(t, h, "GET", "/api/search?from="+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("from=%s returned %d, want 400", bad, w.Code)
		}
		if w := do(t, h, "GET", "/api/search?to="+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("to=%s returned %d, want 400", bad, w.Code)
		}
	}

	// Real dates, including a leap day, still work.
	for _, ok := range []string{"2026-08-13", "2024-02-29", "2026-01-01", "2026-12-31"} {
		if w := do(t, h, "GET", "/api/search?from="+ok, ""); w.Code != http.StatusOK {
			t.Errorf("from=%s returned %d, want 200", ok, w.Code)
		}
	}
}

// §3.1: a schedule change takes effect on the next run with no restart. That has to
// include the weekly round-up — the screen's own save message promises "the schedule
// was rebuilt", and until this was fixed the weekly keys did not trigger a reload at
// all, so the promise was false for them.
func TestSavingTheWeeklyScheduleReloadsTheScheduler(t *testing.T) {
	// testServer builds a server with no scheduler; this needs a real one attached,
	// because a reload that nothing is listening to proves nothing.
	_, _, db := testServer(t)

	env := config.Env{Port: "8422", DBPath: db.Path, Location: time.UTC}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := pipeline.NewRunner(db, env, log)
	t.Cleanup(runner.Stop)
	sched := pipeline.NewScheduler(db, env, runner, log)
	if err := sched.Start(); err != nil {
		t.Fatalf("scheduler Start: %v", err)
	}
	defer sched.Stop()

	srv := New(db, env, fstest.MapFS{}, runner, sched, log)
	handler := srv.Routes()

	weeklyDay := func() time.Weekday {
		for _, n := range sched.NextRuns() {
			if local := n.In(time.UTC); local.Hour() == 11 {
				return local.Weekday()
			}
		}
		return time.Weekday(-1)
	}
	if got := weeklyDay(); got != time.Monday {
		t.Fatalf("weekly day = %v before the change, want Monday", got)
	}

	if w := do(t, handler, "PUT", "/api/settings", `{"schedule_weekly_day":"fri"}`); w.Code != http.StatusOK {
		t.Fatalf("save: status %d: %s", w.Code, w.Body.String())
	}
	if got := weeklyDay(); got != time.Friday {
		t.Errorf("weekly day = %v after saving; the cron was not rebuilt, so the change needs a restart", got)
	}

	// And the time, too.
	if w := do(t, handler, "PUT", "/api/settings", `{"schedule_weekly_time":"06:15"}`); w.Code != http.StatusOK {
		t.Fatalf("save time: status %d", w.Code)
	}
	var sawNewTime bool
	for _, n := range sched.NextRuns() {
		if local := n.In(time.UTC); local.Hour() == 6 && local.Minute() == 15 {
			sawNewTime = true
		}
	}
	if !sawNewTime {
		t.Error("the new weekly time was not picked up without a restart")
	}
}

// The seeded registry ships every provider switched on, and the switch survives
// a round trip through settings.
func TestProviderToggleRoundTrips(t *testing.T) {
	_, h, _ := testServer(t)

	for _, p := range decode[settingsResponse](t, do(t, h, "GET", "/api/settings", "")).Providers {
		if !p.Active {
			t.Fatalf("seeded provider %q arrived switched off", p.Name)
		}
	}

	// deepseek is in no role since rev 6 seeded them onto openrouter, so it is
	// free to park.
	w := do(t, h, "PATCH", "/api/providers/deepseek", `{"active":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("switching off = %d, body %s", w.Code, w.Body)
	}
	if active := providerActive(t, h, "deepseek"); active {
		t.Error("deepseek still reports active after being switched off")
	}
	// Everything else is untouched: one endpoint, one entry.
	if !providerActive(t, h, "openrouter") {
		t.Error("switching deepseek off also switched openrouter off")
	}

	if w := do(t, h, "PATCH", "/api/providers/deepseek", `{"active":true}`); w.Code != http.StatusOK {
		t.Fatalf("switching back on = %d, body %s", w.Code, w.Body)
	}
	if !providerActive(t, h, "deepseek") {
		t.Error("deepseek did not come back on")
	}
}

// The failure this switch could cause is a silent one: park the provider your
// classifier runs on and the morning run stops, hours later, for a reason nobody
// connects to a toggle. So it is refused at the moment of the click, naming what
// stands on it.
func TestSwitchingOffAProviderARoleNeedsIsRefused(t *testing.T) {
	_, h, _ := testServer(t)

	// Rev 6 seeds all three roles onto openrouter.
	w := do(t, h, "PATCH", "/api/providers/openrouter", `{"active":false}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{"classifier", "synthesizer"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %q does not name the %s that depends on it", body, want)
		}
	}
	if !providerActive(t, h, "openrouter") {
		t.Error("a refused switch-off still changed the registry")
	}

	// Move the roles elsewhere and the same switch is allowed -- the guard is
	// about what depends on it now, not about the provider being special.
	put := `{"classifier_model":"ollama/x","synthesizer_model":"ollama/y","chat_model":"ollama/z"}`
	if w := do(t, h, "PUT", "/api/settings", put); w.Code != http.StatusOK {
		t.Fatalf("moving the roles = %d, body %s", w.Code, w.Body)
	}
	if w := do(t, h, "PATCH", "/api/providers/openrouter", `{"active":false}`); w.Code != http.StatusOK {
		t.Fatalf("switching off after moving the roles = %d, body %s", w.Code, w.Body)
	}
}

// A provider used only as a fallback is free to park: the chain is what runs
// when something else has already failed, so a shorter chain is a choice, not a
// breakage.
func TestSwitchingOffAFallbackOnlyProviderIsAllowed(t *testing.T) {
	_, h, _ := testServer(t)

	put := `{"classifier_fallback":["deepseek/some-model"]}`
	if w := do(t, h, "PUT", "/api/settings", put); w.Code != http.StatusOK {
		t.Fatalf("setting the fallback = %d, body %s", w.Code, w.Body)
	}
	if w := do(t, h, "PATCH", "/api/providers/deepseek", `{"active":false}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the fallback-only provider to be parkable; body %s", w.Code, w.Body)
	}
}

func TestProviderPatchRejectsUnknownProvider(t *testing.T) {
	_, h, _ := testServer(t)
	if w := do(t, h, "PATCH", "/api/providers/not-a-provider", `{"active":false}`); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func providerActive(t *testing.T, h http.Handler, name string) bool {
	t.Helper()
	for _, p := range decode[settingsResponse](t, do(t, h, "GET", "/api/settings", "")).Providers {
		if p.Name == name {
			return p.Active
		}
	}
	t.Fatalf("no provider named %q", name)
	return false
}

// The banner has to say what actually happened, not what was configured. A
// keyless provider whose entitlement lapsed passes every readiness check and
// fails every call; before this, the only symptom was an empty sky.
func TestBannerReportsAWhollyFailingRole(t *testing.T) {
	srv, h, db := testServer(t)

	today := store.RunDate(time.Now().In(srv.Env.Location))
	for i := 0; i < 63; i++ {
		if _, err := db.RecordLLMCall(store.LLMCall{
			RunDate: today, Role: "classifier", Provider: "ollama",
			Model: "deepseek-v4-flash:cloud", OK: false,
			Err: "ollama returned 403: this model requires a subscription",
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	got := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))
	if len(got.Failures) != 1 || got.Failures[0].Role != "classifier" {
		t.Fatalf("failures = %+v, want the classifier", got.Failures)
	}
	for _, want := range []string{"classifier", "63", "ollama/deepseek-v4-flash:cloud", "403"} {
		if !strings.Contains(got.Banner, want) {
			t.Errorf("banner %q does not mention %q", got.Banner, want)
		}
	}
}

// Chat failing means the Ask screen is broken, which is not the same as the
// morning run being broken. It travels in the payload so Settings can show it,
// but it must not dress itself up as a broken pipeline on the star map.
func TestChatFailuresStayOutOfTheSkyBanner(t *testing.T) {
	srv, h, db := testServer(t)

	today := store.RunDate(time.Now().In(srv.Env.Location))
	if _, err := db.RecordLLMCall(store.LLMCall{
		RunDate: today, Role: "chat", Provider: "deepseek", Model: "deepseek-chat",
		OK: false, Err: "402 insufficient balance",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	got := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))
	if len(got.Failures) != 1 || got.Failures[0].Role != "chat" {
		t.Fatalf("failures = %+v, want the chat role reported", got.Failures)
	}
	if strings.Contains(got.Banner, "chat") {
		t.Errorf("banner %q raised a chat failure on the star map", got.Banner)
	}
}

// A dead provider burns the daily budget on retries, so both conditions are true
// at once -- and only one of them is the cause. Raising the cap on a model that
// answers 403 just buys more 403s.
func TestFailureBannerOutranksTheCapBanner(t *testing.T) {
	srv, h, db := testServer(t)

	if w := do(t, h, "PUT", "/api/settings", `{"max_calls_per_day":2}`); w.Code != http.StatusOK {
		t.Fatalf("setting the cap = %d, body %s", w.Code, w.Body)
	}
	today := store.RunDate(time.Now().In(srv.Env.Location))
	for i := 0; i < 3; i++ {
		if _, err := db.RecordLLMCall(store.LLMCall{
			RunDate: today, Role: "synthesizer", Provider: "p", Model: "m", OK: false, Err: "403 nope",
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	got := decode[statusResponse](t, do(t, h, "GET", "/api/status", ""))
	if !strings.Contains(got.Banner, "403") {
		t.Errorf("banner %q reported the cap rather than the cause", got.Banner)
	}
}

// The delivery test reports three states for the webhook and only one of them is a
// failure. An unset webhook used to come back as "no webhook URL set", which put a
// red line about a channel the owner never asked for next to the result they were
// actually reading -- and painted the whole report red even when the email went.
//
// No SMTP connection is made here: digest_to seeds empty, so the email half
// short-circuits on Problem() before it dials. The webhook half is a local httptest
// server, which is the only kind of endpoint this suite is allowed to reach.
func TestDeliveryTestSeparatesAnUnsetWebhookFromABrokenOne(t *testing.T) {
	_, h, db := testServer(t)

	type testResult struct {
		Email      bool   `json:"email"`
		EmailError string `json:"email_error"`
		Ping       bool   `json:"ping"`
		PingError  string `json:"ping_error"`
		PingNote   string `json:"ping_note"`
	}
	run := func() testResult {
		w := do(t, h, "POST", "/api/delivery/test", "{}")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		return decode[testResult](t, w)
	}

	// A fresh instance: no webhook, switch off. Nothing to test is not a failure.
	res := run()
	if res.PingError != "" {
		t.Errorf("an unconfigured webhook reported a failure: %q", res.PingError)
	}
	if res.PingNote == "" {
		t.Error("an unconfigured webhook reported nothing at all")
	}

	// Switched on with nowhere to send IS a failure: the round-up will try.
	if err := db.SetSettings(map[string]string{store.KeyPingEnabled: "1"}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	res = run()
	if res.PingError == "" {
		t.Error("a webhook switched on with no URL was not reported as a problem")
	}
	if res.PingNote != "" {
		t.Errorf("a real problem was reported as a note: %q", res.PingNote)
	}

	// And a URL that answers.
	var hits int
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	if err := db.SetSettings(map[string]string{store.KeyPingURL: hook.URL}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	res = run()
	if !res.Ping || res.PingError != "" {
		t.Errorf("ping = %v, error = %q", res.Ping, res.PingError)
	}
	if hits != 1 {
		t.Errorf("the webhook was called %d times, want 1", hits)
	}

	// The email half never dialled: it has no destination, and says so.
	if res.EmailError == "" {
		t.Error("no destination and no email problem reported")
	}
}

// Sending a written round-up costs no model call and does not rewrite it.
//
// That is the whole reason POST /api/weekly/send exists next to /api/weekly/run: the
// button sends the prose on screen, works when the synthesizer does not, and cannot
// replace what the owner just read with a different draft.
func TestWeeklySendDeliversTheStoredRoundUp(t *testing.T) {
	_, h, db := testServer(t)

	// Nothing written yet.
	if w := do(t, h, "POST", "/api/weekly/send", "{}"); w.Code != http.StatusNotFound {
		t.Errorf("status with no round-up = %d, want 404", w.Code)
	}

	if _, err := db.SaveWeeklyDigest(store.WeeklyDigest{
		WeekStart: "2026-08-10", WeekEnd: "2026-08-16",
		Body:    "1. Story number A\n   What changed.\n   4 sources · Agents · 2026-08-10\n",
		Signals: 9, Topics: 2,
	}); err != nil {
		t.Fatalf("SaveWeeklyDigest: %v", err)
	}

	// Written, but both channels off: a configuration state, reported as such rather
	// than as a failure or a 500.
	res := decode[pipeline.WeeklyRunResult](t, do(t, h, "POST", "/api/weekly/send", "{}"))
	if res.Delivered {
		t.Error("something was sent with both channels off")
	}
	if res.DeliverNote == "" {
		t.Error("nothing was sent and nothing said why")
	}

	// Webhook on. Email stays off deliberately: this suite must never open an SMTP
	// connection, and the two channels are independent by design.
	var hits int
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	if err := db.SetSettings(map[string]string{
		store.KeyPingEnabled: "1", store.KeyPingURL: hook.URL,
	}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	res = decode[pipeline.WeeklyRunResult](t, do(t, h, "POST", "/api/weekly/send", "{}"))
	if !res.Delivered || !res.PingSent {
		t.Errorf("delivered=%v ping=%v error=%q", res.Delivered, res.PingSent, res.PingError)
	}
	if hits != 1 {
		t.Errorf("the webhook was called %d times, want 1", hits)
	}
	if res.WeekStart != "2026-08-10" {
		t.Errorf("week = %q, want the latest stored round-up", res.WeekStart)
	}

	// No model call: sending is not composing. This is what makes the button usable on
	// the day the provider is the reason the send has to be retried.
	if n := countLLMCalls(t, db); n != 0 {
		t.Errorf("%d model calls billed for a send", n)
	}

	// And the send is recorded against the digest, so a retry is distinguishable from
	// a first attempt.
	stored, err := db.WeeklyDigestFor("2026-08-10")
	if err != nil {
		t.Fatalf("WeeklyDigestFor: %v", err)
	}
	if stored.SentAt == "" || stored.SentVia != "ping" {
		t.Errorf("sent_at=%q sent_via=%q", stored.SentAt, stored.SentVia)
	}
	// The body must be untouched -- sending is not composing.
	if !strings.Contains(stored.Body, "1. Story number A") {
		t.Errorf("the stored round-up changed when it was sent:\n%s", stored.Body)
	}
}

// A week that has no round-up is a 404, not an empty send.
func TestWeeklySendValidatesTheWeek(t *testing.T) {
	_, h, db := testServer(t)
	if _, err := db.SaveWeeklyDigest(store.WeeklyDigest{
		WeekStart: "2026-08-10", WeekEnd: "2026-08-16", Body: "Body.", Signals: 1, Topics: 1,
	}); err != nil {
		t.Fatalf("SaveWeeklyDigest: %v", err)
	}

	if w := do(t, h, "POST", "/api/weekly/send", `{"week":"not-a-date"}`); w.Code != http.StatusBadRequest {
		t.Errorf("status for a malformed week = %d, want 400", w.Code)
	}
	if w := do(t, h, "POST", "/api/weekly/send", `{"week":"2026-01-05"}`); w.Code != http.StatusNotFound {
		t.Errorf("status for a week with no round-up = %d, want 404", w.Code)
	}
}

func countLLMCalls(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM llm_calls`).Scan(&n); err != nil {
		t.Fatalf("count llm_calls: %v", err)
	}
	return n
}

// Clearing the transcript deletes the turns and refunds nothing.
//
// Both halves matter. A clear that only emptied the screen would put every question
// back on the next open, since the page loads its history each time; and a clear that
// reset the day's chat budget would be a way to spend the cap twice, because the calls
// really were made and the provider really did bill them.
func TestClearChatHistoryKeepsTheCostLog(t *testing.T) {
	_, h, db := testServer(t)

	for _, q := range []string{"what happened?", "and yesterday?"} {
		if _, err := db.SaveChatTurn(store.ChatTurn{
			RunDate: "2026-08-18", Question: q, Answer: "Something did.", HitCount: 2, Calls: 1,
		}); err != nil {
			t.Fatalf("SaveChatTurn: %v", err)
		}
	}
	// Today's date, not a literal: the handler counts the day's chat spend against
	// time.Now(), so a hardcoded run date makes this assertion pass on exactly one
	// calendar day and quietly fail on every other one. (It did -- 2026-08-18.)
	if _, err := db.RecordLLMCall(store.LLMCall{
		RunDate: store.RunDate(time.Now()), Role: "chat", Provider: "deepseek",
		Model: "deepseek-chat", TokensIn: 900, TokensOut: 120, OK: true,
	}); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}

	before := decode[chatHistoryResponse](t, do(t, h, "GET", "/api/chat/history", ""))
	if len(before.Turns) != 2 {
		t.Fatalf("%d turns before clearing, want 2", len(before.Turns))
	}
	if before.ChatCallsToday != 1 {
		t.Fatalf("chat_calls_today = %d before clearing, want 1", before.ChatCallsToday)
	}

	w := do(t, h, "DELETE", "/api/chat/history", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if cleared := decode[map[string]int64](t, w)["cleared"]; cleared != 2 {
		t.Errorf("cleared = %d, want 2", cleared)
	}

	after := decode[chatHistoryResponse](t, do(t, h, "GET", "/api/chat/history", ""))
	if len(after.Turns) != 0 {
		t.Errorf("%d turns survived the clear", len(after.Turns))
	}
	// The budget is read from llm_calls, so it must be untouched.
	if after.ChatCallsToday != 1 {
		t.Errorf("chat_calls_today = %d after clearing, want the spend to stand at 1", after.ChatCallsToday)
	}
	if n := countLLMCalls(t, db); n != 1 {
		t.Errorf("%d rows in the cost log after clearing, want 1", n)
	}

	// Clearing an empty transcript is a no-op, not an error.
	if w := do(t, h, "DELETE", "/api/chat/history", ""); w.Code != http.StatusOK {
		t.Errorf("clearing an empty transcript = %d", w.Code)
	}
}

// The quality half of the cost log, over the wire.
//
// The row must name the model and say both things about it: that the reply parsed,
// and that it covered a fraction of what it was asked. Those two are what tell a
// broken model from a badly written topic definition, and the screen renders them
// side by side.
func TestModelStatsReportsParseAndCompletenessSeparately(t *testing.T) {
	_, h, db := testServer(t)
	today := store.RunDate(time.Now())

	id, err := db.RecordLLMCall(store.LLMCall{
		RunDate: today, Role: "classifier", Provider: "openrouter",
		Model: "flash", OK: true, ItemsIn: 30, TokensIn: 900, TokensOut: 40,
	})
	if err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}
	if err := db.RecordReplyQuality(id, store.ReplyQuality{ItemsOut: 2, ParseOK: true}); err != nil {
		t.Fatalf("RecordReplyQuality: %v", err)
	}
	// A chat answer is not a list of anything and must not acquire a rate.
	if _, err := db.RecordLLMCall(store.LLMCall{
		RunDate: today, Role: "chat", Provider: "openrouter", Model: "flash", OK: true,
	}); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}

	w := do(t, h, "GET", "/api/models/stats", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := decode[modelStatsResponse](t, w)

	if got.Days != 7 {
		t.Errorf("days = %d, want the 7-day default", got.Days)
	}
	if !got.Ready {
		t.Error("not ready, but a classifier reply was measured in the window")
	}

	var classifier, chat *store.ModelReliability
	for i := range got.Rows {
		switch got.Rows[i].Role {
		case "classifier":
			classifier = &got.Rows[i]
		case "chat":
			chat = &got.Rows[i]
		}
	}
	if classifier == nil || chat == nil {
		t.Fatalf("want a row per role, got %+v", got.Rows)
	}

	if classifier.ParseRate != 1 {
		t.Errorf("classifier parse rate %v, want 1", classifier.ParseRate)
	}
	if classifier.Completeness <= 0 || classifier.Completeness > 0.1 {
		t.Errorf("classifier completeness %v, want 2/30", classifier.Completeness)
	}
	if chat.ParseRate != -1 || chat.Completeness != -1 {
		t.Errorf("chat rates = %v/%v, want the -1 sentinel so the screen prints an em dash",
			chat.ParseRate, chat.Completeness)
	}
}

// The window is bounded the same way the topic view's is: the two pickers move
// together on one screen, so they must accept the same range.
func TestModelStatsRejectsAnOutOfRangeWindow(t *testing.T) {
	_, h, _ := testServer(t)

	for _, q := range []string{"0", "-3", "9999", "soon"} {
		w := do(t, h, "GET", "/api/models/stats?days="+q, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("days=%s gave %d, want 400", q, w.Code)
		}
	}
}

// Suggesting from a title alone is how you get eight plausible feeds about the
// wrong subject. The definition is the input, so its absence is a 400 that says
// what to do -- checked before the model config, because it is true regardless of
// whether a model is even configured.
func TestSuggestSourcesRefusesATopicWithNoDefinition(t *testing.T) {
	_, h, db := testServer(t)

	id, err := db.CreateTopic(store.Topic{Title: "Unset", Definition: "", Active: true})
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "POST", fmt.Sprintf("/api/topics/%d/sources/suggest", id), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "definition") {
		t.Errorf("the message does not name the missing thing: %s", w.Body.String())
	}
}

func TestSuggestSourcesRefusesAnUnknownTopic(t *testing.T) {
	_, h, _ := testServer(t)

	w := do(t, h, "POST", "/api/topics/9999/sources/suggest", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// Readiness is checked on the SYNTHESIZER, which is the chain this runs on --
// never Ready(), which also covers the classifier. Whether the classifier has a
// key has nothing to do with whether a suggestion pass can work, and refusing on
// it would be the same mistake as folding chat into Ready().
func TestSuggestSourcesReportsTheSynthesizerSpecifically(t *testing.T) {
	_, h, db := testServer(t)

	id, err := db.CreateTopic(store.Topic{
		Title: "Agents", Definition: "Anything about agent harnesses.", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// No provider keys are set (testServer isolates from the real environment), so
	// the synthesizer cannot run and the message must say which env var is missing.
	w := do(t, h, "POST", fmt.Sprintf("/api/topics/%d/sources/suggest", id), "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "synthesizer") {
		t.Errorf("the message does not name the role that is broken: %s", body)
	}
	if !strings.Contains(body, "API_KEY") {
		t.Errorf("the message does not name the missing env var: %s", body)
	}
}

// An asset served from disk must not be cacheable, and an embedded one must be.
//
// http.FileServerFS sends Last-Modified and no Cache-Control, which is enough
// for a browser to cache heuristically. In development that means an edit which
// is on disk, and which the server is serving, still does not reach the page --
// the same "my edit did nothing" as an unrebuilt binary, from the other end. In
// production the opposite is wanted: embedded assets cannot change without a new
// container, so caching them is free.
func TestAssetCachePolicyFollowsWhereAssetsComeFrom(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fromDisk bool
		want     string
	}{
		{"served from disk", true, "no-store"},
		{"embedded", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := testServer(t)
			srv.WebFromDisk = tc.fromDisk
			h := srv.Routes()

			// "/" not "/index.html": http.FileServer redirects the explicit
			// name to the directory, and a 301 carries no asset headers.
			w := do(t, h, "GET", "/", "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET / = %d", w.Code)
			}
			if got := w.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("asset Cache-Control = %q, want %q", got, tc.want)
			}

			// The API is no-store either way: it is the run's output, not the UI.
			if got := do(t, h, "GET", "/api/status", "").Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("api Cache-Control = %q, want no-store", got)
			}
		})
	}
}

// --- the world model ---

func seedWMSource(t *testing.T, db *store.DB) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO sources (kind, name, config, active) VALUES ('rss', 'a feed', '{}', 1)`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func seedWMItem(t *testing.T, db *store.DB, sourceID int64, url string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO items (source_id, canonical_url, title) VALUES (?, ?, ?)`, sourceID, url, url)
	if err != nil {
		t.Fatalf("insert item: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestCandidateReviewEndpoints(t *testing.T) {
	_, h, db := testServer(t)

	src := seedWMSource(t, db)
	items := []int64{
		seedWMItem(t, db, src, "https://a.test/1"),
		seedWMItem(t, db, src, "https://a.test/2"),
	}
	if err := db.RecordCandidates([]store.CandidateSighting{
		{Name: "Brand New Thing", ItemID: items[0], SourceID: src},
		{Name: "Noise", ItemID: items[1], SourceID: src},
	}); err != nil {
		t.Fatalf("RecordCandidates: %v", err)
	}

	// min_sources=1 so a single-source seed shows up; the default of 3 is the
	// brief's threshold and is exercised by the store tests.
	w := do(t, h, "GET", "/api/candidates?min_sources=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list candidates = %d: %s", w.Code, w.Body.String())
	}
	var list struct {
		Candidates []store.Candidate `json:"candidates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(list.Candidates))
	}

	var newThing, noise int64
	for _, c := range list.Candidates {
		switch c.Name {
		case "Brand New Thing":
			newThing = c.ID
		case "Noise":
			noise = c.ID
		}
	}

	// A layer is required and never guessed: "which layer is this" is the whole
	// judgment the review collects, and a candidate fitting none is itself the
	// signal that a new layer exists.
	if w := do(t, h, "POST", fmt.Sprintf("/api/candidates/%d/promote", newThing), `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("promote without a layer = %d, want 400", w.Code)
	}

	w = do(t, h, "POST", fmt.Sprintf("/api/candidates/%d/promote", newThing),
		`{"layer":"coding_agents"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("promote = %d: %s", w.Code, w.Body.String())
	}
	var entity store.Entity
	if err := json.Unmarshal(w.Body.Bytes(), &entity); err != nil {
		t.Fatalf("decode entity: %v", err)
	}
	if entity.Name != "Brand New Thing" || entity.Layer != "coding_agents" {
		t.Errorf("promoted entity = %+v", entity)
	}

	if w := do(t, h, "DELETE", fmt.Sprintf("/api/candidates/%d", noise), ""); w.Code != http.StatusNoContent {
		t.Errorf("dismiss = %d, want 204", w.Code)
	}

	// Both decisions stick: neither name is offered again.
	w = do(t, h, "GET", "/api/candidates?min_sources=1", "")
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Candidates) != 0 {
		t.Errorf("decided candidates still offered: %+v", list.Candidates)
	}

	// The promoted name now appears on the watchlist.
	w = do(t, h, "GET", "/api/entities", "")
	if !strings.Contains(w.Body.String(), "Brand New Thing") {
		t.Errorf("promoted entity missing from /api/entities: %s", w.Body.String())
	}
}

// Promoting a name that already resolves is a 409 naming the collision, not a
// 500: it is the owner's mistake to see, and a second row for a thing the map
// already answers would split one history in two.
func TestPromoteCandidateConflict(t *testing.T) {
	_, h, db := testServer(t)

	if _, err := db.ImportWatchlist([]store.EntityImport{
		{Name: "Devin", Aliases: []string{"Windsurf"}, Layer: "coding_agents"},
	}); err != nil {
		t.Fatalf("ImportWatchlist: %v", err)
	}
	src := seedWMSource(t, db)
	item := seedWMItem(t, db, src, "https://a.test/1")
	db.RecordCandidates([]store.CandidateSighting{{Name: "windsurf", ItemID: item, SourceID: src}})

	w := do(t, h, "GET", "/api/candidates?min_sources=1", "")
	var list struct {
		Candidates []store.Candidate `json:"candidates"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(list.Candidates))
	}

	w = do(t, h, "POST", fmt.Sprintf("/api/candidates/%d/promote", list.Candidates[0].ID),
		`{"layer":"coding_agents"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("promote a taken alias = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "already resolves") {
		t.Errorf("409 does not name the collision: %s", w.Body.String())
	}
}

// The deltas endpoint is a dry run: it says what the next push would contain
// without sending anything.
func TestDeltasEndpointIsReadOnly(t *testing.T) {
	_, h, db := testServer(t)

	res, err := db.ImportWatchlist([]store.EntityImport{{Name: "Cursor", Layer: "coding_agents"}})
	if err != nil || res.Added != 1 {
		t.Fatalf("ImportWatchlist: %v", err)
	}
	entities, _ := db.Entities()
	src := seedWMSource(t, db)
	item := seedWMItem(t, db, src, "https://a.test/1")
	if _, err := db.Exec(
		`INSERT INTO claims (entity_id, item_id, change_type, source_type, summary, url, hash)
		 VALUES (?, ?, 'launch', 'independent', 'shipped', 'https://a.test/1', 'h1')`,
		entities[0].ID, item,
	); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	w := do(t, h, "GET", "/api/deltas", "")
	if w.Code != http.StatusOK {
		t.Fatalf("deltas = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Cursor") {
		t.Errorf("delta missing: %s", w.Body.String())
	}
	// Reading must not stamp anything as pushed, or the real push would skip it.
	var pushed int
	db.QueryRow(`SELECT count(*) FROM claims WHERE pushed_at IS NOT NULL`).Scan(&pushed)
	if pushed != 0 {
		t.Errorf("reading the dry run marked %d claims pushed", pushed)
	}
}
