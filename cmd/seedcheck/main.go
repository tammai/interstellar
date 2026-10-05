// Command seedcheck validates a seed file and probes every source in it with the
// app's own fetchers -- the check CLAUDE.md requires before any URL is seeded.
//
//	go run ./cmd/seedcheck [-offline] [path]    # default: seed/default.json
//
// It exits non-zero when the file does not parse or an active source fails, so it
// can gate a commit. It makes network requests to the feeds themselves and calls
// no model.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tammai/interstellar/internal/sources"
	"github.com/tammai/interstellar/internal/store"
)

func main() {
	offline := flag.Bool("offline", false, "validate the file only; fetch nothing")
	flag.Parse()
	path := "seed/default.json"
	if flag.NArg() > 0 {
		path = flag.Arg(0)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s, err := store.ParseSeed(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		os.Exit(1)
	}

	fmt.Printf("%s: %d topics, %d sources\n\nTopics\n", path, len(s.Topics), len(s.Sources))
	for _, t := range s.Topics {
		state := "ok   "
		note := fmt.Sprintf("%d-char definition", len(t.Definition))
		if strings.TrimSpace(t.Definition) == "" {
			// Not an error: §7's setup hint is built for exactly this. But the
			// classifier skips the topic until it has one, which is worth saying.
			state, note = "note ", "no definition -- skipped by the classifier until one is written"
		}
		if t.Active != nil && !*t.Active {
			note += ", paused"
		}
		fmt.Printf("  %s %-40s %s\n", state, t.Title, note)
	}

	if *offline {
		fmt.Println("\nSources not probed (-offline).")
		return
	}

	opts := sources.Options{HNMinPoints: 25, YouTubeAPIKey: os.Getenv("YOUTUBE_API_KEY")}
	type result struct {
		n     int
		first string
		err   error
		skip  string
	}
	results := make([]result, len(s.Sources))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, src := range s.Sources {
		if src.Kind == "youtube_rss" && opts.YouTubeAPIKey == "" {
			results[i].skip = "YOUTUBE_API_KEY not set"
			continue
		}
		wg.Add(1)
		go func(i int, src store.SourceExport) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f, err := sources.For(src.Kind)
			if err != nil {
				results[i].err = err
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			items, err := f.Fetch(ctx, src.Config, opts)
			results[i].n, results[i].err = len(items), err
			if len(items) > 0 {
				results[i].first = items[0].Title
			}
		}(i, src)
	}
	wg.Wait()

	fmt.Println("\nSources")
	failed := 0
	for i, src := range s.Sources {
		r := results[i]
		label := fmt.Sprintf("%-12s %-40s", src.Kind, sources.Truncate(src.Name, 40))
		switch {
		case r.skip != "":
			fmt.Printf("  skip  %s %s\n", label, r.skip)
		case r.err != nil:
			// A paused source is allowed to fail -- the default seed carries an
			// inactive placeholder on purpose -- so only an active one fails the run.
			if src.Active {
				failed++
				fmt.Printf("  FAIL  %s %s\n", label, sources.Truncate(r.err.Error(), 80))
			} else {
				fmt.Printf("  off   %s %s (paused, not counted)\n", label, sources.Truncate(r.err.Error(), 60))
			}
		case r.n == 0 && src.Active:
			// 200 with nothing in it: the trap feedcheck exists for.
			failed++
			fmt.Printf("  FAIL  %s 0 items -- fetched fine, parsed to nothing\n", label)
		default:
			// The first title is the identity check: a wrong channel id returns a
			// healthy count for someone else's channel.
			fmt.Printf("  ok    %s %3d items  %s\n", label, r.n, sources.Truncate(r.first, 60))
		}
	}

	if failed > 0 {
		fmt.Printf("\n%d active source(s) failed.\n", failed)
		os.Exit(1)
	}
	fmt.Println("\nAll active sources fetched.")
}
