package store

import (
	"fmt"
	"time"
)

// LLMCall is one call attempt, including failed ones and fallback hops. Recording
// failures matters: a chain that silently falls through to a second vendor is a
// cost and privacy event the owner should be able to see after the fact.
type LLMCall struct {
	RunDate   string
	Role      string
	Provider  string
	Model     string
	TokensIn  int
	TokensOut int
	OK        bool
	Err       string

	// ItemsIn is how many records the prompt asked the model to return: items in a
	// classify batch, matched items in a compose. Zero means the question does not
	// apply -- a chat answer or a transcript summary is not a list of anything --
	// and is stored as NULL so a completeness rate can never pick up a silent zero
	// denominator.
	ItemsIn int
	// Repair marks a second attempt at a reply the first one mangled. Its own
	// column rather than something derived, because "how often does this model
	// need asking twice" is a per-model cost, and the ladder should be able to
	// read it next to the parse rate it pays for.
	Repair bool
}

// RecordLLMCall appends to the cost log and returns the new row's id.
//
// The id is the handle the caller needs to come back and say how the reply turned
// out: the row has to exist before the reply is parsed (an attempt that crashes
// mid-parse still cost money and still belongs in the log), so quality arrives
// second, by id. See RecordReplyQuality.
func (db *DB) RecordLLMCall(rec LLMCall) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO llm_calls
		        (run_date, role, provider, model, tokens_in, tokens_out, ok, error, items_in, repair)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.RunDate, rec.Role, rec.Provider, rec.Model,
		rec.TokensIn, rec.TokensOut, boolToInt(rec.OK), nullIfEmpty(rec.Err),
		nullIfZero(rec.ItemsIn), boolToInt(rec.Repair),
	)
	if err != nil {
		return 0, fmt.Errorf("record llm call: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record llm call: %w", err)
	}
	return id, nil
}

// ReplyQuality is how good a reply turned out to be -- knowable only after it has
// been parsed, which is why it is a second write rather than a column set at
// insert time.
type ReplyQuality struct {
	// ItemsOut is how many usable records came back. Compared against the row's
	// items_in, this is the measurement that catches a model answering one item of
	// thirty without erroring.
	ItemsOut int
	// ParseOK is whether the reply yielded JSON at all. §10's JSON gate, per
	// model, recorded rather than inferred.
	ParseOK bool
}

// RecordReplyQuality attaches a parsed reply's outcome to the attempt that
// produced it.
//
// Keyed by id, and it must be: classify runs several batches concurrently, so
// last_insert_rowid() would attribute one worker's reply to another's call even
// with the pool capped at a single connection.
//
// A callID of 0 is a no-op. That is the case where the insert itself failed, and
// the rule there is the same as everywhere in this file: losing a measurement is
// bad, losing the run over it is worse.
func (db *DB) RecordReplyQuality(callID int64, q ReplyQuality) error {
	if callID == 0 {
		return nil
	}
	_, err := db.Exec(
		`UPDATE llm_calls SET items_out = ?, parse_ok = ? WHERE id = ?`,
		q.ItemsOut, boolToInt(q.ParseOK), callID,
	)
	if err != nil {
		return fmt.Errorf("record reply quality for call %d: %w", callID, err)
	}
	return nil
}

// LLMCallsOn counts calls recorded for a run date. The spend guard reads this on
// every reservation rather than trusting an in-process counter, so a crash loop
// cannot spend the day's budget several times over.
func (db *DB) LLMCallsOn(runDate string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM llm_calls WHERE run_date = ?`, runDate).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count llm calls on %s: %w", runDate, err)
	}
	return n, nil
}

// LLMCallsByRoleOn counts one role's calls on a run date.
//
// Chat has a daily allowance of its own, and the guard reads it here on every
// reservation for the same reason it reads the global count that way: a restart or
// a crash loop must not hand back a fresh budget.
func (db *DB) LLMCallsByRoleOn(runDate, role string) (int, error) {
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM llm_calls WHERE run_date = ? AND role = ?`, runDate, role).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count %s calls on %s: %w", role, runDate, err)
	}
	return n, nil
}

