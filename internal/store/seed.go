package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// SeedVersion is the seed file format version.
const SeedVersion = 1

// Seed is what a new database starts with: the file format of seed/default.json
// and DATA_DIR/seed.json.
//
// Sources use the export bundle's shape (SourceExport), so a list exported from
// Settings → Sources drops straight into a seed.
type Seed struct {
	Version int            `json:"version"`
	Topics  []SeedTopic    `json:"topics"`
	Sources []SourceExport `json:"sources"`
}

// SeedTopic is one topic in a seed. An empty definition is allowed and means the
// topic shows the setup hint until the owner writes one (§7); the classifier skips
// it until then. Positions are optional, and a topic without one is placed the way
// CreateTopic places it.
type SeedTopic struct {
	Title      string   `json:"title"`
	Definition string   `json:"definition"`
	Priority   int      `json:"priority"`
	Active     *bool    `json:"active,omitempty"`
	PosX       *float64 `json:"pos_x,omitempty"`
	PosY       *float64 `json:"pos_y,omitempty"`
}

// seedKinds mirrors migration 001's CHECK on sources.kind, which cannot widen.
var seedKinds = map[string]bool{"rss": true, "hn_search": true, "youtube_rss": true}

// ParseSeed reads and validates a seed. It refuses rather than repairs: a seed
// is written once and read on a first boot nobody is watching, so anything wrong
// with it has to be said before it is applied, all at once, with a line per
// problem.
func ParseSeed(data []byte) (Seed, error) {
	var s Seed
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Seed{}, fmt.Errorf("seed is not valid JSON in the expected shape: %w", err)
	}

	var problems []string
	if s.Version != SeedVersion {
		problems = append(problems, fmt.Sprintf("version is %d, want %d", s.Version, SeedVersion))
	}

	titles := map[string]bool{}
	for i, t := range s.Topics {
		title := strings.TrimSpace(t.Title)
		switch {
		case title == "":
			problems = append(problems, fmt.Sprintf("topic %d has no title", i+1))
		case titles[strings.ToLower(title)]:
			problems = append(problems, fmt.Sprintf("topic %q appears twice", title))
		}
		titles[strings.ToLower(title)] = true
		if (t.PosX == nil) != (t.PosY == nil) {
			problems = append(problems, fmt.Sprintf("topic %q sets only one of pos_x and pos_y", title))
		}
		for _, p := range []*float64{t.PosX, t.PosY} {
			if p != nil && (*p < 0 || *p > 1) {
				problems = append(problems, fmt.Sprintf("topic %q has a position outside 0..1", title))
				break
			}
		}
	}

	seen := map[string]string{}
	hn := 0
	for i, src := range s.Sources {
		label := fmt.Sprintf("source %d (%q)", i+1, src.Name)
		if strings.TrimSpace(src.Name) == "" {
			problems = append(problems, fmt.Sprintf("source %d has no name", i+1))
		}
		if !seedKinds[src.Kind] {
			problems = append(problems, fmt.Sprintf("%s has kind %q; want rss, hn_search or youtube_rss", label, src.Kind))
		}
		var cfg map[string]any
		if err := json.Unmarshal(src.Config, &cfg); err != nil || cfg == nil {
			problems = append(problems, fmt.Sprintf("%s has a config that is not a JSON object", label))
			continue
		}
		// One row per feed, or it is counted twice in every source_count it touches.
		if prev, dup := seen[src.Identity()]; dup {
			problems = append(problems, fmt.Sprintf("%s fetches the same thing as %q", label, prev))
		}
		seen[src.Identity()] = src.Name
		if src.Kind == "hn_search" {
			hn++
		}
	}
	// Migration 007: Hacker News is one source carrying many keywords. Two rows make
	// one story read as two outlets and gild stars that one site carried.
	if hn > 1 {
		problems = append(problems, fmt.Sprintf(
			"%d hn_search sources; there must be at most one, with every keyword in its queries", hn))
	}

	if len(problems) > 0 {
		return Seed{}, fmt.Errorf("seed has %d problem(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return s, nil
}
