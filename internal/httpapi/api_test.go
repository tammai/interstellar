package httpapi

import (
	"encoding/json"
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
		{"GET", "/api/sources", ""},
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

	if len(got.Topics) != 7 {
		t.Errorf("got %d topics, want the 7 seeded", len(got.Topics))
	}
	if got.Signals == nil {
		t.Error("signals should be an empty array, not null -- the UI iterates it")
	}
	if got.HotThreshold != 3 {
		t.Errorf("hot_threshold = %d, want 3", got.HotThreshold)
	}
	// Seeded topics carry the prototype's coordinates, so the sky renders as the
	// approved mock promised.
	for _, tp := range got.Topics {
		if tp.PosX == 0 && tp.PosY == 0 {
			t.Errorf("topic %q has no constellation position", tp.Title)
		}
		if !tp.NeedsSetup {
			t.Errorf("seeded topic %q should report needs_setup", tp.Title)
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
	if !strings.Contains(got.SetupHint, "DEEPSEEK_API_KEY") {
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
	_, h, _ := testServer(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-present") // after testServer; see above

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
		if err := db.RecordLLMCall(store.LLMCall{
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
	if got.Provider != "deepseek" {
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
	}, ""); err != nil {
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
	}, ""); err != nil {
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
