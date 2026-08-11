package sources

import "testing"

func TestCanonical(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already clean", "https://example.test/post", "https://example.test/post"},
		{"uppercase scheme and host", "HTTPS://Example.TEST/post", "https://example.test/post"},
		{"strips www", "https://www.example.test/post", "https://example.test/post"},
		{"strips fragment", "https://example.test/post#section-3", "https://example.test/post"},
		{"strips trailing slash", "https://example.test/post/", "https://example.test/post"},
		{"keeps root slash", "https://example.test", "https://example.test/"},
		{"strips default http port", "http://example.test:80/post", "http://example.test/post"},
		{"strips default https port", "https://example.test:443/post", "https://example.test/post"},
		{"keeps non-default port", "https://example.test:8443/post", "https://example.test:8443/post"},

		{"drops utm params", "https://example.test/post?utm_source=x&utm_medium=y", "https://example.test/post"},
		{"drops fbclid", "https://example.test/post?fbclid=abc", "https://example.test/post"},
		{"drops ref", "https://example.test/post?ref=hn", "https://example.test/post"},
		{"keeps meaningful params", "https://example.test/post?id=42", "https://example.test/post?id=42"},
		{"keeps id, drops tracking", "https://example.test/p?utm_source=x&id=42", "https://example.test/p?id=42"},
		{"sorts params", "https://example.test/p?b=2&a=1", "https://example.test/p?a=1&b=2"},
		{"case-insensitive tracking key", "https://example.test/p?UTM_Source=x", "https://example.test/p"},

		// Path case is preserved: plenty of servers treat paths case-sensitively,
		// so folding it would merge two genuinely different documents.
		{"preserves path case", "https://example.test/Post/Sub", "https://example.test/Post/Sub"},

		{"hn item survives", "https://news.ycombinator.com/item?id=123", "https://news.ycombinator.com/item?id=123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonical(tc.in)
			if err != nil {
				t.Fatalf("Canonical(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Canonical(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// The whole point of canonicalization: these spellings must collapse to one row.
func TestCanonicalCollapsesFeedVariants(t *testing.T) {
	variants := []string{
		"https://example.test/the-post",
		"https://www.example.test/the-post/",
		"http://example.test/the-post?utm_source=newsletter&utm_campaign=weekly",
		"https://example.test/the-post#conclusion",
		"HTTPS://WWW.EXAMPLE.TEST/the-post",
	}

	// http:// and https:// are genuinely different origins, so the http variant
	// is expected to differ only by scheme.
	seen := map[string]bool{}
	for _, v := range variants {
		got, err := Canonical(v)
		if err != nil {
			t.Fatalf("Canonical(%q): %v", v, err)
		}
		seen[got] = true
	}

	if len(seen) != 2 {
		t.Errorf("expected variants to collapse to 2 forms (http + https), got %d: %v", len(seen), keys(seen))
	}
}

func TestCanonicalRejectsUnusable(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"not a url",
		"ftp://example.test/file",
		"mailto:someone@example.test",
		"javascript:alert(1)",
		"/relative/path",
	} {
		if got, err := Canonical(in); err == nil {
			t.Errorf("Canonical(%q) = %q, want error", in, got)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
