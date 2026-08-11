package sources

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// trackingParams are query parameters that identify a *referral*, not a
// document. Two links differing only by these point at the same article, so
// dropping them is what lets INSERT OR IGNORE collapse the same story arriving
// from a newsletter, a tweet, and an aggregator.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "dclid": true, "msclkid": true, "yclid": true,
	"mc_cid": true, "mc_eid": true, "igshid": true, "twclid": true,
	"_hsenc": true, "_hsmi": true, "vero_id": true, "vero_conv": true,
	"ref": true, "referrer": true, "source": true, "src": true,
	"cmpid": true, "campaign_id": true, "spm": true, "at_medium": true, "at_campaign": true,
}

// Canonical normalizes a URL so the same article from different feeds produces
// the same string.
//
// It is deliberately conservative: it only removes things that cannot change
// which document you land on. Path case, for instance, is left alone, because
// plenty of servers treat paths case-sensitively.
func Canonical(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty url")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", raw, err)
	}

	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q in %q", u.Scheme, raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host in %q", raw)
	}

	u.Host = strings.ToLower(u.Host)
	u.Host = strings.TrimPrefix(u.Host, "www.")
	// Default ports carry no meaning.
	u.Host = strings.TrimSuffix(u.Host, ":80")
	u.Host = strings.TrimSuffix(u.Host, ":443")

	// A fragment is a position within a document, never a different document.
	u.Fragment = ""
	u.RawFragment = ""

	// Drop tracking params, keep the rest, and sort so parameter order can't
	// produce two spellings of one URL.
	if u.RawQuery != "" {
		q := u.Query()
		for key := range q {
			lower := strings.ToLower(key)
			if trackingParams[lower] || strings.HasPrefix(lower, "utm_") {
				q.Del(key)
			}
		}
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var b strings.Builder
		for _, k := range keys {
			values := q[k]
			sort.Strings(values)
			for _, v := range values {
				if b.Len() > 0 {
					b.WriteByte('&')
				}
				b.WriteString(url.QueryEscape(k))
				b.WriteByte('=')
				b.WriteString(url.QueryEscape(v))
			}
		}
		u.RawQuery = b.String()
	}

	// "/article/" and "/article" are the same page everywhere that matters.
	// The root path is the exception: "https://x.test" needs its slash.
	if len(u.Path) > 1 {
		u.Path = strings.TrimSuffix(u.Path, "/")
	}
	if u.Path == "" {
		u.Path = "/"
	}

	return u.String(), nil
}
