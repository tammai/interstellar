package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// HitRef points at something a turn retrieved, without copying it.
//
// Signals are rewritten on every compose, so a stored copy would drift away from
// the sky it claims to cite. A reference that has since been replaced is honestly
// a dead link, which is better than a stale one that looks live.
type HitRef struct {
	Kind string `json:"kind"` // signal | item
	ID   int64  `json:"id"`
}

// ChatTurn is one answered question, as stored.
type ChatTurn struct {
	ID       int64  `json:"id"`
	RunDate  string `json:"run_date"`
	Question string `json:"question"`
	Answer   string `json:"answer"`

	// Query is the resolved plan, as raw JSON so this layer stays ignorant of the
	// pipeline's AskQuery shape.
	Query   json.RawMessage `json:"query,omitempty"`
	Planned bool            `json:"planned"`

	HitRefs  []HitRef `json:"hit_refs,omitempty"`
	HitCount int      `json:"hit_count"`

	Calls     int   `json:"calls"`
	TokensIn  int   `json:"tokens_in"`
	TokensOut int   `json:"tokens_out"`
	ElapsedMS int64 `json:"elapsed_ms"`

	CreatedAt string `json:"created_at"`
}

// SaveChatTurn appends a turn and returns its id.
func (db *DB) SaveChatTurn(t ChatTurn) (int64, error) {
	var refs any
	if len(t.HitRefs) > 0 {
		encoded, err := json.Marshal(t.HitRefs)
		if err != nil {
			return 0, fmt.Errorf("encode chat hit refs: %w", err)
		}
		refs = string(encoded)
	}

	var query any
	if len(t.Query) > 0 {
		query = string(t.Query)
	}

	res, err := db.Exec(
		`INSERT INTO chat_turns
		        (run_date, question, answer, query, planned,
		         hit_refs, hit_count, calls, tokens_in, tokens_out, elapsed_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.RunDate, t.Question, t.Answer, query, boolToInt(t.Planned),
		refs, t.HitCount, t.Calls, t.TokensIn, t.TokensOut, t.ElapsedMS,
	)
	if err != nil {
		return 0, fmt.Errorf("save chat turn: %w", err)
	}
	return res.LastInsertId()
}

// ChatTurns returns the most recent turns, newest first.
func (db *DB) ChatTurns(limit int) ([]ChatTurn, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := db.Query(
		`SELECT id, run_date, question, answer,
		        COALESCE(query, ''), planned,
		        COALESCE(hit_refs, ''), hit_count,
		        calls, tokens_in, tokens_out, elapsed_ms, created_at
		   FROM chat_turns
		  ORDER BY id DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read chat turns: %w", err)
	}
	defer rows.Close()

	var out []ChatTurn
	for rows.Next() {
		var (
			t       ChatTurn
			query   string
			refs    string
			planned int
		)
		if err := rows.Scan(&t.ID, &t.RunDate, &t.Question, &t.Answer,
			&query, &planned, &refs, &t.HitCount,
			&t.Calls, &t.TokensIn, &t.TokensOut, &t.ElapsedMS, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan chat turn: %w", err)
		}
		t.Planned = planned != 0
		if query != "" {
			t.Query = json.RawMessage(query)
		}
		if refs != "" {
			// A turn whose refs will not decode is still a real question and answer;
			// dropping the whole row over its citations would lose more than it saves.
			if err := json.Unmarshal([]byte(refs), &t.HitRefs); err != nil {
				t.HitRefs = nil
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ChatTurn returns one turn by id.
func (db *DB) ChatTurn(id int64) (ChatTurn, error) {
	var (
		t       ChatTurn
		query   string
		refs    string
		planned int
	)
	err := db.QueryRow(
		`SELECT id, run_date, question, answer,
		        COALESCE(query, ''), planned,
		        COALESCE(hit_refs, ''), hit_count,
		        calls, tokens_in, tokens_out, elapsed_ms, created_at
		   FROM chat_turns WHERE id = ?`, id,
	).Scan(&t.ID, &t.RunDate, &t.Question, &t.Answer,
		&query, &planned, &refs, &t.HitCount,
		&t.Calls, &t.TokensIn, &t.TokensOut, &t.ElapsedMS, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatTurn{}, ErrNotFound
	}
	if err != nil {
		return ChatTurn{}, fmt.Errorf("read chat turn %d: %w", id, err)
	}

	t.Planned = planned != 0
	if query != "" {
		t.Query = json.RawMessage(query)
	}
	if refs != "" {
		if err := json.Unmarshal([]byte(refs), &t.HitRefs); err != nil {
			t.HitRefs = nil
		}
	}
	return t, nil
}

// ChatCounts summarizes chat use for a day, for the cap display.
func (db *DB) ChatTurnsOn(runDate string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM chat_turns WHERE run_date = ?`, runDate).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count chat turns on %s: %w", runDate, err)
	}
	return n, nil
}
