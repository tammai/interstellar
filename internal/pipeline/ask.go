package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// askRetrievalLimit caps how many rows reach the answering prompt.
//
// Not a cost control so much as a quality one: a model handed eighty rows writes
// about the first few and pads the rest, and the padding reads exactly like the
// substance. Twenty-four is enough to answer a week's question and few enough that
// every row can be cited.
const askRetrievalLimit = 24

// askMaxQuestion bounds the question. Anything longer is not a question.
const askMaxQuestion = 2000

// AskResult is one answered question.
type AskResult struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`

	// Query is the plan the model produced, echoed back so the screen can show
	// what was actually searched. A wrong answer is almost always a wrong plan,
	// and hiding the plan makes that undiagnosable.
	Query AskQuery `json:"query"`

	Hits []store.SearchHit `json:"hits"`

	Calls     int   `json:"calls"`
	TokensIn  int   `json:"tokens_in"`
	TokensOut int   `json:"tokens_out"`
	ElapsedMS int64 `json:"elapsed_ms"`

	// Planned reports whether the planner call succeeded. When it fails the
	// question's own words are used as the search, which still answers most
	// questions -- but the screen should not claim a plan it did not get.
	Planned bool `json:"planned"`
}

// AskQuery is the typed plan a question is turned into.
//
// Deliberately not SQL. A model that can only fill in these fields cannot invent
// a join, drop a filter, or read a table it was not meant to -- and, more to the
// point, cannot produce a query that looks right and quietly answers a different
// question. Every field is validated before it reaches the database.
type AskQuery struct {
	Text      string  `json:"text"`
	Kind      string  `json:"kind"`
	TopicIDs  []int64 `json:"topic_ids,omitempty"`
	From      string  `json:"from,omitempty"`
	To        string  `json:"to,omitempty"`
	Rationale string  `json:"rationale,omitempty"`
}

// Asker answers questions over the owner's own signals and items.
//
// Retrieval is local and takes milliseconds; the model calls are the whole
// latency. That asymmetry is why the plan is a separate cheap call rather than
// one big prompt carrying the entire corpus: the corpus does not fit, and
// searching it properly costs less than stuffing a fraction of it.
type Asker struct {
	db  *store.DB
	log *slog.Logger
	now func() time.Time
}

// NewAsker builds an Asker. now is injectable because "last week" has to resolve
// against a known date in tests.
func NewAsker(db *store.DB, log *slog.Logger, now func() time.Time) *Asker {
	if now == nil {
		now = time.Now
	}
	return &Asker{db: db, log: log, now: now}
}

// Ask plans a search, runs it, and answers from what it found.
func (a *Asker) Ask(ctx context.Context, client *llm.Client, question string) (AskResult, error) {
	started := a.now()

	question = strings.TrimSpace(question)
	if question == "" {
		return AskResult{}, fmt.Errorf("question is empty")
	}
	if len(question) > askMaxQuestion {
		question = sources.Truncate(question, askMaxQuestion)
	}

	result := AskResult{Question: question}

	topics, err := a.db.AllTopics()
	if err != nil {
		return result, err
	}

	// --- 1. plan ---
	//
	// Runs on the CLASSIFIER's chain but is logged and capped as chat. The
	// classifier is already the cheap-and-fast role by design (§3.1's cheap-first
	// posture), which is exactly what a short JSON-out call wants, and CompleteAs
	// is the same seam the Settings test button uses. The owner gets one model
	// setting to think about, not two.
	plan, planned, planUsage, err := a.plan(ctx, client, question, topics)
	result.Planned = planned
	result.Query = plan
	result.Calls++
	result.TokensIn += planUsage.TokensIn
	result.TokensOut += planUsage.TokensOut
	if err != nil {
		// A cap breach or a cancellation. Neither is something to degrade around:
		// the answer call would hit the same wall, so stop here rather than run a
		// retrieval whose results nobody will ever see.
		return result, err
	}

	// --- 2. retrieve ---
	hits, err := a.db.Search(store.SearchQuery{
		Text:     plan.Text,
		Kind:     plan.Kind,
		TopicIDs: plan.TopicIDs,
		From:     plan.From,
		To:       plan.To,
		Limit:    askRetrievalLimit,
	})
	if err != nil {
		return result, err
	}
	result.Hits = hits

	// Nothing found is an answer, and a cheap one. Spending a second call to have
	// a model say "I could not find anything" is pure waste, and it would say it
	// less clearly.
	if len(hits) == 0 {
		result.Answer = a.nothingFound(plan)
		result.ElapsedMS = a.now().Sub(started).Milliseconds()
		return result, nil
	}

	// --- 3. answer ---
	resp, err := client.Complete(ctx, llm.RoleChat, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: answerSystemPrompt},
			{Role: "user", Content: answerPrompt(question, hits, a.now())},
		},
	})
	result.Calls++
	result.TokensIn += resp.Usage.TokensIn
	result.TokensOut += resp.Usage.TokensOut
	if err != nil {
		return result, err
	}

	result.Answer = strings.TrimSpace(resp.Text)
	result.ElapsedMS = a.now().Sub(started).Milliseconds()
	return result, nil
}

// plan asks for a search plan, falling back to the question's own words.
//
// A soft failure here is deliberately not fatal. Keyword search over the raw
// question answers most questions perfectly well, so a planner that returns prose
// instead of JSON, or that a provider 500s, should degrade the result rather than
// deny it -- the alternative is a chat screen that stops working entirely because
// a cheap auxiliary call went wrong.
//
// A returned error means the opposite: a hard stop the answer call would also hit
// -- the spend cap, or a cancelled request. Degrading there would burn a retrieval
// to produce nothing.
func (a *Asker) plan(
	ctx context.Context,
	client *llm.Client,
	question string,
	topics []store.Topic,
) (AskQuery, bool, llm.Usage, error) {
	fallback := AskQuery{Text: question, Rationale: "the question's own words"}

	resp, err := client.CompleteAs(ctx, llm.RoleClassifier, llm.RoleChat, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: planSystemPrompt},
			{Role: "user", Content: planPrompt(question, topics, a.now())},
		},
		JSONMode:  true,
		MaxTokens: 400,
		// A plan is one record. Expect/ReportReply here is not about volume -- it
		// is the JSON gate for whichever model the classifier chain is pointed at,
		// measured on the traffic that runs all day rather than only at 10:30.
		Expect: 1,
	})
	if err != nil {
		if errors.Is(err, llm.ErrCapReached) || errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) || errors.Is(err, llm.ErrRoleNotConfigured) {
			return fallback, false, resp.Usage, err
		}
		a.log.Warn("chat planner failed, falling back to keyword search", "err", err)
		return fallback, false, resp.Usage, nil
	}

	var plan AskQuery
	obj, err := llm.ExtractObject(resp.Text)
	if err != nil {
		a.log.Warn("chat planner returned unparseable output", "err", err)
		client.ReportReply(resp.CallID, 0, false)
		return fallback, false, resp.Usage, nil
	}
	if err := json.Unmarshal(obj, &plan); err != nil {
		a.log.Warn("chat planner returned unexpected shape", "err", err)
		// Valid JSON of the wrong shape is a parse failure for this purpose. The
		// gate is "did the model return the thing that was asked for", and JSON
		// that cannot be decoded into a plan is as unusable as prose.
		client.ReportReply(resp.CallID, 0, false)
		return fallback, false, resp.Usage, nil
	}

	client.ReportReply(resp.CallID, 1, true)
	return a.sanitizePlan(plan, question, topics), true, resp.Usage, nil
}

// sanitizePlan is the whitelist. Nothing a model returns reaches the database
// without passing through here.
func (a *Asker) sanitizePlan(plan AskQuery, question string, topics []store.Topic) AskQuery {
	// An empty text with no filters would retrieve the newest rows regardless of
	// what was asked, which looks like an answer and is not one.
	if strings.TrimSpace(plan.Text) == "" {
		plan.Text = question
	}

	switch plan.Kind {
	case "signals", "items":
	default:
		plan.Kind = "" // both
	}

	// Topic ids must exist. A hallucinated id would silently filter everything out
	// and produce a confident "nothing happened".
	known := make(map[int64]bool, len(topics))
	for _, t := range topics {
		known[t.ID] = true
	}
	var keep []int64
	for _, id := range plan.TopicIDs {
		if known[id] {
			keep = append(keep, id)
		}
	}
	plan.TopicIDs = keep

	if !isDate(plan.From) {
		plan.From = ""
	}
	if !isDate(plan.To) {
		plan.To = ""
	}
	// A reversed window matches nothing. Swapping is friendlier than dropping and
	// is almost always what was meant.
	if plan.From != "" && plan.To != "" && plan.From > plan.To {
		plan.From, plan.To = plan.To, plan.From
	}

	plan.Rationale = strings.TrimSpace(plan.Rationale)
	if len(plan.Rationale) > 300 {
		plan.Rationale = sources.Truncate(plan.Rationale, 300)
	}
	return plan
}

func (a *Asker) nothingFound(plan AskQuery) string {
	var b strings.Builder
	b.WriteString("Nothing in your signals matches that.")
	if plan.From != "" || plan.To != "" {
		b.WriteString(" The search was limited to ")
		switch {
		case plan.From != "" && plan.To != "":
			fmt.Fprintf(&b, "%s to %s", plan.From, plan.To)
		case plan.From != "":
			fmt.Fprintf(&b, "%s onwards", plan.From)
		default:
			fmt.Fprintf(&b, "up to %s", plan.To)
		}
		b.WriteString(", so a wider window may help.")
	} else {
		b.WriteString(" Try different words, or check that a run has produced signals for the topic.")
	}
	return b.String()
}

// isDate accepts a strict YYYY-MM-DD. A loose date would reach a string
// comparison against run_date and silently match nothing.
func isDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

const planSystemPrompt = `You turn a question about someone's news feed into a search plan.

