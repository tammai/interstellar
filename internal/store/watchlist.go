package store

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// ParseWatchlistCSV reads the seed format: name, aliases, layer, type, status,
// first_party_url, with aliases semicolon-separated.
//
// Columns are located by HEADER NAME rather than by position. The file is edited
// by hand and by spreadsheet, both of which reorder and add columns, and a
// positional reader would not fail on that -- it would quietly import the layer
// column as a status. `type` is accepted as the header because that is what the
// seed file calls it; it is stored as `kind`, matching sources.
func ParseWatchlistCSV(r io.Reader) ([]EntityImport, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // a trailing empty column is not a reason to refuse the file

	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read watchlist header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	if _, ok := col["name"]; !ok {
		return nil, fmt.Errorf("watchlist has no `name` column")
	}
	if _, ok := col["layer"]; !ok {
		return nil, fmt.Errorf("watchlist has no `layer` column")
	}

	at := func(rec []string, key string) string {
		i, ok := col[key]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var out []EntityImport
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read watchlist row: %w", err)
		}
		name := at(rec, "name")
		if name == "" {
			continue
		}
		e := EntityImport{
			Name:          name,
			Layer:         at(rec, "layer"),
			Kind:          at(rec, "type"),
			Status:        at(rec, "status"),
			FirstPartyURL: at(rec, "first_party_url"),
		}
		for _, a := range strings.Split(at(rec, "aliases"), ";") {
			if a = strings.TrimSpace(a); a != "" {
				e.Aliases = append(e.Aliases, a)
			}
		}
		out = append(out, e)
	}
	return out, nil
}
