package store

import (
	"strings"
	"testing"
)

func TestNormalizeEntityName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Claude Code", "claude code"},
		{"claude-code", "claude code"}, // the seed file carries both as one entity
		{"  Claude   Code  ", "claude code"},
		{"GPT-5", "gpt 5"},
		{"OpenAI Codex", "openai codex"},
		{"", ""},
		{"---", ""},
		// Distinctions that must SURVIVE the fold: merging two entities is
		// unrecoverable, keeping them apart is one alias away from fixed.
		{"v0.dev", "v0 dev"},
		{"v0", "v0"},
		// Runes, not bytes. A byte-wise fold splits these and json.Marshal
		// substitutes U+FFFD rather than failing, so the damage is silent.
		{"Trí tuệ nhân tạo", "trí tuệ nhân tạo"},
		{"MÔ HÌNH", "mô hình"},
	}
	for _, c := range cases {
		if got := NormalizeEntityName(c.in); got != c.want {
			t.Errorf("NormalizeEntityName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestImportWatchlistAddsAndResolves(t *testing.T) {
	db := openTemp(t)

	res, err := db.ImportWatchlist([]EntityImport{
		{Name: "Claude Code", Aliases: []string{"claude-code", "Agent View"},
			Layer: "coding_agents", Kind: "tool", Status: "active",
			FirstPartyURL: "https://code.claude.com"},
		{Name: "Cursor", Aliases: []string{"Cursor IDE", "BugBot"}, Layer: "coding_agents"},
	})
	if err != nil {
		t.Fatalf("ImportWatchlist: %v", err)
	}
	if res.Added != 2 {
		t.Errorf("added = %d, want 2", res.Added)
	}
	// "claude-code" normalizes to the name itself and carries no information, so
	// it is not stored as an alias: 1 + 2 = 3.
	if res.AliasesAdded != 3 {
		t.Errorf("aliases added = %d, want 3", res.AliasesAdded)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("conflicts = %v, want none", res.Conflicts)
	}

	aliases, err := db.EntityAliasMap()
	if err != nil {
		t.Fatalf("EntityAliasMap: %v", err)
	}
	// Every route to one entity resolves to the same id: its name, its own
	// normalized spelling, and each alias.
	for _, spelling := range []string{"Claude Code", "claude-code", "CLAUDE  CODE", "Agent View"} {
		if aliases[NormalizeEntityName(spelling)] != aliases["claude code"] {
			t.Errorf("%q did not resolve to Claude Code", spelling)
		}
	}
	if aliases["bugbot"] == aliases["claude code"] {
		t.Error("BugBot resolved to the wrong entity")
	}

	// Defaults fill in where the file is silent.
	all, err := db.Entities()
	if err != nil {
		t.Fatalf("Entities: %v", err)
	}
	for _, e := range all {
		if e.Kind == "" || e.Status == "" {
			t.Errorf("entity %q has empty kind/status: %+v", e.Name, e)
		}
	}
}

// Re-importing the same file must not duplicate anything: an import is run again
// whenever the seed is edited, and a second run that doubled the alias table
// would be a silent corruption of the resolution map.
func TestImportWatchlistIsIdempotent(t *testing.T) {
	db := openTemp(t)

	rows := []EntityImport{
		{Name: "Cursor", Aliases: []string{"Cursor IDE"}, Layer: "coding_agents"},
	}
	if _, err := db.ImportWatchlist(rows); err != nil {
		t.Fatalf("first import: %v", err)
	}
	second, err := db.ImportWatchlist(rows)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Added != 0 || second.AliasesAdded != 0 {
		t.Errorf("second import added %d entities, %d aliases; want 0, 0",
			second.Added, second.AliasesAdded)
	}
	if second.Updated != 1 {
		t.Errorf("updated = %d, want 1", second.Updated)
	}
	if n := count(t, db, "SELECT count(*) FROM entities"); n != 1 {
		t.Errorf("entities = %d, want 1", n)
	}
	if n := count(t, db, "SELECT count(*) FROM entity_aliases"); n != 1 {
		t.Errorf("aliases = %d, want 1", n)
	}
}

// Mutable by design: a tool changing layer or being deprecated is the thing the
// watchlist exists to record, so a re-import carries those through.
func TestImportWatchlistUpdatesMutableFields(t *testing.T) {
	db := openTemp(t)

	if _, err := db.ImportWatchlist([]EntityImport{
		{Name: "Windsurf", Layer: "coding_agents", Status: "active"},
	}); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := db.ImportWatchlist([]EntityImport{
		{Name: "Windsurf", Layer: "harnesses", Status: "retired"},
	}); err != nil {
		t.Fatalf("second import: %v", err)
	}

	all, _ := db.Entities()
	if len(all) != 1 {
		t.Fatalf("entities = %d, want 1", len(all))
	}
	if all[0].Layer != "harnesses" || all[0].Status != "retired" {
		t.Errorf("got layer=%q status=%q, want harnesses/retired", all[0].Layer, all[0].Status)
	}
}

// One alias claimed by two entities is reported and skipped, never resolved.
// Attaching claims to whichever row was written first splits one history in two,
// and both halves look plausible, so nothing downstream can notice.
func TestImportWatchlistReportsAliasConflict(t *testing.T) {
	db := openTemp(t)

	res, err := db.ImportWatchlist([]EntityImport{
		{Name: "OpenAI Codex", Aliases: []string{"Codex"}, Layer: "coding_agents"},
		{Name: "Codex Cloud", Aliases: []string{"codex"}, Layer: "coding_agents"},
	})
	if err != nil {
		t.Fatalf("ImportWatchlist: %v", err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("conflicts = %v, want exactly one", res.Conflicts)
	}
	if !strings.Contains(res.Conflicts[0], "Codex") {
		t.Errorf("conflict does not name the alias: %q", res.Conflicts[0])
	}
	// Both entities still exist -- the conflict costs an alias, not a row.
	if n := count(t, db, "SELECT count(*) FROM entities"); n != 2 {
		t.Errorf("entities = %d, want 2", n)
	}
	if n := count(t, db, "SELECT count(*) FROM entity_aliases"); n != 1 {
		t.Errorf("aliases = %d, want 1", n)
	}
}

// A name that is already somebody's alias is refused rather than inserted: it
// would create a second row for a thing the map already resolves elsewhere.
func TestImportWatchlistRefusesNameTakenByAlias(t *testing.T) {
	db := openTemp(t)

	if _, err := db.ImportWatchlist([]EntityImport{
		{Name: "Devin", Aliases: []string{"Windsurf"}, Layer: "coding_agents"},
	}); err != nil {
		t.Fatalf("first import: %v", err)
	}
	res, err := db.ImportWatchlist([]EntityImport{
		{Name: "Windsurf", Layer: "coding_agents"},
	})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res.Added != 0 || res.Skipped != 1 {
		t.Errorf("added=%d skipped=%d, want 0 and 1", res.Added, res.Skipped)
	}
	if len(res.Conflicts) != 1 {
		t.Errorf("conflicts = %v, want one", res.Conflicts)
	}
}

func TestParseWatchlistCSV(t *testing.T) {
	const file = `name,aliases,layer,type,status,first_party_url
Claude Code,claude-code;Agent View,coding_agents,tool,active,https://code.claude.com
Cursor,,coding_agents,tool,active,https://cursor.com
,,ignored,,,
`
	rows, err := ParseWatchlistCSV(strings.NewReader(file))
	if err != nil {
		t.Fatalf("ParseWatchlistCSV: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the nameless row is skipped)", len(rows))
	}
	if len(rows[0].Aliases) != 2 {
		t.Errorf("aliases = %v, want 2", rows[0].Aliases)
	}
	if rows[1].Aliases != nil {
		t.Errorf("empty alias cell produced %v, want nil", rows[1].Aliases)
	}
	if rows[0].Kind != "tool" {
		t.Errorf("kind = %q, want tool (from the `type` header)", rows[0].Kind)
	}
}

// Columns are found by name, so a reordered or extended file still imports
// correctly. A positional reader would not fail here -- it would store the layer
// as the status.
func TestParseWatchlistCSVReordersColumns(t *testing.T) {
	const file = `layer,first_party_url,name,notes,aliases
coding_agents,https://cursor.com,Cursor,ignore me,Cursor IDE
`
	rows, err := ParseWatchlistCSV(strings.NewReader(file))
	if err != nil {
		t.Fatalf("ParseWatchlistCSV: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.Name != "Cursor" || got.Layer != "coding_agents" ||
		got.FirstPartyURL != "https://cursor.com" || len(got.Aliases) != 1 {
		t.Errorf("parsed wrongly: %+v", got)
	}
	if got.Status != "" {
		t.Errorf("status = %q, want empty (no column); the default is applied at import", got.Status)
	}
}
