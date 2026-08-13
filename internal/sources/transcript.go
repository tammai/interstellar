package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// YouTube transcripts.
//
// There is no public API for this that does not need a key, so this reads the
// watch page and follows the caption track it advertises. That is undocumented
// and can break whenever YouTube changes its page, which shapes every decision
// here: every failure is a plain error the caller logs and moves past, nothing
// panics, nothing retries forever, and a channel whose captions vanish costs one
// video's summary rather than a run.
//
// Two steps, because the timedtext endpoint on its own no longer answers reliably
// without the signed parameters the page supplies:
//
//  1. GET the watch page and pull captionTracks out of ytInitialPlayerResponse.
//  2. GET the track's own baseUrl, asking for json3.

// maxWatchPageBytes caps the watch page read. captionTracks appears inside the
// first player response, well within this.
const maxWatchPageBytes = 4 << 20

// maxTranscriptChars caps a stored transcript. A long talk runs to tens of
// thousands of characters; this keeps one pathological video from dominating the
// database, and the summarizer only ever sees a prefix anyway.
const maxTranscriptChars = 120_000

var (
	reCaptionTracks = regexp.MustCompile(`"captionTracks":(\[.*?\])`)
	reVideoID       = regexp.MustCompile(`(?:v=|/shorts/|youtu\.be/|/embed/)([A-Za-z0-9_-]{11})`)
)

// captionTrack is one advertised subtitle track.
type captionTrack struct {
	BaseURL string `json:"baseUrl"`
	Kind    string `json:"kind"` // "asr" for auto-generated, empty for authored
	Name    struct {
		SimpleText string `json:"simpleText"`
	} `json:"name"`
	LanguageCode string `json:"languageCode"`
}

// Transcript is a fetched caption track, flattened to text.
type Transcript struct {
	Text string
	Lang string
	// Generated is true for YouTube's automatic captions. Worth keeping: an ASR
	// transcript carries misheard proper nouns, which is exactly the kind of
	// detail a classifier might match on.
	Generated bool
}

// VideoID extracts a YouTube video id from a watch, shorts, youtu.be or embed URL.
func VideoID(rawURL string) (string, bool) {
	m := reVideoID.FindStringSubmatch(rawURL)
	if len(m) < 2 {
		return "", false
	}
	return m[1], true
}

// FetchTranscript returns a video's captions as plain text.
//
// preferLang is tried first; the first authored track wins over any automatic one,
// and an automatic track is better than nothing.
func FetchTranscript(ctx context.Context, videoID, preferLang string) (Transcript, error) {
	tracks, err := captionTracksFor(ctx, videoID)
	if err != nil {
		return Transcript{}, err
	}
	if len(tracks) == 0 {
		return Transcript{}, fmt.Errorf("no caption tracks")
	}

	track, ok := pickTrack(tracks, preferLang)
	if !ok {
		return Transcript{}, fmt.Errorf("no usable caption track")
	}

	text, err := fetchTrackText(ctx, track.BaseURL)
	if err != nil {
		return Transcript{}, err
	}
	if strings.TrimSpace(text) == "" {
		// A track that exists and is empty. Reported rather than stored, because an
		// empty transcript would look like a completed fetch and never be retried.
		return Transcript{}, fmt.Errorf("caption track was empty")
	}

	return Transcript{
		Text:      truncate(text, maxTranscriptChars),
		Lang:      track.LanguageCode,
		Generated: track.Kind == "asr",
	}, nil
}

func captionTracksFor(ctx context.Context, videoID string) ([]captionTrack, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.youtube.com/watch?v="+videoID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Interstellar/0.1 (personal news reader)")
	// Without this, some regions get a consent interstitial with no player response.
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("watch page returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWatchPageBytes))
	if err != nil {
		return nil, err
	}

	m := reCaptionTracks.FindSubmatch(body)
	if len(m) < 2 {
		// The common, boring case: the video has no captions at all.
		return nil, fmt.Errorf("no captions advertised on the watch page")
	}

	var tracks []captionTrack
	if err := json.Unmarshal(m[1], &tracks); err != nil {
		return nil, fmt.Errorf("caption track list did not parse: %w", err)
	}
	return tracks, nil
}

// pickTrack prefers an authored track in the wanted language, then any authored
// track, then automatic captions in the wanted language, then anything.
//
// Authored over automatic because a human wrote it; the wanted language over
// others because a translated auto-caption compounds two lots of error.
func pickTrack(tracks []captionTrack, preferLang string) (captionTrack, bool) {
	if preferLang == "" {
		preferLang = "en"
	}
	matches := func(t captionTrack) bool {
		return strings.HasPrefix(strings.ToLower(t.LanguageCode), strings.ToLower(preferLang))
	}

	for _, want := range []func(captionTrack) bool{
		func(t captionTrack) bool { return t.Kind != "asr" && matches(t) },
		func(t captionTrack) bool { return t.Kind != "asr" },
		func(t captionTrack) bool { return matches(t) },
		func(captionTrack) bool { return true },
	} {
		for _, t := range tracks {
			if t.BaseURL != "" && want(t) {
				return t, true
			}
		}
	}
	return captionTrack{}, false
}

// json3Track is the shape of the fmt=json3 caption response.
type json3Track struct {
	Events []struct {
		Segs []struct {
			Utf8 string `json:"utf8"`
		} `json:"segs"`
	} `json:"events"`
}

func fetchTrackText(ctx context.Context, baseURL string) (string, error) {
	// json3 rather than the default XML: it needs no entity handling and no tag
	// stripping, so there is one less thing to get subtly wrong.
	//
	// The separator is chosen rather than assumed. YouTube's baseUrl always carries
	// a query string today, so "&fmt=json3" happens to work -- but appending an &
	// to a URL with no ? makes it part of the path, and the request 404s in a way
	// that looks like a missing caption track rather than a malformed URL.
	url := baseURL
	if !strings.Contains(url, "fmt=") {
		sep := "&"
		if !strings.Contains(url, "?") {
			sep = "?"
		}
		url += sep + "fmt=json3"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Interstellar/0.1 (personal news reader)")

	resp, err := DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("caption track returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWatchPageBytes))
	if err != nil {
		return "", err
	}

	var track json3Track
	if err := json.Unmarshal(body, &track); err != nil {
		return "", fmt.Errorf("caption track did not parse as json3: %w", err)
	}

	var b strings.Builder
	for _, e := range track.Events {
		for _, s := range e.Segs {
			b.WriteString(s.Utf8)
		}
	}
	// Captions arrive as timed fragments, so newlines land mid-sentence. Collapsing
	// them gives the summarizer prose instead of a column of half-phrases.
	return strings.Join(strings.Fields(b.String()), " "), nil
}
