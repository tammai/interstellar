package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"encoding/json"
	"slices"

	"github.com/tammai/interstellar/internal/llm"
	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

// weeklyPromptSignals caps how many STORIES reach the round-up prompt.
//
// A week at ~8 a day is under 60, so this is headroom rather than a real limit -- but
// a first run over a backlog can produce far more, and a model handed 300 stories
// writes about the first ten and pads. Ordered by source_count, so what gets cut is
// the least corroborated.
const weeklyPromptSignals = 80

// weeklyListItems caps the rendered list.
//
// The round-up used to be 300 words of prose over a whole week, which at 60 signals
// meant every sentence was six stories averaged together -- "a clear shift into
// production-grade agent engineering accelerated" is what that averaging reads like,
// and it names nothing a person could go and look at. A dozen named stories is more
// information in the same space.
const weeklyListItems = 12

// weeklyNoteRunes caps one item's annotation. The title, the source count and the
// link are printed around it, so the note only has to say what changed.
const weeklyNoteRunes = 260

// weeklyPick is one story the model chose, by id, with its own note.
//
// The model returns ONLY these two fields. Every fact printed in the round-up --
// title, source count, topic, date, link -- is read from the database around the
// note, so the digest cannot name a story that does not exist or attribute one to
// outlets that did not carry it. Asking a model to copy 12 URLs back accurately is
// a bet with no upside: it has the ids, and the ids are enough.
type weeklyPick struct {
	ID   int64  `json:"id"`
	Note string `json:"note"`
}

// weeklyPlan is the whole of the model's answer.
type weeklyPlan struct {
	Lead  string       `json:"lead"`
	Items []weeklyPick `json:"items"`
}

// weeklyStory is one story, once: the best reading of it, plus every topic it matched
// and every signal row that collapsed into it.
//
// The week's signals contain the same story more than once, for two unrelated reasons,
// and the round-up listed it once per row until this existed:
//
//   - Every run composes ITS OWN day's signals and the sky shows seven days, so a
//     story that stays newsworthy gets a fresh row each morning -- same URL, same
//     topic, a reworded title and a different run_date. The round-up listed "Meta
//     releases Muse Glimmer" and "Meta launches Muse Glimmer" as items 1 and 2.
//   - A story matching two topics IS two signals by definition (§4), which is what
//     the sky draws as one shared star. The round-up listed it twice, under each.
//
// Neither is a data bug, and neither is the model's fault: it was handed both rows as
// separate ids and picked both, correctly. The prompt has to offer one story once.
//
// Keyed on primary_url, exactly as groupSignals does on the canvas -- the same
// decision in the same words, because a story is a URL in both places.
type weeklyStory struct {
	store.SearchHit
	// Topics is every topic that matched this story, in the order they were met.
	Topics []string
	// Rows is how many signal rows collapsed in, so a diagnostic can see the fan-in.
	Rows int
}

// weeklyItem is one rendered entry: the story, plus the model's note.
type weeklyItem struct {
	story weeklyStory
	note  string
}

// collapseWeeklyStories folds repeated rows into one story each.
//
// The FIRST row for a URL wins its title, reason and date, and the incoming query is
// ordered source_count DESC, score DESC, run_date DESC -- so the surviving reading is
// the best-corroborated and, among equals, the most recent. Counts take the maximum
// rather than the sum: two rows for one story are two readings of it, not two more
// outlets carrying it, and adding them would invent corroboration that never existed.
func collapseWeeklyStories(hits []store.SearchHit) []weeklyStory {
	out := make([]weeklyStory, 0, len(hits))
	at := make(map[string]int, len(hits))

	for _, h := range hits {
		key := h.URL
		if strings.TrimSpace(key) == "" {
			// No URL to key on: keep it as its own story rather than collapsing every
			// such row into one. Signals get a primary_url from their first member, so
			// this is the rare empty case, not the normal path.
			key = fmt.Sprintf("signal-%d", h.ID)
		}

		i, seen := at[key]
		if !seen {
			at[key] = len(out)
			out = append(out, weeklyStory{SearchHit: h, Topics: topicList(h.TopicTitle), Rows: 1})
			continue
		}

		st := &out[i]
		st.Rows++
		if h.SourceCount > st.SourceCount {
			st.SourceCount = h.SourceCount
		}
		if h.Score > st.Score {
			st.Score = h.Score
		}
		if t := strings.TrimSpace(h.TopicTitle); t != "" && !slices.Contains(st.Topics, t) {
			st.Topics = append(st.Topics, t)
		}
	}
	return out
}

