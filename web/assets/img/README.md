# Bundled images

## milky-way.webp

The galactic band rendered behind the star map. Baked into `bgLayer` once per
layout by `Sky.drawBandPhoto`; see the note in `sky.js` for how it is composited.

- **Origin:** generated with an AI image model.
- **Use:** personal, non-commercial only.

Bundled rather than fetched, like the fonts beside it: spec §6 requires the UI
to render offline, so nothing here is loaded from a host.

**This asset constrains distribution.** Spec §7 and the 2026-08-11 decision
already make distribution a local `docker build` with no published image, and
"publish to GHCR" is closed as *won't do*. That was a preference. With this file
embedded in the binary it is also a licence condition: a published image, or any
commercial use of the tool, would need this image replaced first. `drawMilkyWay`
in `sky.js` is the hand-drawn fallback and is what the sky renders without it,
so removing this file degrades the background rather than breaking the build.
