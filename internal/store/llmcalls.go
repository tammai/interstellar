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
}

// RecordLLMCall appends to the cost log.
func (db *DB) RecordLLMCall(rec LLMCall) error {
	_, err := db.Exec(
		`INSERT INTO llm_calls (run_date, role, provider, model, tokens_in, tokens_out, ok, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.RunDate, rec.Role, rec.Provider, rec.Model,
		rec.TokensIn, rec.TokensOut, boolToInt(rec.OK), nullIfEmpty(rec.Err),
	)
	if err != nil {
		return fmt.Errorf("record llm call: %w", err)
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