Reply with ONLY a JSON object:
{"text": "<keywords to search for>",
 "kind": "signals" | "items" | "",
 "topic_ids": [<numbers>],
 "from": "YYYY-MM-DD",
 "to": "YYYY-MM-DD",
 "rationale": "<one short clause naming what you narrowed to>"}

Rules:
- "text" carries the words worth matching. Strip question words and filler; keep
  the nouns, names and jargon that would actually appear in a headline. Add close
  synonyms when the question is conceptual, because matching is by word.
- "kind": "signals" for what mattered and why (the default, and what the user
  sees as their star map), "items" for individual articles, "" for both.
- "topic_ids" only when the question is clearly about one of the listed topics.
  Omit it otherwise -- a wrong topic filter hides the answer completely.
- "from"/"to" only when the question refers to time. Omit both otherwise.
- Every field is optional except "text". Omit rather than guess.

No preamble, no code fences.`

func planPrompt(question string, topics []store.Topic, now time.Time) string {
	var b strings.Builder

	fmt.Fprintf(&b, "TODAY: %s (%s)\n\n", now.Format("2006-01-02"), now.Format("Monday"))

	b.WriteString("THE USER'S TOPICS:\n")
	for _, t := range topics {
		if !t.Active {
			continue
		}
		fmt.Fprintf(&b, "  id %d: %s\n", t.ID, t.Title)
	}

	fmt.Fprintf(&b, "\nQUESTION:\n%s\n\nReturn the JSON object only.", question)
	return b.String()
}

const answerSystemPrompt = `You answer questions about someone's own curated news feed,
using only the search results you are given.

