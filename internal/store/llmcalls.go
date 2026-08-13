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
