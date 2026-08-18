# Provider marks

The six logos the Model screen shows for the seeded providers, one file per
registry preset. `settings.js` maps a registry name to a file in `PROVIDER_MARKS`;
a provider with no file here falls back to a monogram of its name.

## Why they are files, and why they are here

They are **bundled, never hotlinked** — §6 requires the UI to render offline, and
a logo CDN is exactly the dependency that rule exists to refuse. They are files
rather than inlined markup because they are full-colour artwork with their own
gradients: there is nothing for `currentColor` to do, and inlining them would put
~14KB of path data into a script the browser has to parse on every visit.

Each was re-framed to a square `viewBox` around its own bounding box, with 6%
padding, so six marks from six sources sit on a common optical axis in one 38px
tile. Nothing else about the artwork was altered, except where a source used
`fill="currentColor"` — meaningless in an `<img>`, which has no inherited colour —
and the vendor's own colour was substituted:

| file | source | change |
|---|---|---|
| `deepseek.svg` | the wordmark inlined on deepseek.com | whale glyph only, split from the lettering; `currentColor` → `#4D6BFE` |
| `anthropic.svg` | `claude.ai/favicon.svg` | re-framed only; ships its own `#D97757` |
| `google.svg` | the Gemini mark on `gstatic.com` | re-framed only; keeps its gradient |
| `openrouter.svg` | the glyph openrouter.ai inlines in its own header | re-framed; `currentColor` → `#FCFCFE`, the colour their header renders it in on a dark ground |
| `ollama.svg` | `docs.ollama.com/favicon.svg` | re-framed only; ships `fill="white"` |
| `litellm.svg` | Twemoji's `1f684` (🚄), re-framed | LiteLLM's logo *is* the bullet-train emoji beside a black wordmark — see the header on litellm.ai. Neither half was usable directly: they publish no SVG of it (not on their site, not in their repo), their 256px favicon is a muddy grey sliver at 22px, and the wordmark is black type that vanishes on this background. Twemoji ships the same glyph as clean vector art |

## Trademarks

These are the vendors' trademarks, not ours. They are used here only to identify
each vendor's own API endpoint in a settings list — nominative use, the same thing
a "sign in with" button does. They do not imply any endorsement of or affiliation
with this project.

That is a narrower position than the rest of `web/assets/img/` sits on: unlike
`milky-way.webp`, which carries a licence condition of its own, these carry the
ordinary limits of trademark use. Do not repurpose them as decoration, in
marketing, or anywhere they could read as a badge of association. If a vendor asks
for a file to be removed, delete it — `providerMark` degrades to the monogram
fallback on its own, so nothing breaks.

### Twemoji

`litellm.svg` is [Twemoji](https://github.com/twitter/twemoji), © Twitter, Inc and
other contributors, licensed **CC-BY 4.0**. That licence needs attribution, which
is what this paragraph is — keep it with the file. It is the one mark here that is
not the vendor's own artwork, because LiteLLM's mark is an emoji and this is that
emoji drawn properly.
