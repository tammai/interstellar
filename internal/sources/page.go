package sources

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// maxPageBytes caps how much of a page is read while looking for its title.
//
// The metadata lives in <head>, so 256KB is generous. The cap is the point: a
// pasted URL is arbitrary, and one that streams forever would otherwise hold a
// request open until the timeout.
const maxPageBytes = 256 << 10

var (
	reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	// Open Graph first: og:title is what the author chose for sharing, and it is
	// usually cleaner than <title>, which carries the site name and separators.
	reOGTitle = regexp.MustCompile(
		`(?is)<meta[^>]+property\s*=\s*["']og:title["'][^>]+content\s*=\s*["']([^"']*)["']`)
	reOGTitleAlt = regexp.MustCompile(
		`(?is)<meta[^>]+content\s*=\s*["']([^"']*)["'][^>]+property\s*=\s*["']og:title["']`)
	reOGDesc = regexp.MustCompile(
		`(?is)<meta[^>]+property\s*=\s*["']og:description["'][^>]+content\s*=\s*["']([^"']*)["']`)
	reOGDescAlt = regexp.MustCompile(
		`(?is)<meta[^>]+content\s*=\s*["']([^"']*)["'][^>]+property\s*=\s*["']og:description["']`)
	reMetaDesc = regexp.MustCompile(
		`(?is)<meta[^>]+name\s*=\s*["']description["'][^>]+content\s*=\s*["']([^"']*)["']`)
	reMetaDescAlt = regexp.MustCompile(
		`(?is)<meta[^>]+content\s*=\s*["']([^"']*)["'][^>]+name\s*=\s*["']description["']`)
)

// FetchPage reads one arbitrary URL and pulls out enough to classify it.
//
// Regexes rather than a parser, deliberately: the whole job is four fields out of
// <head>, and the alternative is a dependency whose only use is this. Attribute
// order is not fixed in real HTML, so each field is tried both ways round -- the
// og: pattern with content before property is common enough to matter.
//
// A page that yields no title is an error, not an empty item. The classifier judges
// on title and summary, so an item with neither is a row that can never match
// anything and will sit in the database forever.
func FetchPage(ctx context.Context, rawURL string) (Item, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Item{}, fmt.Errorf("bad URL: %w", err)
	}
	// Some publishers serve a challenge page to an unknown agent. Identifying
	// honestly is more likely to work than pretending to be a browser, and it is
	// what a well-behaved reader does.
	req.Header.Set("User-Agent", "Interstellar/0.1 (personal news reader)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := DefaultClient.Do(req)
	if err != nil {
		return Item{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Item{}, fmt.Errorf("the page returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return Item{}, err
	}
	html := string(body)

	title := firstMatch(html, reOGTitle, reOGTitleAlt, reTitle)
	if title == "" {
		return Item{}, fmt.Errorf("no title found; is this an HTML page?")
	}

	item := Item{
		// resp.Request.URL, not rawURL: this is where redirects landed, which is
		// the URL that should be canonicalized and deduplicated against.
		URL:     resp.Request.URL.String(),
		Title:   cleanText(title),
		Summary: truncate(cleanText(firstMatch(html, reOGDesc, reOGDescAlt, reMetaDesc, reMetaDescAlt)), maxSummary),
	}
	return item, nil
}

func firstMatch(s string, patterns ...*regexp.Regexp) string {
	for _, re := range patterns {
		if m := re.FindStringSubmatch(s); len(m) > 1 {
			if v := strings.TrimSpace(m[1]); v != "" {
				return v
			}
		}
	}
	return ""
}

// Title and summary are run through rss.go's cleanText, which already strips tags,
// unescapes the full HTML entity table via the standard library and collapses
// whitespace. A second, page-specific version of that would drift from it.
