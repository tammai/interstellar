package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/store"
)

// fixedNow pins "today" so a plan mentioning a date window is testable.
func fixedNow() func() time.Time {
	t := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// askCorpus seeds two topics and three signals to retrieve.
func askCorpus(t *testing.T, db *store.DB) (agents, voice int64) {
	t.Helper()
	agents = seedTopic(t, db, "Agents", "Agent harnesses.")
	voice = seedTopic(t, db, "Voice AI", "Speech models.")
	src := seedSource(t, db, "Blog")

	for _, s := range []struct {
		date  string
		topic int64
		title string
	}{
		{"2026-08-12", agents, "Agent harness patterns converge"},
		{"2026-08-11", agents, "Tool loops get a standard"},
		{"2026-08-10", voice, "Speech models learn to interrupt"},
	} {
		item := seedItem(t, db, src, "https://x.test/"+s.title, s.title)
		if err := db.SaveMatches([]store.Match{{ItemID: item, TopicID: s.topic, Score: 5, Reason: "on topic"}}); err != nil {
			t.Fatalf("save match: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO signals (run_date, topic_id, title, reason, score, source_count, primary_url)
			 VALUES (?, ?, ?, 'Several outlets covered it.', 5, 3, 'https://x.test/a')`,
			s.date, s.topic, s.title,
		); err != nil {
			t.Fatalf("insert signal: %v", err)
		}
	}
	return agents, voice
}

// askModel routes the two prompts a question makes: the planner asks for JSON,
// the answerer asks for prose.
func askModel(t *testing.T, plan string, answer string) *fakeModel {
	t.Helper()
	return newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "search plan") {
			return plan
		}
		return answer
	})
}

func TestAskPlansRetrievesAndAnswers(t *testing.T) {
	db := testDB(t)
	agents, _ := askCorpus(t, db)

	plan := fmt.Sprintf(`{"text":"agent harness","kind":"signals","topic_ids":[%d],"rationale":"the Agents topic"}`, agents)
	model := askModel(t, plan, "Harness patterns converged across three outlets [1].")

	asker := NewAsker(db, quietLogger(), fixedNow())
	got, err := asker.Ask(context.Background(), clientFor(t, db, model), "what happened with agent harnesses?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if !got.Planned {
		t.Error("Planned is false despite a good plan")
	}
	if got.Query.Text != "agent harness" {
		t.Errorf("plan text = %q", got.Query.Text)
	}
	if len(got.Query.TopicIDs) != 1 || got.Query.TopicIDs[0] != agents {
		t.Errorf("plan topics = %v, want [%d]", got.Query.TopicIDs, agents)
	}
	if len(got.Hits) == 0 {
		t.Fatal("no hits retrieved")
	}
	for _, h := range got.Hits {
		if h.TopicID != agents {
			t.Errorf("the topic filter did not apply: got topic %d", h.TopicID)
		}
	}
	if !strings.Contains(got.Answer, "[1]") {
		t.Errorf("answer has no citation: %q", got.Answer)
	}
	// Two calls: plan, then answer.
	if got.Calls != 2 {
		t.Errorf("calls = %d, want 2", got.Calls)
	}
	if got.TokensIn == 0 || got.TokensOut == 0 {
		t.Errorf("token accounting is empty: in=%d out=%d", got.TokensIn, got.TokensOut)
	}
}

// Both calls must be logged under the chat role, even though the planner runs on
// the classifier's chain. Otherwise chat spend hides inside classifier spend and
// the per-role cap cannot see it.
func TestAskLogsEveryCallAsChat(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	model := askModel(t, `{"text":"agent"}`, "An answer [1].")
	asker := NewAsker(db, quietLogger(), fixedNow())
	if _, err := asker.Ask(context.Background(), clientFor(t, db, model), "agents?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if n := countRows(t, db, `SELECT count(*) FROM llm_calls WHERE role = 'chat'`); n != 2 {
		t.Errorf("%d calls logged as chat, want 2", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM llm_calls WHERE role = 'classifier'`); n != 0 {
		t.Errorf("%d calls logged as classifier; the planner must not bill the pipeline's role", n)
	}
}

// A planner that returns prose is a soft failure: the question's own words still
// make a usable search, and denying the answer would be worse.
func TestAskFallsBackWhenThePlannerFails(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "search plan") {
			return "I'm afraid I can't do that."
		}
		return "Still answered [1]."
	})

	asker := NewAsker(db, quietLogger(), fixedNow())
	got, err := asker.Ask(context.Background(), clientFor(t, db, model), "agent harness patterns")
	if err != nil {
		t.Fatalf("Ask should survive a bad plan: %v", err)
	}
	if got.Planned {
		t.Error("Planned is true after the planner failed to produce JSON")
	}
	if got.Query.Text != "agent harness patterns" {
		t.Errorf("fallback should search the question itself, got %q", got.Query.Text)
	}
	if len(got.Hits) == 0 {
		t.Error("the fallback search found nothing")
	}
	if got.Answer == "" {
		t.Error("no answer produced")
	}
}