func topicList(title string) []string {
	if t := strings.TrimSpace(title); t != "" {
		return []string{t}
	}
	return nil
}

// WeeklyResult summarizes a round-up pass.
type WeeklyResult struct {
	WeekStart string `json:"week_start"`
	WeekEnd   string `json:"week_end"`
	Signals   int    `json:"signals"`
	Topics    int    `json:"topics"`
	Written   bool   `json:"written"`
	// Skipped says why nothing was written, when nothing was.
	Skipped string `json:"skipped,omitempty"`
}

// WriteWeekly composes the round-up for the week containing `when`.
//
// Runs on the SYNTHESIZER's chain and is logged under its own role. The
// synthesizer's job is already "turn a pile of matched items into a story a person
// reads", which is this task at a different scale -- so it gets the good model
// rather than the cheap one, and its own line in the cost log.
func WriteWeekly(
	ctx context.Context,
	db *store.DB,
	client *llm.Client,
	when time.Time,
	log *slog.Logger,
) (WeeklyResult, error) {
	start, end := store.WeekBounds(when)
	result := WeeklyResult{WeekStart: start, WeekEnd: end}

	signals, err := db.SignalsForWeek(start, end)
	if err != nil {
		return result, err
	}
	result.Signals = len(signals)

	topics := map[int64]bool{}
	for _, s := range signals {
		topics[s.TopicID] = true
	}
	result.Topics = len(topics)

	// A week with nothing in it gets no round-up. Writing "there was no news" costs
	// a call on the good model to say something the absence of a digest already
	// says, and it would occupy the week slot so a later re-run looked done.
	if len(signals) == 0 {
		result.Skipped = "no signals in this week"
		return result, nil
	}

	// Collapsed before anything else sees the list: the cap counts stories, the prompt
	// offers each one once, and the ids the model picks from are story ids.
	stories := collapseWeeklyStories(signals)
	if len(stories) > weeklyPromptSignals {
		stories = stories[:weeklyPromptSignals]
	}

	resp, err := client.CompleteAs(ctx, llm.RoleSynthesizer, llm.RoleWeekly, llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: weeklySystemPrompt},
			{Role: "user", Content: weeklyPrompt(start, end, stories)},
		},
		JSONMode:  true,
		MaxTokens: 1600,
	})
	if err != nil {
		return result, err
	}

	body := strings.TrimSpace(renderWeekly(weeklySelection(resp.Text, stories, log)))
	if body == "" {
		return result, fmt.Errorf("the model returned an empty round-up")
	}

	if _, err := db.SaveWeeklyDigest(store.WeeklyDigest{
		WeekStart: start, WeekEnd: end, Body: body,
		Signals: result.Signals, Topics: result.Topics,
		TokensIn: resp.Usage.TokensIn, TokensOut: resp.Usage.TokensOut,
	}); err != nil {
		return result, err
	}
	result.Written = true

	log.Info("weekly round-up written",
		"week", start, "signals", result.Signals, "topics", result.Topics)
	return result, nil
}

// weeklySystemPrompt asks for a SELECTION, not an essay.
//
// The essay is what it asked for first, and prose over a whole week is a compression
// problem the model solves by averaging: 60 stories into 300 words produces sentences
// like "a clear shift into production-grade agent engineering accelerated", which is
// true of every week in this field and names nothing. Picking twelve stories and
// saying what each one changed is the same call, the same tokens, and actually tells
// the reader what happened.
const weeklySystemPrompt = `You pick and annotate the week's most important stories from someone's own news feed.

You are given the week's signals -- stories their pipeline already judged relevant --
each with an id, the topic it matched, how many independent sources carried it, and
why it mattered.

Return JSON and nothing else:

{"lead": "...", "items": [{"id": 12, "note": "..."}, {"id": 7, "note": "..."}]}

lead: two or three sentences on the single most consequential thing that happened,
NAMED. Not the week in general terms -- if you write "a shift toward", "momentum in",
or "the landscape widened", you have written nothing. Name the release and say what it
changes.

items: the 8 to 12 stories worth this person's attention, MOST CONSEQUENTIAL FIRST:

- Use only ids from the list. Never invent one and never repeat one.
- note is one or two sentences on WHAT CHANGED and what someone can do now that they
  could not last week. Concrete: the name, the licence, the number, the thing that
  shipped.
- Do NOT restate the title. The title is printed directly above your note, so
  repeating it wastes the only line you get. Add to it.
- One item is one story. Never merge several releases into one item to fit more in --
  that is how a round-up turns into a list of trends nobody can act on.
- Source count is your evidence of significance: something four outlets carried
  outranks something one blog did, whatever the titles suggest.
- Leave a story out rather than padding. Eight real ones beat twelve stretched.
- No markdown, no headings, no bullet characters inside the strings.
- Write in English.`

