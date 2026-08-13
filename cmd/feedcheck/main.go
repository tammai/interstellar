package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/tammai/interstellar/internal/sources"
)

func main() {
	urls := os.Args[1:]
	var wg sync.WaitGroup
	type res struct {
		url   string
		n     int
		first string // first item's title, so the caller can confirm identity
		err   error
	}
	out := make([]res, len(urls))
	sem := make(chan struct{}, 8)
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cfg, _ := json.Marshal(map[string]string{"url": u})
			items, err := sources.RSSFetcher{}.Fetch(context.Background(), cfg, sources.Options{})
			var first string
			if len(items) > 0 {
				first = items[0].Title
			}
			out[i] = res{u, len(items), first, err}
		}(i, u)
	}
	wg.Wait()
	for _, r := range out {
		if r.err != nil {
			fmt.Printf("  FAIL  %-58s %v\n", r.url, shorten(r.err.Error()))
			continue
		}
		// The title is the whole point, not decoration. A YouTube feed for the
		// wrong channel returns 200 and a healthy item count, so a count alone
		// prints an identical line for the right channel and the wrong one --
		// the exact swap the CLAUDE.md convention tells you to use this tool to
		// catch. Confirming against what the feed itself says it is costs one
		// more verb.
		fmt.Printf("  ok    %-58s %3d items  %s\n", r.url, r.n, shorten(r.first))
	}
}

func shorten(s string) string {
	if len(s) > 60 {
		// This prints feed titles, and CLAUDE.md's convention is to confirm a channel
		// by the title the feed itself returns. A title mangled mid-rune undermines
		// exactly that check.
		return strings.ToValidUTF8(s[:60], "") + "..."
	}
	return s
}
