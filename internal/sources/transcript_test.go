package sources

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVideoIDHandlesEveryURLShape(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":            "dQw4w9WgXcQ",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42s":      "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ":                           "dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ":             "dQw4w9WgXcQ",
		"https://www.youtube.com/embed/dQw4w9WgXcQ":              "dQw4w9WgXcQ",
		"https://www.youtube.com/watch?list=PL123&v=dQw4w9WgXcQ": "dQw4w9WgXcQ",
	}
	for in, want := range cases {
		got, ok := VideoID(in)
		if !ok || got != want {
			t.Errorf("VideoID(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}

	for _, in := range []string{
		"https://example.com/article",
		"https://www.youtube.com/@channel",
		"https://www.youtube.com/watch?v=tooshort",
		"",
	} {
		if got, ok := VideoID(in); ok {
			t.Errorf("VideoID(%q) unexpectedly matched %q", in, got)
		}
	}
}

// watchPage serves a stub watch page advertising the given tracks, plus the track
// endpoint itself.
func transcriptServer(t *testing.T, tracksJSON string, json3 string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)

	mux.HandleFunc("/watch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// The real page embeds this inside a much larger script blob.
		w.Write([]byte(`<html><body><script>var ytInitialPlayerResponse = {"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":` +
			tracksJSON + `}}};</script></body></html>`))
	})
	mux.HandleFunc("/track", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fmt") != "json3" {
			t.Errorf("track fetched without fmt=json3: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(json3))
	})
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// The parsing and track-selection logic, exercised without reaching YouTube.
func TestPickTrackPrefersAuthoredThenLanguage(t *testing.T) {
	tracks := []captionTrack{
		{BaseURL: "asr-en", Kind: "asr", LanguageCode: "en"},
		{BaseURL: "authored-de", LanguageCode: "de"},
		{BaseURL: "authored-en", LanguageCode: "en"},
	}
	got, ok := pickTrack(tracks, "en")
	if !ok || got.BaseURL != "authored-en" {
		t.Errorf("picked %q; want the authored English track", got.BaseURL)
	}

	// No authored English: an authored track in another language still beats ASR,
	// because a human wrote it and a translated auto-caption compounds two errors.
	got, _ = pickTrack([]captionTrack{
		{BaseURL: "asr-en", Kind: "asr", LanguageCode: "en"},
		{BaseURL: "authored-de", LanguageCode: "de"},
	}, "en")
	if got.BaseURL != "authored-de" {
		t.Errorf("picked %q; want the authored track", got.BaseURL)
	}

	// ASR is better than nothing.
	got, ok = pickTrack([]captionTrack{{BaseURL: "asr-fr", Kind: "asr", LanguageCode: "fr"}}, "en")
	if !ok || got.BaseURL != "asr-fr" {
		t.Errorf("picked %q, ok=%v; ASR should be used when it is all there is", got.BaseURL, ok)
	}

	// A track with no baseUrl is unusable.
	if _, ok := pickTrack([]captionTrack{{LanguageCode: "en"}}, "en"); ok {
		t.Error("a track with no baseUrl was picked")
	}
	if _, ok := pickTrack(nil, "en"); ok {
		t.Error("picked a track from an empty list")
	}
}

func TestFetchTrackTextFlattensJSON3(t *testing.T) {
	srv := transcriptServer(t, `[]`, `{"events":[
		{"segs":[{"utf8":"Hello"},{"utf8":" there"}]},
		{"segs":[{"utf8":"\n"}]},
		{"segs":[{"utf8":"second   line"}]}
	]}`)

	got, err := fetchTrackText(context.Background(), srv.URL+"/track?a=1")
	if err != nil {
		t.Fatalf("fetchTrackText: %v", err)
	}
	// Captions arrive as timed fragments, so newlines land mid-sentence; collapsing
	// them is what gives the summarizer prose instead of a column of half-phrases.
	if got != "Hello there second line" {
		t.Errorf("got %q, want whitespace collapsed to prose", got)
	}
}

func TestFetchTrackTextReportsBadResponses(t *testing.T) {
	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<xml>not json3</xml>"))
	}))
	defer notJSON.Close()
	if _, err := fetchTrackText(context.Background(), notJSON.URL+"?fmt=json3"); err == nil {
		t.Error("non-json3 output should be an error")
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer dead.Close()
	if _, err := fetchTrackText(context.Background(), dead.URL+"?fmt=json3"); err == nil {
		t.Error("a 403 should be an error")
	}
}

// The most common real answer: the video simply has no captions. captionTracksFor
// builds its own youtube.com URL, so the marker detection is exercised directly --
// the property that matters is that a page without it is a clean error rather than
// a panic or an empty success.
func TestCaptionTracksMarkerDetection(t *testing.T) {
	if m := reCaptionTracks.FindStringSubmatch(`{"other":"fields"}`); m != nil {
		t.Error("the captionTracks regex matched a page that has none")
	}
	m := reCaptionTracks.FindStringSubmatch(
		`"captionTracks":[{"baseUrl":"https://x/t","languageCode":"en"}],"audioTracks":[]`)
	if m == nil {
		t.Fatal("the captionTracks regex missed a page that has them")
	}
	// Non-greedy: it must stop at its own closing bracket, not run on into the
	// next array in the blob.
	if m[1] != `[{"baseUrl":"https://x/t","languageCode":"en"}]` {
		t.Errorf("captured %q; the match ran past the track list", m[1])
	}
}