// The whitelist. A model may return anything; none of it reaches the database
// unchecked.
func TestAskSanitizesTheModelsPlan(t *testing.T) {
	db := testDB(t)
	agents, _ := askCorpus(t, db)

	// A hallucinated topic id, a nonsense kind, a reversed window and junk dates.
	plan := fmt.Sprintf(
		`{"text":"agent","kind":"everything","topic_ids":[%d,99999],"from":"2026-08-12","to":"2026-08-10","rationale":"%s"}`,
		agents, strings.Repeat("x", 500))
	model := askModel(t, plan, "Answer [1].")

	asker := NewAsker(db, quietLogger(), fixedNow())
	got, err := asker.Ask(context.Background(), clientFor(t, db, model), "agents?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if got.Query.Kind != "" {
		t.Errorf(`kind = %q, want "" — an unknown kind must fall back to both`, got.Query.Kind)
	}
	if len(got.Query.TopicIDs) != 1 || got.Query.TopicIDs[0] != agents {
		t.Errorf("topic ids = %v; the hallucinated id must be dropped", got.Query.TopicIDs)
	}
	// A hallucinated id left in place would filter everything out and produce a
	// confident "nothing happened", which is the worst possible failure here.
	if len(got.Hits) == 0 {
		t.Error("no hits: the bad topic id was not dropped")
	}
	if got.Query.From != "2026-08-10" || got.Query.To != "2026-08-12" {
		t.Errorf("reversed window not swapped: from=%q to=%q", got.Query.From, got.Query.To)
	}
	if len(got.Query.Rationale) > 300 {
		t.Errorf("rationale not bounded: %d chars", len(got.Query.Rationale))
	}
}

func TestAskDropsMalformedDates(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	model := askModel(t, `{"text":"agent","from":"last week","to":"2026-13-45"}`, "Answer [1].")
	asker := NewAsker(db, quietLogger(), fixedNow())
	got, err := asker.Ask(context.Background(), clientFor(t, db, model), "agents?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Query.From != "" || got.Query.To != "" {
		t.Errorf("malformed dates survived: from=%q to=%q", got.Query.From, got.Query.To)
	}
	if len(got.Hits) == 0 {
		t.Error("no hits: a bad date bound was passed through to the query")
	}
}

// Nothing found is an answer. Paying a model to say so is waste, and it would say
// it less clearly than a sentence naming the window that was searched.
func TestAskAnswersNothingFoundWithoutASecondCall(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	model := askModel(t, `{"text":"kubernetes helm sharding","kind":"signals"}`, "SHOULD NOT BE CALLED")
	asker := NewAsker(db, quietLogger(), fixedNow())
	got, err := asker.Ask(context.Background(), clientFor(t, db, model), "anything about helm?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if len(got.Hits) != 0 {
		t.Fatalf("expected no hits, got %d", len(got.Hits))
	}
	if got.Calls != 1 {
		t.Errorf("calls = %d, want 1 — no answer call when there is nothing to answer from", got.Calls)
	}
	if model.callCount() != 1 {
		t.Errorf("model called %d times, want 1", model.callCount())
	}
	if !strings.Contains(strings.ToLower(got.Answer), "nothing") {
		t.Errorf("answer should say nothing matched, got %q", got.Answer)
	}
	if strings.Contains(got.Answer, "SHOULD NOT BE CALLED") {
		t.Error("the answering model was called for an empty result set")
	}
}