func weeklyPrompt(start, end string, stories []weeklyStory) string {
	var b strings.Builder

	fmt.Fprintf(&b, "WEEK: %s to %s\n\nSTORIES (most corroborated first, each listed once):\n", start, end)

	for _, s := range stories {
		fmt.Fprintf(&b, "\n- id: %d\n", s.ID)
		fmt.Fprintf(&b, "  title: %s\n", s.Title)
		fmt.Fprintf(&b, "  topics: %s · sources: %d · %s\n",
			strings.Join(s.Topics, ", "), s.SourceCount, s.Date)
		if s.Reason != "" {
			fmt.Fprintf(&b, "  why it mattered: %s\n", s.Reason)
		}
	}

	b.WriteString("\nReturn the JSON.")
	return b.String()
}

// weeklySelection turns the model's answer into a lead and an ordered list, dropping
// anything it invented.
//
// Degrades rather than fails, on the same reasoning as the chat planner: the stories
// are already in the database, already ranked by corroboration, and each already
// carries the reason the synthesizer gave it. A model that returns prose instead of
// JSON costs the round-up its lead and its editorial ordering -- it must not cost the
// owner the week.
func weeklySelection(raw string, stories []weeklyStory, log *slog.Logger) (string, []weeklyItem) {
	byID := make(map[int64]weeklyStory, len(stories))
	for _, s := range stories {
		byID[s.ID] = s
	}

	fallback := func(reason string, err error) (string, []weeklyItem) {
		log.Warn("weekly round-up falling back to the stored signals", "reason", reason, "err", err)
		items := make([]weeklyItem, 0, weeklyListItems)
		for _, s := range stories {
			if len(items) == weeklyListItems {
				break
			}
			items = append(items, weeklyItem{story: s, note: s.Reason})
		}
		return "", items
	}

	obj, err := llm.ExtractObject(raw)
	if err != nil {
		return fallback("unparseable output", err)
	}
	var plan weeklyPlan
	if err := json.Unmarshal(obj, &plan); err != nil {
		return fallback("unexpected shape", err)
	}

	items := make([]weeklyItem, 0, len(plan.Items))
	seen := map[int64]bool{}
	for _, pick := range plan.Items {
		// An id that is not in the week is the dangerous case: printed anyway it
		// would carry a real-looking title and link from nowhere. Dropped, in the
		// same spirit as the chat planner's topic whitelist.
		story, ok := byID[pick.ID]
		if !ok || seen[pick.ID] {
			continue
		}
		seen[pick.ID] = true
		note := strings.Join(strings.Fields(pick.Note), " ")
		if note == "" {
			// The model chose the story but said nothing about it. The synthesizer's
			// own reason is a worse note than a good one and a better one than none.
			note = story.Reason
		}
		items = append(items, weeklyItem{story: story, note: sources.Truncate(note, weeklyNoteRunes)})
		if len(items) == weeklyListItems {
			break
		}
	}
	if len(items) == 0 {
		return fallback("no usable ids", nil)
	}

	return strings.Join(strings.Fields(plan.Lead), " "), items
}

// renderWeekly builds the digest body: the model's lead, then one block per story.
//
// Plain text with hard newlines, which is the one format that works in both places
// this is read -- text/plain email, and a .weekly-body element whose CSS is
// white-space: pre-wrap. Everything except the lead and the notes comes from the
// database.
func renderWeekly(lead string, items []weeklyItem) string {
	var b strings.Builder

	if lead != "" {
		b.WriteString(lead)
		b.WriteString("\n\n")
	}

	for i, it := range items {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, it.story.Title)
		if it.note != "" {
			fmt.Fprintf(&b, "   %s\n", it.note)
		}

		meta := []string{plural(it.story.SourceCount, "source", "sources")}
		// Every topic it matched, not just the one whose row survived the collapse.
		// A story crossing two topics is the most interesting thing a week contains,
		// and printing one of them hides exactly that.
		if len(it.story.Topics) > 0 {
			meta = append(meta, strings.Join(it.story.Topics, ", "))
		}
		if it.story.Date != "" {
			meta = append(meta, it.story.Date)
		}
		fmt.Fprintf(&b, "   %s\n", strings.Join(meta, " · "))

		if it.story.URL != "" {
			fmt.Fprintf(&b, "   %s\n", it.story.URL)
		}
	}

	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
