package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SourceBundleVersion is the export format version.
//
// Bumped only on a breaking change. An importer that meets a version it does not
// know refuses rather than guessing, because a half-understood source list is
// worse than no import: the owner would not know which feeds were dropped.
const SourceBundleVersion = 1

// SourceBundle is an exported source list.
//
// Ids and last_run_at are deliberately absent. Both are instance-local: an id
// means nothing in another database, and importing one instance's run history
// would make a feed that has never been fetched here look healthy.
type SourceBundle struct {
	Version int            `json:"version"`
	Sources []SourceExport `json:"sources"`
}

// SourceExport is one source, stripped to what transfers.
type SourceExport struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
	Active bool            `json:"active"`
}

// Identity is what makes two sources the same source across instances.
//
// The config's target, not the name: the same feed renamed on one instance is
// still that feed, and importing it again would give the owner two rows that
// fetch identical items and inflate every source_count that touches them.
func (s SourceExport) Identity() string {
	var cfg struct {
		URL       string `json:"url"`
		ChannelID string `json:"channel_id"`
		Query     string `json:"query"`
	}
	// A config that will not parse still has an identity: its raw bytes. That is
	// enough to avoid importing the same broken row twice.
	if err := json.Unmarshal(s.Config, &cfg); err != nil {
		return s.Kind + "\x00" + string(s.Config)
	}

	switch {
	case cfg.URL != "":
		return s.Kind + "\x00" + strings.ToLower(strings.TrimRight(cfg.URL, "/"))
	case cfg.ChannelID != "":
		return s.Kind + "\x00" + cfg.ChannelID
	case cfg.Query != "":
		return s.Kind + "\x00" + strings.ToLower(strings.TrimSpace(cfg.Query))
	default:
		return s.Kind + "\x00" + string(s.Config)
	}
}

// ExportSources returns every source in transfer form, ordered stably so two
// exports of an unchanged list are byte-identical and diffable.
func (db *DB) ExportSources() (SourceBundle, error) {
	sources, err := db.AllSources()
	if err != nil {
		return SourceBundle{}, err
	}

	bundle := SourceBundle{Version: SourceBundleVersion, Sources: make([]SourceExport, 0, len(sources))}
	for _, s := range sources {
		bundle.Sources = append(bundle.Sources, SourceExport{
			Kind:   s.Kind,
			Name:   s.Name,
			Config: s.Config,
			Active: s.Active,
		})
	}
	return bundle, nil
}

// SourceIdentities maps every existing source's identity to its id.
func (db *DB) SourceIdentities() (map[string]int64, error) {
	sources, err := db.AllSources()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(sources))
	for _, s := range sources {
		key := SourceExport{Kind: s.Kind, Name: s.Name, Config: s.Config}.Identity()
		out[key] = s.ID
	}
	return out, nil
}

// ImportOutcome is what happened to one source in an import.
type ImportOutcome struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"` // added | skipped | rejected | added_inactive
	Reason string `json:"reason,omitempty"`
	// Items is what the verifying fetch actually returned. Zero on a feed that
	// answered but carried nothing, which is the failure the convention in
	// CLAUDE.md exists for -- a 200 is not evidence.
	Items int `json:"items"`
}

// ImportSources adds sources that are not already present.
//
// Additive only. Deleting whatever a file does not mention would make an import
// from a stale export silently destroy the pruning the owner has done since --
// and pruning is most of what an owner does to this list. Removing a source stays
// a deliberate, one-at-a-time act.
func (db *DB) ImportSources(candidates []SourceExport) ([]int64, error) {
	existing, err := db.SourceIdentities()
	if err != nil {
		return nil, err
	}

	var added []int64
	for _, c := range candidates {
		if _, ok := existing[c.Identity()]; ok {
			continue
		}
		id, err := db.CreateSource(Source{
			Kind:   c.Kind,
			Name:   c.Name,
			Config: c.Config,
			Active: c.Active,
		})
		if err != nil {
			return added, fmt.Errorf("import source %q: %w", c.Name, err)
		}
		// Within one file, two rows may share an identity. Register as we go so the
		// second is skipped rather than inserted.
		existing[c.Identity()] = id
		added = append(added, id)
	}
	return added, nil
}
