package store

import (
	"bytes"
	"encoding/json"
	"sort"
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
		Queries   []struct {
			Query string `json:"query"`
		} `json:"queries"`
	}
	// A config that will not parse still has an identity: its raw bytes. That is
	// enough to avoid importing the same broken row twice.
	if err := json.Unmarshal(s.Config, &cfg); err != nil {
		return s.Kind + "\x00" + rawIdentity(s.Config)
	}

	switch {
	case cfg.URL != "":
		return s.Kind + "\x00" + strings.ToLower(strings.TrimRight(cfg.URL, "/"))
	case cfg.ChannelID != "":
		return s.Kind + "\x00" + cfg.ChannelID
	case len(cfg.Queries) > 0:
		// The multi-keyword Hacker News shape. Its identity is the keyword set,
		// so re-importing an export is a no-op the way it is for a feed URL --
		// falling through to the raw bytes would not do, because the bytes are
		// compacted by the export encoder and would never match the stored ones.
		keys := make([]string, 0, len(cfg.Queries)+1)
		for _, q := range cfg.Queries {
			keys = append(keys, strings.ToLower(strings.TrimSpace(q.Query)))
		}
		if cfg.Query != "" {
			keys = append(keys, strings.ToLower(strings.TrimSpace(cfg.Query)))
		}
		// Sorted: the same keywords in a different order are the same source, and
		// reordering the list in the editor must not orphan the row on import.
		sort.Strings(keys)
		return s.Kind + "\x00" + strings.Join(keys, "\x1f")
	case cfg.Query != "":
		return s.Kind + "\x00" + strings.ToLower(strings.TrimSpace(cfg.Query))
	default:
		return s.Kind + "\x00" + rawIdentity(s.Config)
	}
}

// rawIdentity is the fallback for a config with no recognized target. Compacted,
// because the export encoder compacts what it writes -- so the same config would
// otherwise have one identity in the database and another in its own export, and
// re-importing would duplicate the row.
func rawIdentity(cfg json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, cfg); err != nil {
		return string(cfg)
	}
	return buf.String()
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