// A window mentioned in the "nothing found" sentence is what tells the reader the
// search was narrow rather than the corpus empty.
func TestAskNothingFoundNamesTheWindow(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	model := askModel(t, `{"text":"helm","from":"2026-01-01","to":"2026-01-31"}`, "unused")
	asker := NewAsker(db, quietLogger(), fixedNow())
	got, _ := asker.Ask(context.Background(), clientFor(t, db, model), "helm in january?")

	if !strings.Contains(got.Answer, "2026-01-01") || !strings.Contains(got.Answer, "2026-01-31") {
		t.Errorf("answer does not name the window searched: %q", got.Answer)
	}
}

// The cap is a hard stop, not something to degrade around: the answer call would
// hit the same wall, so no retrieval should happen and the error must surface.
func TestAskStopsOnTheChatCap(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	model := askModel(t, `{"text":"agent"}`, "unused")
	client := clientForChatCapped(t, db, model, 0, 1) // one chat call already spent

	// Pre-spend the allowance so the very first reservation is refused.
	if _, err := db.RecordLLMCall(store.LLMCall{RunDate: store.RunDate(time.Now()), Role: "chat", Model: "m", OK: true}); err != nil {
		t.Fatal(err)
	}

	asker := NewAsker(db, quietLogger(), fixedNow())
	got, err := asker.Ask(context.Background(), client, "agents?")
	if !errors.Is(err, llm.ErrCapReached) {
		t.Fatalf("error = %v, want ErrCapReached", err)
	}
	if len(got.Hits) != 0 {
		t.Error("a retrieval ran despite the cap having stopped the question")
	}
}

func TestAskRejectsAnEmptyQuestion(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	model := askModel(t, `{"text":"x"}`, "unused")
	asker := NewAsker(db, quietLogger(), fixedNow())
	if _, err := asker.Ask(context.Background(), clientFor(t, db, model), "   "); err == nil {
		t.Error("an empty question should be refused, not sent to a model")
	}
	if model.callCount() != 0 {
		t.Errorf("model called %d times for an empty question", model.callCount())
	}
}

// The planner is told today's date, or "last week" cannot be resolved.
func TestAskTellsThePlannerTheDate(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	var planPrompt string
	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "search plan") {
			planPrompt = prompt
			return `{"text":"agent"}`
		}
		return "Answer [1]."
	})

	asker := NewAsker(db, quietLogger(), fixedNow())
	if _, err := asker.Ask(context.Background(), clientFor(t, db, model), "what happened last week?"); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !strings.Contains(planPrompt, "2026-08-13") {
		t.Error("the planner prompt does not carry today's date")
	}
	if !strings.Contains(planPrompt, "Agents") {
		t.Error("the planner prompt does not list the topics it may filter by")
	}
}

// The answering model must be given numbered results, because the citations it is
// asked to produce refer to those numbers.
func TestAskGivesTheAnswererNumberedResults(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	var answerPrompt string
	model := newFakeModel(t, func(prompt string) string {
		if strings.Contains(prompt, "search plan") {
			return `{"text":"agent harness","kind":"signals"}`
		}
		answerPrompt = prompt
		return "Answer [1]."
	})

	asker := NewAsker(db, quietLogger(), fixedNow())
	got, err := asker.Ask(context.Background(), clientFor(t, db, model), "agents?")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !strings.Contains(answerPrompt, "[1]") {
		t.Error("results are not numbered in the answering prompt")
	}
	// Every retrieved hit must be numbered, or a cited [n] points at nothing.
	for i := range got.Hits {
		if !strings.Contains(answerPrompt, fmt.Sprintf("[%d]", i+1)) {
			t.Errorf("hit %d is missing its number in the prompt", i+1)
		}
	}
	if !strings.Contains(answerPrompt, "only the results above") {
		t.Error("the answering prompt does not confine the model to the results")
	}
}

// --- storage ---