// LLMUsage summarizes spend for a day, per provider and model.
type LLMUsage struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Role      string `json:"role"`
	Calls     int    `json:"calls"`
	Failed    int    `json:"failed"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
}

// LLMUsageOn breaks down a day's calls. Grouped by provider as well as model so
// a fallback firing is visible as its own line rather than blended in.
func (db *DB) LLMUsageOn(runDate string) ([]LLMUsage, error) {
	rows, err := db.Query(
		`SELECT COALESCE(provider, ''), model, role,
		        count(*), sum(CASE WHEN ok = 0 THEN 1 ELSE 0 END),
		        COALESCE(sum(tokens_in), 0), COALESCE(sum(tokens_out), 0)
		   FROM llm_calls
		  WHERE run_date = ?
		  GROUP BY provider, model, role
		  ORDER BY count(*) DESC`,
		runDate,
	)
	if err != nil {
		return nil, fmt.Errorf("llm usage on %s: %w", runDate, err)
	}
	defer rows.Close()

	var out []LLMUsage
	for rows.Next() {
		var u LLMUsage
		if err := rows.Scan(&u.Provider, &u.Model, &u.Role, &u.Calls, &u.Failed, &u.TokensIn, &u.TokensOut); err != nil {
			return nil, fmt.Errorf("scan llm usage: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RunDate formats a time as the calendar date a run belongs to. Local, not UTC:
// the owner's sense of "today" is what the sky reflects, and §7 says the
// schedule honors TZ.
func RunDate(t time.Time) string { return t.Format("2006-01-02") }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullIfZero keeps "not applicable" out of the arithmetic.
//
// A zero items_in written as 0 would join the denominator of every completeness
// rate the moment a chat answer or a test call landed in the same group, dragging
// it toward a number nobody asked. NULL is the honest value for a question that
// was never posed.
func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// ModelReliability is one provider/model/role's record over a window: what it
// cost, and how well it answered.
//
// The pairing is the point. Cost per model has been in this table since day one
// and quality has not, which left §3.1's escalation ladder -- "classifier fails
// the JSON gate -> google flash tier -> anthropic haiku" -- as a runbook with no
// instrument behind it. A model that is cheap and answers 4 of 30 items is not
// cheap.
type ModelReliability struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Role     string `json:"role"`

	Calls  int `json:"calls"`
	Failed int `json:"failed"`
	// Replies counts the attempts that came back and were assessed -- the
	// denominator for ParseRate. It is smaller than Calls by exactly the attempts
	// that never produced text (transport failures) plus the ones whose caller
	// reports no quality at all (chat, transcripts, test calls).
	Replies     int `json:"replies"`
	ParseFailed int `json:"parse_failed"`
	Repairs     int `json:"repairs"`

	ItemsAsked    int `json:"items_asked"`
	ItemsAnswered int `json:"items_answered"`

	TokensIn  int `json:"tokens_in"`
	TokensOut int `json:"tokens_out"`
	// RunDays counts dates with calls, for the same reason TopicHitRate carries
	// it: one bad afternoon and three bad weeks read identically without it.
	RunDays int `json:"run_days"`

	// ParseRate and Completeness are -1 when there is no denominator, matching
	// TopicHitRate.HitRate. A sentinel rather than 0, because "never measured" and
	// "measured and terrible" are the two states this view exists to tell apart,
	// and they both render as 0% otherwise.
	ParseRate    float64 `json:"parse_rate"`
	Completeness float64 `json:"completeness"`
}

// ModelReliabilityOver aggregates the cost log by provider, model and role over
// the last `days` days.
//
// Grouped by provider as well as model for the reason LLMUsageOn is: the same
// model name reached through two providers is two different things to diagnose,
// and a fallback firing must read as its own line rather than blend into the
// primary's numbers.
func (db *DB) ModelReliabilityOver(days int) ([]ModelReliability, error) {
	if days < 1 {
		days = 1
	}

	rows, err := db.Query(
		// items_in/items_out are summed only over rows that were actually
		// assessed. An attempt that died in transport recorded what it asked for
		// and never got a reply, so counting its items_in would charge the model
		// for incompleteness that was really a 500.
		`SELECT COALESCE(provider, ''), model, role,
		        count(*),
		        sum(CASE WHEN ok = 0 THEN 1 ELSE 0 END),
		        sum(CASE WHEN parse_ok IS NOT NULL THEN 1 ELSE 0 END),
		        sum(CASE WHEN parse_ok = 0 THEN 1 ELSE 0 END),
		        COALESCE(sum(repair), 0),
		        COALESCE(sum(CASE WHEN parse_ok IS NOT NULL THEN items_in  END), 0),
		        COALESCE(sum(CASE WHEN parse_ok IS NOT NULL THEN items_out END), 0),
		        COALESCE(sum(tokens_in), 0), COALESCE(sum(tokens_out), 0),
		        count(DISTINCT run_date)
		   FROM llm_calls
		  WHERE run_date >= date('now', 'localtime', ?)
		  GROUP BY provider, model, role
		  ORDER BY count(*) DESC, model, role`,
		fmt.Sprintf("-%d days", days-1),
	)
	if err != nil {
		return nil, fmt.Errorf("model reliability over %d days: %w", days, err)
	}
	defer rows.Close()

	var out []ModelReliability
	for rows.Next() {
		var m ModelReliability
		if err := rows.Scan(&m.Provider, &m.Model, &m.Role,
			&m.Calls, &m.Failed, &m.Replies, &m.ParseFailed, &m.Repairs,
			&m.ItemsAsked, &m.ItemsAnswered,
			&m.TokensIn, &m.TokensOut, &m.RunDays); err != nil {
			return nil, fmt.Errorf("scan model reliability: %w", err)
		}
		m.ParseRate, m.Completeness = -1, -1
		if m.Replies > 0 {
			m.ParseRate = float64(m.Replies-m.ParseFailed) / float64(m.Replies)
		}
		if m.ItemsAsked > 0 {
			m.Completeness = float64(m.ItemsAnswered) / float64(m.ItemsAsked)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Ref is the provider/model the settings screen would name.
func (m ModelReliability) Ref() string {
	if m.Provider == "" {
		return m.Model
	}
	return m.Provider + "/" + m.Model
}

// RoleFailure is a role whose most recent day of calls produced nothing but
// failures.
type RoleFailure struct {
	Role string `json:"role"`
	// RunDate is the day those calls belong to, not today. A failure the owner
	// has not fixed stays reported, and the date is what stops it reading as
	// something that just happened.
	RunDate string `json:"run_date"`
	Failed  int    `json:"failed"`
	// Ref is the provider/model of the last failing attempt, so the message can
	// name what to change rather than only what went wrong.
	Ref   string `json:"ref"`
	Error string `json:"error"`
}

// FailingRoles reports the roles that are wholly broken: on the most recent day
// each one made any call, every call failed.
//
// The gap this fills: readiness only checks that a role's provider resolves and
// that its key is present, which for a KEYLESS provider is always true. An Ollama
// endpoint whose subscription lapsed answers 403 to everything while the settings
// screen reports it ready, and the only symptom is a sky that says "nothing yet
// today". This reads what actually happened instead of what was configured.
//
// "Every call failed" rather than "any call failed" is deliberate. A fallback
// chain doing its job means failures followed by a success -- that is the chain
// working, it is already in the cost log, and a banner for it would cry wolf on
// every transient 500. Nothing succeeding is the state worth interrupting for.
//
// The test role is excluded: its failures are shown inline the moment the button
// is pressed, and re-reporting them as a banner would nag about a diagnosis the
// owner just ran deliberately.
func (db *DB) FailingRoles(since string) ([]RoleFailure, error) {
	// One statement. The correlated subqueries pick the last failure's details
	// without a second round trip -- the pool is capped at one connection, so a
	// follow-up query issued while these rows are open would deadlock.
	rows, err := db.Query(
		`WITH latest AS (
		   SELECT role, max(run_date) AS run_date
		     FROM llm_calls
		    WHERE run_date >= ? AND role <> 'test'
		    GROUP BY role
		 )
		 SELECT c.role, c.run_date,
		        sum(CASE WHEN c.ok = 0 THEN 1 ELSE 0 END) AS failed,
		        COALESCE((SELECT COALESCE(f.provider, '') || '/' || f.model
		                    FROM llm_calls f
		                   WHERE f.role = c.role AND f.run_date = c.run_date AND f.ok = 0
		                   ORDER BY f.id DESC LIMIT 1), '') AS ref,
		        COALESCE((SELECT f.error
		                    FROM llm_calls f
		                   WHERE f.role = c.role AND f.run_date = c.run_date AND f.ok = 0
		                   ORDER BY f.id DESC LIMIT 1), '') AS err
		   FROM llm_calls c
		   JOIN latest l ON l.role = c.role AND l.run_date = c.run_date
		  GROUP BY c.role, c.run_date
		 HAVING sum(CASE WHEN c.ok = 1 THEN 1 ELSE 0 END) = 0
		    AND sum(CASE WHEN c.ok = 0 THEN 1 ELSE 0 END) > 0
		  ORDER BY c.role`,
		since,
	)
	if err != nil {
		return nil, fmt.Errorf("failing roles since %s: %w", since, err)
	}
	defer rows.Close()

	var out []RoleFailure
	for rows.Next() {
		var f RoleFailure
		if err := rows.Scan(&f.Role, &f.RunDate, &f.Failed, &f.Ref, &f.Error); err != nil {
			return nil, fmt.Errorf("scan failing role: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