Rules:
- Answer from the results, and nothing else. You have no other knowledge of these
  events. If the results do not support an answer, say so plainly and stop.
- Cite as you go, using the bracketed number of the result: "Anthropic shipped X [3]".
  Every specific claim needs one. The user checks these.
- Lead with the answer. No preamble, no restating the question.
- Be brief -- a few sentences, or a short list when the answer really is a list.
- If the results disagree, say they disagree rather than picking one.
- Never invent a source, a date, or a number that is not in the results.
- Write in English.

No code fences.`

func answerPrompt(question string, hits []store.SearchHit, now time.Time) string {
	var b strings.Builder

	fmt.Fprintf(&b, "TODAY: %s\n\nRESULTS:\n", now.Format("2006-01-02"))

	for i, h := range hits {
		fmt.Fprintf(&b, "\n[%d] %s\n", i+1, h.Title)
		fmt.Fprintf(&b, "    date: %s\n", h.Date)
		if h.TopicTitle != "" {
			fmt.Fprintf(&b, "    topic: %s\n", h.TopicTitle)
		}
		if h.Kind == "signal" {
			fmt.Fprintf(&b, "    sources: %d\n", h.SourceCount)
			if h.Reason != "" {
				fmt.Fprintf(&b, "    why it mattered: %s\n", h.Reason)
			}
		}
		if h.SourceName != "" {
			fmt.Fprintf(&b, "    source: %s\n", h.SourceName)
		}
	}

	fmt.Fprintf(&b, "\nQUESTION:\n%s\n\nAnswer using only the results above, citing [n] as you go.", question)
	return b.String()
}