func TestChatTurnRoundTrips(t *testing.T) {
	db := testDB(t)

	query, _ := json.Marshal(AskQuery{Text: "agent harness", Kind: "signals"})
	id, err := db.SaveChatTurn(store.ChatTurn{
		RunDate:   "2026-08-13",
		Question:  "what happened with agents?",
		Answer:    "Harness patterns converged [1].",
		Query:     query,
		Planned:   true,
		HitRefs:   []store.HitRef{{Kind: "signal", ID: 7}, {Kind: "item", ID: 9}},
		HitCount:  2,
		Calls:     2,
		TokensIn:  120,
		TokensOut: 40,
		ElapsedMS: 2500,
	})
	if err != nil {
		t.Fatalf("SaveChatTurn: %v", err)
	}

	got, err := db.ChatTurn(id)
	if err != nil {
		t.Fatalf("ChatTurn: %v", err)
	}
	if got.Question != "what happened with agents?" || got.Answer == "" {
		t.Errorf("round trip lost content: %+v", got)
	}
	if !got.Planned {
		t.Error("planned did not survive")
	}
	if len(got.HitRefs) != 2 || got.HitRefs[0].Kind != "signal" || got.HitRefs[0].ID != 7 {
		t.Errorf("hit refs = %+v", got.HitRefs)
	}
	if got.Calls != 2 || got.TokensIn != 120 || got.TokensOut != 40 || got.ElapsedMS != 2500 {
		t.Errorf("accounting lost: %+v", got)
	}

	var plan AskQuery
	if err := json.Unmarshal(got.Query, &plan); err != nil {
		t.Fatalf("stored query does not decode: %v", err)
	}
	if plan.Text != "agent harness" {
		t.Errorf("stored plan = %+v", plan)
	}
}

func TestChatTurnsAreNewestFirstAndBounded(t *testing.T) {
	db := testDB(t)

	for i := range 5 {
		if _, err := db.SaveChatTurn(store.ChatTurn{
			RunDate: "2026-08-13", Question: fmt.Sprintf("q%d", i), Answer: "a",
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	turns, err := db.ChatTurns(3)
	if err != nil {
		t.Fatalf("ChatTurns: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(turns))
	}
	if turns[0].Question != "q4" {
		t.Errorf("first turn is %q, want the newest (q4)", turns[0].Question)
	}
	for i := 1; i < len(turns); i++ {
		if turns[i-1].ID < turns[i].ID {
			t.Error("turns are not newest-first")
		}
	}

	if n, err := db.ChatTurnsOn("2026-08-13"); err != nil || n != 5 {
		t.Errorf("ChatTurnsOn = %d, %v; want 5", n, err)
	}
}

// A turn with unreadable refs is still a real question and answer. Losing the row
// over its citations would lose more than it saves.
func TestChatTurnSurvivesCorruptRefs(t *testing.T) {
	db := testDB(t)

	if _, err := db.Exec(
		`INSERT INTO chat_turns (run_date, question, answer, hit_refs, hit_count)
		 VALUES ('2026-08-13', 'q', 'a', 'not json at all', 3)`); err != nil {
		t.Fatal(err)
	}

	turns, err := db.ChatTurns(10)
	if err != nil {
		t.Fatalf("ChatTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("got %d turns, want 1 — a bad refs blob dropped the whole row", len(turns))
	}
	if turns[0].Question != "q" || turns[0].Answer != "a" {
		t.Errorf("content lost: %+v", turns[0])
	}
	if turns[0].HitRefs != nil {
		t.Errorf("refs should be nil, got %+v", turns[0].HitRefs)
	}
}

// Truncation must never cut a rune in half.
//
// §9 keeps topic definitions in Vietnamese and Vietnamese feeds carry Vietnamese
// titles; on that text 27% of byte offsets fall inside a multi-byte rune. json
// encoding does not error on invalid UTF-8, it substitutes U+FFFD — so the damage
// arrives as mojibake in the database and on screen rather than as a failure.
func TestQuestionAndPlanTruncationStayValidUTF8(t *testing.T) {
	db := testDB(t)
	askCorpus(t, db)

	// Longer than askMaxQuestion, all multi-byte.
	long := strings.Repeat("Tiếng Việt có dấu ", 200)
	if len(long) <= askMaxQuestion {
		t.Fatalf("fixture is too short to exercise truncation: %d bytes", len(long))
	}

	rationale := strings.Repeat("Chủ đề tiếng Việt ", 40)
	plan := fmt.Sprintf(`{"text":"agent","rationale":%q}`, rationale)
	model := askModel(t, plan, "Answer [1].")

	asker := NewAsker(db, quietLogger(), fixedNow())
	got, err := asker.Ask(context.Background(), clientFor(t, db, model), long)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if !utf8.ValidString(got.Question) {
		t.Error("the truncated question is not valid UTF-8")
	}
	if !utf8.ValidString(got.Query.Rationale) {
		t.Error("the truncated rationale is not valid UTF-8")
	}
	// And it survives the encoding the API actually uses.
	enc, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(enc), "�") {
		t.Error("the JSON response contains a replacement character; a rune was cut in half")
	}
}
