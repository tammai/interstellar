// The star map.
//
// Rendering is carried over from the approved prototype and §6 treats it as a
// contract: colour-temperature variation, tapered diffraction spikes with the
// vertical pair longer, a rotating glint on hot stars, a hard bright core,
// two-layer parallax dust, and the celestial graticule.
//
// What is new here is the three problems §6 lists as unsolved in the mock:
// collision spacing, one signal belonging to several topics, and canvas
// performance past 100 stars.

// Guarded so the module can be imported outside a browser (see sky.test.js),
// and live rather than read once: someone who turns the setting on mid-session
// should get a still sky without reloading.
const motionQuery = typeof matchMedia === 'function'
  ? matchMedia('(prefers-reduced-motion: reduce)')
  : null;
let REDUCED_MOTION = motionQuery ? motionQuery.matches : false;
motionQuery?.addEventListener?.('change', (e) => { REDUCED_MOTION = e.matches });

// Which layers of the sky get drawn.
//
// These are view preferences, not configuration. Nothing here reaches a run, is
// read by the pipeline, or changes what was ingested, matched or composed — so
// they live in the browser rather than the `settings` table, and they apply on
// the next frame rather than the next run. Putting them in `settings` would mean
// a migration and new API surface to express "this reader prefers less scenery".
//
// Every layer defaults on: the sky §6 approved is the one you get before touching
// anything, and a stale key must never be able to hide a layer you did not turn off.
const SHOW_KEY = 'interstellar.sky.show';
const DEFAULT_SHOW = {
  dust: true,        // the two parallax dust layers
  milkyway: true,    // the galactic band, baked into bgLayer
  grid: true,        // the celestial graticule
  // Constellation joins and shared-star ties (never the labels). The one layer
  // that defaults off: at today's density the sky reads better without it, and a
  // reader who wants the topic structure drawn can ask for it. §6 specifies the
  // links, so this is a default, not a removal — see spec decision 18.
  lines: false,
  meteors: true,     // the occasional streak
  motion: true,      // twinkle, pulse, glint rotation, view easing
};

export const SKY_LAYERS = Object.keys(DEFAULT_SHOW);

function storage() {
  // Guarded so the module still imports outside a browser (see sky.test.js), and
  // so a locked-down profile that throws on localStorage access cannot take the
  // whole star map down with it.
  try {
    return typeof localStorage === 'object' && localStorage ? localStorage : null;
  } catch {
    return null;
  }
}

export function loadShow(store) {
  const ls = store === undefined ? storage() : store;
  const show = { ...DEFAULT_SHOW };
  if (!ls) return show;
  try {
    const raw = JSON.parse(ls.getItem(SHOW_KEY) || '{}');
    // Whitelisted key by key rather than merged wholesale. A hand-edited or
    // stale value must not introduce a layer name nothing draws, and a
    // non-boolean must not become a layer state by being truthy.
    for (const k of SKY_LAYERS) {
      if (typeof raw?.[k] === 'boolean') show[k] = raw[k];
    }
  } catch {
    // Unparseable is the same as unset: draw everything.
  }
  return show;
}

export function saveShow(show, store) {
  const ls = store === undefined ? storage() : store;
  if (!ls) return;
  try {
    ls.setItem(SHOW_KEY, JSON.stringify(show));
  } catch {
    // A full or blocked quota costs the preference, not the frame.
  }
}

// Colour temperature palette. Real skies are not uniformly white, and the
// variation is what stops a field of stars reading as a scatter plot.
const TEMPS = [
  { core: '255,255,255', mid: '214,230,255', halo: '150,185,255' }, // blue-white
  { core: '255,255,255', mid: '232,240,255', halo: '175,200,255' }, // white
  { core: '255,252,244', mid: '255,243,222', halo: '255,215,170' }, // warm white
  { core: '252,248,255', mid: '238,230,255', halo: '195,175,255' }, // violet-white
];
const HOT = { core: '255,240,210', mid: '255,214,140', halo: '255,190,90' };

const MIN_SCALE = 0.55;
const MAX_SCALE = 3.5;

// The galactic band, as a photograph.
//
// Bundled next to the fonts and for the same reason: §6 requires the UI to render
// offline, so nothing here is fetched from a host. It is baked into bgLayer once
// per layout, so it costs a decode at startup and nothing per frame — the same
// budget the procedural band had.
//
// drawMilkyWay is kept as the fallback rather than deleted. If the file is
// missing or fails to decode, a sky with a hand-drawn band is a far better
// outcome than a flat gradient, and it keeps the offline guarantee honest
// instead of resting on one asset.
const BAND_IMAGE_SRC = 'img/milky-way.webp';

// How much of the photograph's light reaches the sky. It is composited
// additively, so this scales the added light directly: halving it halves the
// band's brightness, and 0 is the same as switching the layer off.
//
// Held at half its original 0.85 deliberately. The band is scenery — §6 asks the
// stars to be what the eye lands on, and a photograph bright enough to admire is
// bright enough to compete with the signals it exists behind.
const BAND_STRENGTH = 0.42;

// Deterministic PRNG: the same sky must look the same on every reload, or the
// spatial memory §6 protects is worthless.
function mulberry32(a) {
  return function () {
    a |= 0; a = (a + 0x6D2B79F5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export class Sky {
  constructor(canvas, { onSelect, onHover }) {
    this.canvas = canvas;
    // alpha: false — the background layer covers every pixel, so there is nothing
    // to see through the canvas. Keeping an alpha channel it never uses costs a
    // full-screen composite against the page on every frame.
    this.ctx = canvas.getContext('2d', { alpha: false });
    this.onSelect = onSelect;
    this.onHover = onHover;

    this.topics = [];
    this.groups = [];       // one entry per rendered star
    this.stars = [];
    this.dustFar = [];
    this.dustNear = [];
    this.hoverStar = null;
    this.hotThreshold = 3;

    // Read before the first layout(): buildLayers bakes the Milky Way into
    // bgLayer, so this has to be known before that bake, not after it.
    this.show = loadShow();

    // Interaction gate. The pan/hover listeners live on `window`, because a drag
    // that leaves the canvas should keep panning — but that also means the list
    // overlay does not shield them, and a mouse moved over a list row was firing
    // star tooltips behind it. List view turns this off instead.
    this.interactive = true;

    // Active legend filters, empty meaning "show everything". Filtering hides
    // stars rather than re-laying them out: §6 rests on the reader building
    // spatial memory of where a story sits, and moving the survivors to fill the
    // gaps would throw that away every time a filter is touched.
    this.filters = new Set();
    this.shown = [];
    // Per-topic view of `shown`, rebuilt only when `shown` changes. See
    // buildConstellations: the star positions and topic memberships behind it are
    // fixed between rebuilds, so recomputing them per frame was pure waste.
    this.constellations = [];
    this.bigCut = Infinity;
    this.drawn = 0;                          // stars blitted last frame, see onScreen

    // Frame-rate meter for the Layers panel. Counted inside frame() rather than
    // by a second rAF loop, which would measure its own scheduling alongside the
    // sky's and cost a callback per frame to do it.
    this.fps = 0;
    this.fpsFrames = 0;
    this.fpsSince = 0;

    // Idle skipping. A sky with motion off, a settled view and no pointer
    // activity is identical frame to frame, and repainting it meant overwriting
    // the whole backing store — 4.4 million device pixels on a 2x display —
    // sixty times a second to produce the same image. `dirty` is set by anything
    // that changes what a frame would look like; `idle` is what the last frame
    // actually did, for the readout.
    this.dirty = true;
    this.idle = false;
    this.wasStill = null;   // reconciled on the first frame, see frame()

    // Latest pointer position, consumed once per frame by updateHover rather
    // than hit-tested per mousemove event.
    this.pointer = null;
    this.pointerDirty = false;

    this.view = { x: 0, y: 0, s: 1 };        // screen = world * s + offset
    this.viewTarget = { x: 0, y: 0, s: 1 };
    this.sprites = new Map();                // pre-baked star bitmaps, see spriteFor
    this.bgLayer = null;                     // background + nebula, screen space
    this.gridLayer = null;                   // celestial graticule, world space
    this.bandImage = null;                   // the photographic band, see loadBand

    // The occasional meteor. Ambient, so unlike the sky layout it is deliberately
    // not seeded — seeing the identical streak at the identical moment on every
    // reload would read as a loop rather than as weather.
    this.meteor = null;
    this.nextMeteor = 0;

    // Whether the render loop is running at all. List view covers the canvas with
    // an opaque overlay, and drawing a sky nobody can see at 60fps competes with
    // that list's scrolling for the same main thread.
    this.running = false;
    this.visible = true;
    this.raf = null;                         // pending rAF handle, see start/stop
    this.resizeRaf = null;                   // pending relayout, see onResize
    this.lastNow = null;                     // previous frame timestamp, see updateMeteor

    this.bindEvents();

    // Coalesced to one relayout per frame. layout() is not cheap -- it re-places
    // every star, runs relax(), and re-bakes the background layers, which is
    // hundreds of arcs and gradients at device resolution. Dragging a window
    // edge fires resize dozens of times a second, and doing all of that per
    // event pinned the main thread for the length of the drag.
    addEventListener('resize', () => this.onResize());

    // Size the canvas before the first frame. The render loop starts now but
    // data arrives over the network, so without this the first frame can run
    // with no dimensions -- and a canvas call with a NaN argument throws inside
    // the rAF callback, which skips the re-schedule at the end of frame() and
    // kills the loop permanently. It is a race, so it fails intermittently.
    this.layout();
    this.loadBand();
    this.start();

    // A hidden tab should not render either. Browsers throttle rAF heavily when
    // backgrounded, but stopping outright also means no queued work to catch up on
    // when the tab comes back.
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) this.stop();
      else if (this.visible) this.start();
    });
  }

  // start/stop own the rAF loop. Guarded against double-scheduling, because two
  // concurrent loops would render everything twice per frame and look like exactly
  // the kind of lag this exists to avoid.
  //
  // The `running` flag alone is not that guard. A hidden tab does not run its
  // queued rAF callbacks, it holds them -- so stop() left one pending, start()
  // saw running === false and queued a second, and on the next paint both ran
  // with running === true and each re-scheduled itself. Every tab-away/tab-back
  // doubled the loop count, and a fast List/Sky toggle did the same inside one
  // frame. Cancelling the pending handle is what actually makes stop() stop.
  start() {
    if (this.running) return;
    this.running = true;
    // Don't fire a meteor the instant the sky comes back into view.
    this.nextMeteor = 0;
    // A resumed loop has no previous frame to measure against; updateMeteor
    // treats a null lastNow as "start the clock here" rather than integrating
    // across however long the tab was hidden.
    this.lastNow = null;
    // A resumed loop measures from here; averaging across the gap would report a
    // frame rate for time the sky spent not drawing at all.
    this.fpsFrames = 0;
    this.fpsSince = 0;
    // A resumed loop has nothing on the canvas it can trust: the tab may have
    // been backgrounded for hours, or List view may have covered it.
    this.dirty = true;
    this.raf = requestAnimationFrame((t) => this.frame(t));
  }

  stop() {
    this.running = false;
    this.fps = 0;
    this.idle = false;
    if (this.raf !== null) {
      cancelAnimationFrame(this.raf);
      this.raf = null;
    }
  }

  // One relayout per frame no matter how many resize events arrive. Scheduled on
  // rAF rather than a timeout so the rebuild lands on the same frame that paints
  // it, and a resize while the loop is stopped still takes effect.
  onResize() {
    if (this.resizeRaf !== null) return;
    this.resizeRaf = requestAnimationFrame(() => {
      this.resizeRaf = null;
      this.layout();
    });
  }

  // Called when the list overlay covers the canvas, or uncovers it.
  setVisible(on) {
    this.visible = on;
    if (on) this.start();
    else this.stop();
  }

  // The motion gate, replacing what used to be a bare REDUCED_MOTION read.
  //
  // The OS preference wins by construction: this ORs the two, so the settings
  // toggle can turn motion off but never on. §10 requires prefers-reduced-motion
  // to disable twinkle, pulse, glint and parallax easing, and that guarantee must
  // not be defeatable from a settings sheet.
  get still() { return REDUCED_MOTION || !this.show.motion }

  // Flip one layer on or off.
  //
  // Everything except the Milky Way is a per-frame guard and needs nothing but
  // the next frame. The band is baked into bgLayer, so turning it off means
  // re-baking that layer rather than skipping a draw call.
  setShow(name, on) {
    if (!SKY_LAYERS.includes(name)) return false;
    this.show[name] = !!on;
    saveShow(this.show);
    if (name === 'milkyway') this.buildLayers();
    this.dirty = true;
    return true;
  }

  // Ease the view toward its target, and report whether it actually moved.
  //
  // The snap is what makes idle skipping possible at all: the ease is asymptotic,
  // so `view` never becomes exactly `viewTarget`, and a sky that has visibly
  // stopped would otherwise keep repainting forever over a difference measured in
  // thousandths of a pixel. The threshold is half a device pixel of translation,
  // and a scale step too small to move any star by that much.
  easeView() {
    const t = this.viewTarget, v = this.view;
    const dx = t.x - v.x, dy = t.y - v.y, ds = t.s - v.s;

    if (Math.abs(dx) < 0.25 && Math.abs(dy) < 0.25 && Math.abs(ds) < 0.0005) {
      if (dx === 0 && dy === 0 && ds === 0) return false;
      v.x = t.x; v.y = t.y; v.s = t.s;
      return true;
    }

    // Reduced motion snaps rather than eases (§10).
    const e = this.still ? 1 : 0.3;
    v.x += dx * e; v.y += dy * e; v.s += ds * e;
    return true;
  }

  // setData takes the /api/sky payload and rebuilds the scene.
  setData({ topics, signals, hot_threshold }) {
    this.topics = topics || [];
    this.hotThreshold = hot_threshold || 3;
    this.groups = groupSignals(signals || []);
    this.layout();
  }

  isHot(g) { return g.sourceCount >= this.hotThreshold }

  // Size is relevance, opacity is age (§6).
  starRadius(g) { return 4.5 + (g.score - 4) * 6 + Math.min(g.sourceCount, 6) * 0.55 }
  starAlpha(g) { return Math.max(0.35, 1 - g.ageDays * 0.105) }

  layout() {
    const dpr = Math.min(devicePixelRatio || 1, 2);
    this.dpr = dpr;
    this.W = innerWidth;
    this.H = innerHeight;
    this.canvas.width = this.W * dpr;
    this.canvas.height = this.H * dpr;
    this.canvas.style.width = this.W + 'px';
    this.canvas.style.height = this.H + 'px';

    const rnd = mulberry32(20260811);
    const margin = { x: 70, top: 90, bottom: 110 };
    const uw = this.W - margin.x * 2;
    const uh = this.H - margin.top - margin.bottom;

    const centreOf = (topicId) => {
      const t = this.topics.find((tp) => tp.id === topicId);
      if (!t) return null;
      return { x: margin.x + t.pos_x * uw, y: margin.top + t.pos_y * uh };
    };
    this.centres = new Map(this.topics.map((t) => [t.id, centreOf(t.id)]));

    // Place each star around its constellation centre. A signal belonging to
    // more than one topic sits at the centroid of those centres — §6's answer to
    // "place the star between constellations".
    const perTopic = new Map();
    for (const g of this.groups) {
      for (const tid of g.topicIds) {
        if (!perTopic.has(tid)) perTopic.set(tid, []);
        perTopic.get(tid).push(g);
      }
    }

    this.stars = this.groups.map((g) => {
      const centres = g.topicIds.map((tid) => this.centres.get(tid)).filter(Boolean);
      if (!centres.length) return null;

      // The star's home is its PRIMARY topic — the first one, which the API
      // orders by score. A shared star drifts toward the others but stays
      // anchored here.
      const primary = centres[0];
      let base = primary;

      if (centres.length > 1) {
        const centroid = {
          x: centres.reduce((s, c) => s + c.x, 0) / centres.length,
          y: centres.reduce((s, c) => s + c.y, 0) / centres.length,
        };
        // Half-way to the centroid gives §6's "between constellations" for the
        // two-topic case. The clamp matters for the degenerate one: a story
        // touching six topics would otherwise land in the dead centre of the
        // canvas, visually attached to nothing.
        let dx = (centroid.x - primary.x) * 0.5;
        let dy = (centroid.y - primary.y) * 0.5;
        const drift = Math.hypot(dx, dy);
        const maxDrift = 170;
        if (drift > maxDrift) {
          dx = (dx / drift) * maxDrift;
          dy = (dy / drift) * maxDrift;
        }
        base = { x: primary.x + dx, y: primary.y + dy };
      }

      let x = base.x, y = base.y;
      if (centres.length === 1) {
        // Fan the topic's own stars around its centre, as the mock does.
        const siblings = perTopic.get(g.topicIds[0]) || [];
        const k = siblings.indexOf(g);
        const n = siblings.length;
        const angle = (k / Math.max(n, 1)) * Math.PI * 2 + rnd() * 1.4;
        const radius = n === 1 ? 0 : 34 + rnd() * 46 + (k % 2) * 18;
        x = base.x + Math.cos(angle) * radius;
        y = base.y + Math.sin(angle) * radius * 0.72;
      }

      return {
        g,
        home: base,
        x, y,
        r: this.starRadius(g),
        a: this.starAlpha(g),
        phase: rnd() * Math.PI * 2,
        ph2: rnd() * Math.PI * 2,
        temp: TEMPS[Math.floor(rnd() * TEMPS.length)],
        spin: rnd() * Math.PI,
        shared: centres.length > 1,
        // Resolved sprite per layer. Keyed here rather than looked up through
        // spriteFor's string key on every blit -- see blit().
        sp: Object.create(null),
      };
    }).filter(Boolean);

    relax(this.stars);
    this.computeBigCut();
    this.applyFilters();

    const d1 = mulberry32(42), d2 = mulberry32(7);
    const dust = (rand, count, rMin, rSpread, aMin, aSpread, spMin, spSpread) =>
      Array.from({ length: count }, () => ({
        x: rand() * this.W * 1.6 - this.W * 0.3,
        y: rand() * this.H * 1.6 - this.H * 0.3,
        r: rMin + rand() * rSpread,
        a: aMin + rand() * aSpread,
        ph: rand() * 7,
        sp: spMin + rand() * spSpread,
      }));
    this.dustFar = dust(d1, 130, 0.35, 0.8, 0.05, 0.18, 0.3, 1);
    this.dustNear = dust(d2, 70, 0.6, 1.4, 0.1, 0.3, 0.4, 1.3);

    // Both caches are sized in device pixels, so a resize or a move to a
    // different-DPI display invalidates them.
    this.sprites.clear();
    this.buildLayers();

    // Everything on screen was just rebuilt.
    this.dirty = true;
  }

  // ---------- filters ----------

  // Each filter is defined by the same function that draws the thing it names, so
  // "larger" cannot come to mean something different from what looks larger.
  //
  // The size threshold is derived from the current range rather than fixed,
  // because the score scale is not ours to assume: the classifier returns 1-5 and
  // the match threshold clips it at 4, so a hardcoded `score >= 6` looked
  // reasonable and matched nothing at all. A range midpoint keeps working whatever
  // the model emits.
  //
  // Several active filters union rather than intersect. "Show me the big ones and
  // the hot ones" is the question people actually ask; intersecting them would
  // usually return nothing and read as a broken control.
  passes(g) {
    return passesFilter(g, {
      filters: this.filters,
      hotThreshold: this.hotThreshold,
      bigCut: this.bigCut,
      radius: this.starRadius(g),
      alpha: this.starAlpha(g),
    });
  }

  computeBigCut() {
    this.bigCut = bigCutFor(this.groups.map((g) => this.starRadius(g)));
  }

  setFilters(names) {
    this.filters = new Set(names);
    this.applyFilters();
  }

  applyFilters() {
    this.shown = this.stars.filter((s) => this.passes(s.g));
    if (this.hoverStar && !this.shown.includes(this.hoverStar)) {
      this.hoverStar = null;
      this.onHover(null);
    }
    this.buildConstellations();
    this.dirty = true;
  }

  // Group `shown` by topic once, instead of once per topic per frame.
  //
  // Everything here is fixed until the next rebuild: star positions are set by
  // layout() and relax(), membership comes from the signal, and the label text
  // never changes. drawConstellations was recomputing all of it 60 times a
  // second -- three array allocations and an O(stars) scan per topic per frame,
  // plus a spread into Math.min -- which is garbage the collector then has to
  // chase while the reader is trying to pan.
  buildConstellations() {
    const centres = this.centres;
    if (!centres) { this.constellations = []; return }

    const byTopic = new Map();
    for (const t of this.topics) byTopic.set(t.id, []);
    for (const s of this.shown) {
      for (const tid of s.g.topicIds) byTopic.get(tid)?.push(s);
    }

    this.constellations = [];
    for (const t of this.topics) {
      const centre = centres.get(t.id);
      if (!centre) continue;
      const pts = byTopic.get(t.id) || [];

      // The label is lifted by the topic's own stars only; a shared star sits
      // between constellations and must not drag the name away from where the
      // reader learned it (§6, spatial memory).
      let minOwnY = centre.y;
      const shared = [];
      for (const p of pts) {
        if (p.shared) shared.push(p);
        else if (p.y < minOwnY) minOwnY = p.y;
      }

      this.constellations.push({
        centre, pts, shared, minOwnY,
        // Computed here, not per frame: the positions and memberships behind it
        // are fixed until the next rebuild.
        edges: constellationEdges(pts),
        label: t.title.toUpperCase(),
        count: pts.length ? `· ${pts.length} ·` : '· no signal ·',
        dim: pts.length === 0,
      });
    }
  }

  // ---------- rendering ----------

  tapered(ctx, x1, y1, x2, y2, rgb, alpha, w) {
    const g = ctx.createLinearGradient(x1, y1, x2, y2);
    g.addColorStop(0, `rgba(${rgb},${alpha})`);
    g.addColorStop(1, `rgba(${rgb},0)`);
    ctx.strokeStyle = g;
    ctx.lineWidth = w;
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    ctx.lineTo(x2, y2);
    ctx.stroke();
  }

  // drawStar is the full-quality path: ~6 gradient objects built fresh every
  // call. Used for hot stars (whose glint rotates and whose halo pulses), for the
  // hovered star, and once per sprite bake. NOT used for every star every frame —
  // see frame() and spriteFor.
  //
  // `flat` bakes the star at rest: no twinkle, no pulse, no glint rotation, no
  // hover. That is what makes a sprite reusable across frames.
  drawStar(ctx, s, now, { alpha, flat, part, spin } = {}) {
    const hot = this.isHot(s.g);
    const c = hot ? HOT : s.temp;

    // Organic twinkle from two incommensurate sines, so it never visibly loops.
    const tw = (this.still || flat) ? 1
      : 0.9 + 0.06 * Math.sin(now / 860 + s.phase) + 0.04 * Math.sin(now / 347 + s.ph2);
    const pulse = hot && !this.still && !flat ? 1 + 0.2 * Math.sin(now / 520 + s.phase) : 1;
    const hover = (!flat && this.hoverStar === s) ? 1.2 : 1;
    const R = s.r * tw * hover;
    const a = alpha === undefined ? s.a : alpha;

    // part selects which layers to draw, so they can be baked separately and
    // reassembled. Two rules decide the split.
    //
    // By motion, because a sprite can only carry a rigid transform: 'pulsing'
    // scales with the hot pulse, 'glint' also rotates, 'glow' does neither.
    //
    // By required sharpness, which is the subtler one. Blitting at a fractional
    // scale resamples bilinearly, and §6 asks specifically for "a hard bright
    // core — the pinprick that makes it read as a star, not a blob". Softening it
    // measurably changed the star: mean channel error 5.7, with 1183 pixels off by
    // more than 32. So the core is never baked; it is two solid arcs and costs
    // almost nothing to draw exactly. Everything above it is gradient work, where
    // resampling is invisible because the source is already soft.
    const layers = {
      body:    ['halo', 'spikes', 'diagonals', 'glow'],  // cold stars: one sprite
      pulsing: ['halo', 'spikes'],
      glint:   ['diagonals'],
      glow:    ['glow'],
      core:    ['core'],
    };
    const active = part === undefined
      ? ['halo', 'spikes', 'diagonals', 'glow', 'core']
      : (layers[part] || []);
    const want = (p) => active.includes(p);
    const spinBase = spin === undefined ? s.spin : spin;

    // 1. chromatic outer halo
    let g;
    if (want('halo')) {
      const haloR = R * (hot ? 4.6 : 3.2) * pulse;
      g = ctx.createRadialGradient(s.x, s.y, 0, s.x, s.y, haloR);
      g.addColorStop(0, `rgba(${c.mid},${0.5 * a})`);
      g.addColorStop(0.45, `rgba(${c.halo},${0.16 * a})`);
      g.addColorStop(1, `rgba(${c.halo},0)`);
      ctx.fillStyle = g;
      ctx.beginPath(); ctx.arc(s.x, s.y, haloR, 0, 7); ctx.fill();
    }

    // 2. diffraction spikes — the vertical pair longer, like refractor optics
    if (s.r > 5.5) {
      const L = R * (hot ? 3.4 : 2.7) * pulse;
      const rgb = hot ? HOT.mid : c.mid;
      const sa = (hot ? 0.6 : 0.45) * a;
      if (want('spikes')) {
        this.tapered(ctx, s.x, s.y, s.x, s.y - L * 1.15, rgb, sa, 1.1);
        this.tapered(ctx, s.x, s.y, s.x, s.y + L * 1.15, rgb, sa, 1.1);
        this.tapered(ctx, s.x, s.y, s.x - L, s.y, rgb, sa * 0.85, 1);
        this.tapered(ctx, s.x, s.y, s.x + L, s.y, rgb, sa * 0.85, 1);
      }

      // Secondary diagonals on the largest stars; on hot ones they rotate.
      if (s.r > 9 && want('diagonals')) {
        const rot = hot && !this.still && !flat ? now / 9000 : 0;
        const d = L * 0.5;
        const ca = Math.cos(Math.PI / 4 + rot + spinBase);
        const sb = Math.sin(Math.PI / 4 + rot + spinBase);
        this.tapered(ctx, s.x, s.y, s.x + ca * d, s.y + sb * d, rgb, sa * 0.5, 0.8);
        this.tapered(ctx, s.x, s.y, s.x - ca * d, s.y - sb * d, rgb, sa * 0.5, 0.8);
        this.tapered(ctx, s.x, s.y, s.x - sb * d, s.y + ca * d, rgb, sa * 0.5, 0.8);
        this.tapered(ctx, s.x, s.y, s.x + sb * d, s.y - ca * d, rgb, sa * 0.5, 0.8);
      }
    }

    // 3. mid glow
    if (want('glow')) {
    g = ctx.createRadialGradient(s.x, s.y, 0, s.x, s.y, R * 1.15);
    g.addColorStop(0, `rgba(${c.core},${0.95 * a})`);
    g.addColorStop(0.6, `rgba(${c.mid},${0.55 * a})`);
    g.addColorStop(1, `rgba(${c.mid},0)`);
    ctx.fillStyle = g;
    ctx.beginPath(); ctx.arc(s.x, s.y, R * 1.15, 0, 7); ctx.fill();
    }

    // 4. hard bright core — the pinprick that makes it read as a star, not a blob.
    // Never baked: see the note on `layers` above.
    if (want('core')) {
      ctx.fillStyle = `rgba(${c.core},${a})`;
      ctx.beginPath(); ctx.arc(s.x, s.y, Math.max(R * 0.28, 1.4), 0, 7); ctx.fill();
      ctx.fillStyle = `rgba(255,255,255,${0.9 * a})`;
      ctx.beginPath(); ctx.arc(s.x - R * 0.07, s.y - R * 0.07, Math.max(R * 0.12, 0.7), 0, 7); ctx.fill();
    }
  }

  // A star pre-rendered once into an offscreen canvas, then blitted.
  //
  // §6 flagged canvas performance past 100 stars as an unsolved problem, and
  // measurement found it is not the star count that hurts — it is that the direct
  // path allocates a fresh gradient object per halo, per glow and per diffraction
  // spike. Measured per star: 0.024ms blitted against 0.22ms drawn, a factor of
  // nine that does not shrink with scale.
  //
  // A cold star bakes whole. A hot one cannot, because its halo pulses and its
  // glint rotates — so it bakes in three pieces split by how each piece *moves*:
  //
  //   pulsing  halo + main spikes   scale by tw * pulse
  //   static   glow + core          scale by tw only
  //   glint    rotating diagonals   scale by tw * pulse, rotate by rot + spin
  //
  // Reassembled at blit time this is visually faithful but not bit-exact, and it is
  // worth knowing which. Twinkle is brightness and size, which globalAlpha and a
  // blit scale reproduce exactly; the pulse and rotation are rigid transforms. What
  // does differ is resampling: blitting at a fractional scale redistributes the
  // gradient tails slightly. Measured against the direct path, the residual sits in
  // a thin ring at the outer edge of the halo where alpha is already near zero —
  // spikes and core come out identical, and at 3x magnification the two are
  // indistinguishable. Three blits and two arcs, zero gradients per frame.
  //
  // The glint bakes at spin 0 and takes its per-star offset from the blit rotation,
  // so all hot stars of a size share one glint sprite instead of one each.
  spriteFor(s, part) {
    const hot = this.isHot(s.g);
    // The radius bucket is deliberately coarse -- that is what lets stars share
    // sprites -- but drawStar gates whole layers on the exact radius (spikes at
    // r > 5.5, diagonals at r > 9), and those thresholds fall inside a bucket.
    // Without them in the key, r=5.4 and r=5.6 share one sprite and whichever
    // baked first decides whether the other gets diffraction spikes, flipping
    // with iteration order. Two characters in the key, and size once again
    // encodes relevance the way §6 reads it.
    const key = `${Math.round(s.r * 2)}|${s.r > 5.5 ? 1 : 0}${s.r > 9 ? 1 : 0}`
      + `|${TEMPS.indexOf(s.temp)}|${hot ? 'h' : 'c'}|${part || 'all'}`;
    let sprite = this.sprites.get(key);
    if (sprite) return sprite;

    // Padding is per layer, sized to that layer's own reach. Baked at pulse 1; the
    // blit scale carries the pulse, so no extra room is needed for it.
    const reach = part === 'glow' ? 1.5
      : part === 'glint' ? 2.0
      : part === 'pulsing' ? (hot ? 4.8 : 3.4)
      : 3.9;   // 'body' — halo, spikes, diagonals and glow together
    const pad = Math.ceil(s.r * reach) + 4;
    const size = pad * 2;
    const off = document.createElement('canvas');
    off.width = off.height = Math.ceil(size * this.dpr);
    const octx = off.getContext('2d');
    octx.scale(this.dpr, this.dpr);

    this.drawStar(octx, { ...s, x: pad, y: pad }, 0,
      { alpha: 1, flat: true, part, spin: part === 'glint' ? 0 : undefined });

    sprite = { canvas: off, pad, size };
    this.sprites.set(key, sprite);
    return sprite;
  }

  // Blit one baked layer, scaled about the star's centre.
  blit(ctx, s, part, scale, alpha) {
    // Resolve through the star's own cache. spriteFor's key is a template
    // string plus a linear TEMPS scan, and this runs per star, per layer, per
    // frame -- tens of thousands of throwaway strings a second for a value that
    // cannot change without a rebuild. The sprites themselves stay shared in
    // this.sprites; only the lookup is memoized here.
    let sp = s.sp[part];
    if (!sp) sp = s.sp[part] = this.spriteFor(s, part);
    ctx.globalAlpha = alpha;
    ctx.drawImage(sp.canvas, s.x - sp.pad * scale, s.y - sp.pad * scale,
      sp.size * scale, sp.size * scale);
  }

  drawConstellations() {
    const ctx = this.ctx;
    const inv = 1 / this.view.s;

    // Setting ctx.font re-parses the shorthand and re-resolves the webfont, and
    // it was being set once per topic with a size of 11.5 / view.s -- a fresh
    // fractional value on every eased frame. Seven parses a frame, and only
    // while the view was moving, which is exactly when the sky felt stuck.
    //
    // Two changes: hoist it out of the loop, and quantize the scale so the
    // string repeats and the parse cache hits. The label's on-screen size then
    // drifts by at most 1/64 of itself -- under a fifth of a pixel at 11.5px,
    // and it snaps back the moment the ease settles.
    const sQ = Math.round(this.view.s * 64) / 64 || this.view.s;
    if (sQ !== this._fontScale) {
      this._fontScale = sQ;
      this._fontStr = `${11.5 / sQ}px 'Noto Sans', system-ui, sans-serif`;
    }
    ctx.font = this._fontStr;
    ctx.textAlign = 'center';

    // Lines are toggleable; labels are not. §10 requires an empty topic to render
    // "No signal today" rather than disappear, and the label is the only place
    // the sky says that — so a switch that hid it would turn an acceptance
    // criterion off. Joins and ties carry no such guarantee.
    const lines = this.show.lines;

    for (const c of this.constellations) {
      const { centre, pts } = c;

      // The join, as a spanning tree rather than one polyline. Each edge is its
      // own subpath: they branch, so there is no single stroke order through them.
      if (lines && c.edges.length) {
        ctx.strokeStyle = 'rgba(132,172,255,.5)';
        ctx.lineWidth = 1.3 * inv;
        ctx.setLineDash([2 * inv, 4 * inv]);
        ctx.beginPath();
        for (const [a, b] of c.edges) {
          ctx.moveTo(a.x, a.y);
          ctx.lineTo(b.x, b.y);
        }
        ctx.stroke();
        ctx.setLineDash([]);
      }

      // A shared star gets its own dashed tie to each constellation it belongs
      // to, so "between constellations" reads as membership, not as drift.
      if (lines && c.shared.length) {
        // Dimmer than the join, and a finer dash: a tie states membership, the
        // join states shape, and the two must not be mistaken for each other.
        ctx.strokeStyle = 'rgba(132,172,255,.3)';
        ctx.lineWidth = inv;
        ctx.setLineDash([inv, 4 * inv]);
        for (const p of c.shared) {
          ctx.beginPath();
          ctx.moveTo(p.x, p.y);
          ctx.lineTo(centre.x, centre.y);
          ctx.stroke();
        }
        ctx.setLineDash([]);
      }

      const labelX = centre.x;
      const labelY = c.minOwnY - 26 * inv;

      ctx.fillStyle = c.dim ? 'rgba(158,195,255,.35)' : 'rgba(158,195,255,.8)';
      ctx.fillText(c.label, labelX, labelY);
      ctx.fillStyle = 'rgba(74,86,124,.8)';
      ctx.fillText(c.count, labelX, labelY + 14 * inv);
    }
  }

  // Dust is 200 particles across two parallax layers. Drawn naively that is 200
  // fillStyle assignments a frame — each one a string build plus a CSS colour
  // parse. Bucketing the twinkled alpha to 1/24ths collapses that to ~9 styles
  // per layer; the quantization is finer than the eye resolves on a 1px dot.
  //
  // The bucket decides the *colour*, not the path. Collecting each bucket's dots
  // into one path and issuing a single fill() looks like the same optimization
  // carried one step further, and it was measured to cost two thirds of the frame
  // rate: 21fps against 55 for the identical dots filled one at a time, in both a
  // GPU window and a software-rasterized one. The dots in a bucket are scattered
  // across the whole sky, so one path per bucket means ~18 fills a frame whose
  // bounding boxes each span the entire canvas, and the rasterizer sets up over
  // the box rather than over the ink. Batching draw calls is usually the right
  // instinct; batching *geometry this sparse* into one path inverts it.
  //
  // Per-dot fills also blend overlaps twice where one path with nonzero winding
  // merged them. At these radii, scattered over 1.6x the viewport, that is not a
  // visible difference.
  drawDust(list, parallax, now) {
    const ctx = this.ctx;
    ctx.save();
    ctx.setTransform(this.dpr, 0, 0, this.dpr, this.view.x * parallax * this.dpr, this.view.y * parallax * this.dpr);
    const sc = 1 + (this.view.s - 1) * parallax;
    ctx.scale(sc, sc);

    const buckets = new Map();
    for (const d of list) {
      const tw = this.still ? 1 : 0.55 + 0.45 * Math.sin((now / 1500) * d.sp + d.ph);
      const q = Math.max(1, Math.round(d.a * tw * 24));
      let b = buckets.get(q);
      if (!b) buckets.set(q, (b = []));
      b.push(d);
    }
    for (const [q, group] of buckets) {
      ctx.fillStyle = `rgba(200,215,245,${q / 24})`;
      for (const d of group) {
        ctx.beginPath();
        ctx.arc(d.x, d.y, d.r, 0, 7);
        ctx.fill();
      }
    }
    ctx.restore();
  }

  // Fetch the band photograph and re-bake the background once it decodes.
  //
  // Asynchronous, and the first layout has already run by the time it lands, so
  // the sky opens on the procedural band and swaps to the photograph a frame or
  // two later. That order is deliberate: it means a slow decode shows a finished
  // sky rather than an empty one, and a failed decode simply never swaps.
  loadBand() {
    if (typeof Image === 'undefined') return;   // imported outside a browser
    const img = new Image();
    img.onload = () => {
      this.bandImage = img;
      this.buildLayers();
      this.dirty = true;   // the frame loop may be idle; see frame()
    };
    // No onerror handling beyond leaving bandImage null: drawMilkyWay is the
    // fallback and is what has already been drawn.
    img.src = BAND_IMAGE_SRC;
  }

  // The photographic band, cover-fitted into the background layer.
  //
  // Drawn additively. The photograph is light on black, so 'lighter' lets the
  // band and its star grain add to the gradient underneath while the empty sky
  // between them contributes nothing. Source-over would paint that black over the
  // gradient instead, flattening the depth the gradient and nebula exist to give
  // — the picture would sit *on* the sky rather than be part of it.
  //
  // Cover rather than contain: the photograph's aspect will not match the
  // viewport at most window sizes, and cropping a sky is much better than
  // letterboxing one.
  drawBandPhoto(c) {
    const img = this.bandImage;
    const iw = img.naturalWidth || img.width;
    const ih = img.naturalHeight || img.height;
    if (!iw || !ih) return;

    const scale = Math.max(this.W / iw, this.H / ih);
    const w = iw * scale, h = ih * scale;

    c.save();
    c.globalCompositeOperation = 'lighter';
    c.globalAlpha = BAND_STRENGTH;
    c.drawImage(img, (this.W - w) / 2, (this.H - h) / 2, w, h);
    c.restore();
  }

  // A galactic band across the sky, drawn by hand.
  //
  // The fallback since the photograph landed — used when the image has not
  // decoded yet, or cannot be.
  //
  // Baked into the background layer, so it costs nothing per frame — which is the
  // only reason it can afford to be this many gradients. Three ingredients do the
  // work, and the third is the one people notice without knowing why: a bright
  // haze, clumps to break it up, and a dark rift down the middle. Real skies have
  // that rift (dust blocking the galactic core), and without it a band like this
  // reads as a smear of fog rather than something far away.
  //
  // Seeded, like everything else here: the sky must look the same on every reload
  // or the spatial memory §6 protects is worthless.
  drawMilkyWay(c) {
    const rnd = mulberry32(31337);
    const span = Math.hypot(this.W, this.H) * 1.25;  // along the band, past both edges
    const width = Math.min(this.W, this.H) * 0.62;   // across it

    c.save();
    c.translate(this.W * 0.52, this.H * 0.46);
    c.rotate(-0.4);

    // 1. the haze. Fades out across the band, and runs off both edges so it needs
    //    no taper along its length.
    const haze = c.createLinearGradient(0, -width / 2, 0, width / 2);
    haze.addColorStop(0, 'rgba(116,146,224,0)');
    haze.addColorStop(0.26, 'rgba(132,162,236,.035)');
    haze.addColorStop(0.44, 'rgba(178,198,255,.062)');
    haze.addColorStop(0.56, 'rgba(178,198,255,.062)');
    haze.addColorStop(0.74, 'rgba(132,162,236,.035)');
    haze.addColorStop(1, 'rgba(116,146,224,0)');
    c.fillStyle = haze;
    c.fillRect(-span / 2, -width / 2, span, width);

    // 2. clumps, so the band is not a uniform stripe. A few are warm, which is
    //    what stops the whole sky reading as one flat blue.
    for (let i = 0; i < 26; i++) {
      const x = (rnd() - 0.5) * span * 0.95;
      const y = (rnd() - 0.5) * width * 0.5;
      const r = width * (0.10 + rnd() * 0.26);
      const warm = rnd() < 0.3;
      const g = c.createRadialGradient(x, y, 0, x, y, r);
      const a = 0.020 + rnd() * 0.030;
      g.addColorStop(0, warm ? `rgba(226,206,236,${a})` : `rgba(176,198,255,${a})`);
      g.addColorStop(1, warm ? 'rgba(226,206,236,0)' : 'rgba(176,198,255,0)');
      c.fillStyle = g;
      c.beginPath(); c.arc(x, y, r, 0, 7); c.fill();
    }

    // 3. the rift, plus a few dark knots hanging off it.
    const rift = c.createLinearGradient(0, -width * 0.11, 0, width * 0.15);
    rift.addColorStop(0, 'rgba(6,10,24,0)');
    rift.addColorStop(0.5, 'rgba(6,10,24,.5)');
    rift.addColorStop(1, 'rgba(6,10,24,0)');
    c.fillStyle = rift;
    c.fillRect(-span / 2, -width * 0.11, span, width * 0.26);

    for (let i = 0; i < 12; i++) {
      const x = (rnd() - 0.5) * span * 0.9;
      const y = (rnd() - 0.5) * width * 0.24;
      const r = width * (0.05 + rnd() * 0.13);
      const g = c.createRadialGradient(x, y, 0, x, y, r);
      g.addColorStop(0, `rgba(5,8,20,${0.16 + rnd() * 0.2})`);
      g.addColorStop(1, 'rgba(5,8,20,0)');
      c.fillStyle = g;
      c.beginPath(); c.arc(x, y, r, 0, 7); c.fill();
    }

    // 4. the grain: unresolved stars, densest on the band's axis. Summing two
    //    uniforms clusters them toward the middle instead of spreading them evenly,
    //    which is what makes the band look like stars rather than paint.
    for (let i = 0; i < 620; i++) {
      const x = (rnd() - 0.5) * span;
      const y = ((rnd() + rnd() + rnd()) / 3 - 0.5) * width * 1.05;
      const fall = 1 - Math.min(1, Math.abs(y) / (width * 0.5));
      const a = (0.10 + rnd() * 0.5) * (0.35 + fall * 0.65);
      c.fillStyle = `rgba(226,235,255,${a.toFixed(3)})`;
      c.beginPath(); c.arc(x, y, 0.28 + rnd() * 0.72, 0, 7); c.fill();
    }

    c.restore();
  }

  // The background gradient, nebula and graticule never change between frames —
  // only the view transform does. Baking them once per layout turns four gradient
  // allocations and nine long arc strokes per frame into two blits.
  //
  // The graticule lives in world space, so its layer is drawn under the same
  // transform as the stars. Its line width is baked at 1 device pixel and then
  // scales with zoom, which is what a real chart engraving would do.
  buildLayers() {
    const dpr = this.dpr;

    const screen = document.createElement('canvas');
    screen.width = Math.ceil(this.W * dpr);
    screen.height = Math.ceil(this.H * dpr);
    // Opaque for the same reason as the main context, and it matters more here:
    // blitting a full-screen source *with* alpha forced a per-pixel source-over
    // blend every frame, which measured at 6.3ms — over a third of the budget, for
    // an image whose every pixel is opaque anyway.
    let c = screen.getContext('2d', { alpha: false });
    c.scale(dpr, dpr);

    const bg = c.createLinearGradient(0, 0, this.W, this.H);
    bg.addColorStop(0, '#060A18');
    bg.addColorStop(0.55, '#0A1026');
    bg.addColorStop(1, '#0B0F24');
    c.fillStyle = bg;
    c.fillRect(0, 0, this.W, this.H);

    // The soft nebula belongs to the hand-drawn sky. The photograph carries its
    // own structure, and adding a broad blue lift underneath it flattens exactly
    // the contrast that makes a real dust lane read as one — the band went hazy
    // and the black between the stars stopped being black.
    const usingPhoto = this.show.milkyway && this.bandImage;
    if (!usingPhoto) {
      const neb = c.createRadialGradient(this.W * 0.44, this.H * 0.28, 0,
        this.W * 0.44, this.H * 0.28, Math.max(this.W, this.H) * 0.6);
      neb.addColorStop(0, 'rgba(38,53,94,.30)');
      neb.addColorStop(1, 'rgba(38,53,94,0)');
      c.fillStyle = neb;
      c.fillRect(0, 0, this.W, this.H);
    }

    if (this.show.milkyway) {
      if (this.bandImage) this.drawBandPhoto(c);
      else this.drawMilkyWay(c);
    }

    // Corner falloff. The single cheapest depth cue there is: a flat field reads
    // as paper, a darkened rim reads as a dome.
    const vig = c.createRadialGradient(this.W * 0.5, this.H * 0.5, Math.min(this.W, this.H) * 0.32,
      this.W * 0.5, this.H * 0.5, Math.hypot(this.W, this.H) * 0.62);
    vig.addColorStop(0, 'rgba(2,4,12,0)');
    vig.addColorStop(1, 'rgba(2,4,12,.55)');
    c.fillStyle = vig;
    c.fillRect(0, 0, this.W, this.H);

    this.bgLayer = screen;

    const grid = document.createElement('canvas');
    grid.width = Math.ceil(this.W * dpr);
    grid.height = Math.ceil(this.H * dpr);
    c = grid.getContext('2d');
    c.scale(dpr, dpr);
    c.lineWidth = 1 / dpr;
    c.strokeStyle = 'rgba(20,30,63,.55)';
    for (let i = 1; i <= 4; i++) {
      c.beginPath();
      c.arc(this.W * 0.5, this.H * 2.1, this.H * 1.55 + i * 90, Math.PI * 1.28, Math.PI * 1.72);
      c.stroke();
    }
    c.strokeStyle = 'rgba(20,30,63,.30)';
    for (let i = 1; i <= 5; i++) {
      const x = (this.W / 6) * i;
      c.beginPath(); c.moveTo(x, 70); c.lineTo(x, this.H - 60); c.stroke();
    }
    this.gridLayer = grid;
  }

  // One streak at a time, a few seconds apart. This is the only thing on the
  // screen that allocates gradients outside the hot-star path, which is affordable
  // precisely because it is rare and singular — spawning a shower here would undo
  // the frame budget the sprite work bought.
  //
  // Reduced motion gets none of it. §6 disables twinkle, pulse, glint and parallax
  // easing, and a meteor is the most motion on the screen; exempting it because it
  // is decorative would be exactly backwards.
  updateMeteor(now) {
    if (this.still) { this.meteor = null; return }

    if (!this.nextMeteor) {
      // Don't fire one the instant the page opens; let the sky settle first.
      this.nextMeteor = now + 4000 + Math.random() * 9000;
    }

    if (!this.meteor && now >= this.nextMeteor) {
      // Enter along the top or one side, heading down-and-across. Angle stays well
      // clear of vertical so the streak reads as a trajectory, not a falling dot.
      const dir = Math.random() < 0.5 ? 1 : -1;
      const angle = (0.28 + Math.random() * 0.42) * Math.PI * (dir > 0 ? 1 : 1);
      const speed = 0.55 + Math.random() * 0.75;             // px per ms
      const vx = Math.cos(angle) * speed * dir;
      const vy = Math.sin(angle) * speed;
      const margin = 120;
      this.meteor = {
        x: dir > 0 ? -margin + Math.random() * this.W * 0.5
                   : this.W + margin - Math.random() * this.W * 0.5,
        y: -margin + Math.random() * this.H * 0.35,
        vx, vy,
        life: 0,
        span: 900 + Math.random() * 900,
        len: 90 + Math.random() * 160,
        warm: Math.random() < 0.35,
      };
    }

    const m = this.meteor;
    if (!m) return;

    // Integrate on the real frame delta, not an assumed 60fps. The speeds above
    // are px/ms and the span is in ms, but a hardcoded 16.7 made both scale with
    // the display: on a 120Hz panel every streak crossed at double speed and
    // faded halfway through its arc, and a throttled frame made it crawl.
    //
    // Clamped because `now` can jump by seconds after a stall or a resumed tab,
    // and an unclamped delta would teleport the streak off screen in one step.
    const dt = this.lastNow === null ? 16.7 : Math.min(now - this.lastNow, 50);

    m.life += dt;
    m.x += m.vx * dt;
    m.y += m.vy * dt;

    const off = m.x < -400 || m.x > this.W + 400 || m.y > this.H + 400;
    if (m.life > m.span || off) {
      this.meteor = null;
      this.nextMeteor = now + 7000 + Math.random() * 18000;
    }
  }

  drawMeteor() {
    const m = this.meteor;
    if (!m) return;
    const ctx = this.ctx;

    // Fade in and out over the streak's life so it never pops on or off.
    const t = m.life / m.span;
    const fade = Math.min(1, t / 0.18) * Math.min(1, (1 - t) / 0.32);
    if (fade <= 0) return;

    const speed = Math.hypot(m.vx, m.vy) || 1;
    const tailX = m.x - (m.vx / speed) * m.len;
    const tailY = m.y - (m.vy / speed) * m.len;
    const rgb = m.warm ? '255,224,180' : '214,232,255';

    ctx.save();
    ctx.lineCap = 'round';
    this.tapered(ctx, m.x, m.y, tailX, tailY, rgb, 0.7 * fade, 1.7);
    this.tapered(ctx, m.x, m.y, tailX - (m.vx / speed) * m.len * 0.5,
                 tailY - (m.vy / speed) * m.len * 0.5, rgb, 0.22 * fade, 3.4);

    // A small bright head, or the streak looks like a scratch on the glass.
    const head = ctx.createRadialGradient(m.x, m.y, 0, m.x, m.y, 7);
    head.addColorStop(0, `rgba(255,255,255,${0.95 * fade})`);
    head.addColorStop(0.4, `rgba(${rgb},${0.5 * fade})`);
    head.addColorStop(1, `rgba(${rgb},0)`);
    ctx.fillStyle = head;
    ctx.beginPath(); ctx.arc(m.x, m.y, 7, 0, 7); ctx.fill();
    ctx.restore();
  }

  frame(now) {
    if (!this.running) return;

    // Re-schedule before drawing anything. Anything that throws below would
    // otherwise stop the loop for good, turning a transient glitch into a
    // permanently frozen sky — the worst possible failure for the front door.
    // The handle is kept so stop() can cancel this one too, not just the one
    // start() queued.
    this.raf = requestAnimationFrame((t) => this.frame(t));

    if (!this.dpr || !this.W || !this.H || !this.bgLayer) return;

    const ctx = this.ctx;

    // Ease toward the target. The rate is a responsiveness/smoothness trade: too
    // low and a zoom feels like it lags the wheel, which reads as the app being
    // slow rather than as easing.
    const moved = this.easeView();

    // After the ease, before the draw: the hit test then uses the same view the
    // frame is about to render, and hoverStar stays consistent within the frame.
    this.updateHover();

    // A change in the motion gate repaints once, to settle the stars at rest or
    // to start them moving again. This covers the system preference flipping
    // mid-session as well as the Layers switch.
    if (this.still !== this.wasStill) {
      this.wasStill = this.still;
      this.dirty = true;
    }

    // The skip. Motion on means every star twinkles, so that alone forces a
    // repaint every frame; the meteor needs no separate test, because
    // updateMeteor never spawns one while `still` is true.
    if (this.still && !moved && !this.dirty) {
      this.idle = true;
      this.fps = 0;
      this.fpsFrames = 0;
      this.fpsSince = 0;
      this.lastNow = now;
      return;
    }
    this.idle = false;
    this.dirty = false;

    ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0);
    ctx.globalAlpha = 1;
    // 'copy' overwrites rather than blends, and doubles as the clear: the layer
    // covers every pixel, so there is nothing left of the previous frame to erase.
    ctx.globalCompositeOperation = 'copy';
    ctx.drawImage(this.bgLayer, 0, 0, this.W, this.H);
    ctx.globalCompositeOperation = 'source-over';

    if (this.show.dust) {
      this.drawDust(this.dustFar, 0.25, now);
      this.drawDust(this.dustNear, 0.55, now);
    }

    // Behind the world layer on purpose: a meteor crossing *in front of* the
    // signals would compete with them for attention.
    //
    // Switched off, the streak in flight is dropped rather than left frozen
    // mid-arc — updateMeteor owns that state and is the thing being skipped.
    if (this.show.meteors) {
      this.updateMeteor(now);
      this.drawMeteor();
    } else {
      this.meteor = null;
    }

    // World layer.
    ctx.setTransform(this.dpr, 0, 0, this.dpr, this.view.x * this.dpr, this.view.y * this.dpr);
    ctx.scale(this.view.s, this.view.s);

    if (this.show.grid) ctx.drawImage(this.gridLayer, 0, 0, this.W, this.H);
    this.drawConstellations();

    // Every star's gradient work blits; every star's core is drawn exactly. The
    // hovered star is the one exception and goes fully through the direct path,
    // because at 1.2x even the soft layers would visibly blur on the one star the
    // reader is looking straight at.
    const hovered = this.hoverStar;
    const hotList = [];
    const bounds = this.viewBounds();
    let drawn = 0;
    for (const s of this.shown) {
      if (s === hovered) continue;
      if (!this.onScreen(s, bounds)) continue;
      drawn++;
      const tw = this.still ? 1
        : 0.9 + 0.06 * Math.sin(now / 860 + s.phase) + 0.04 * Math.sin(now / 347 + s.ph2);

      if (this.isHot(s.g)) { hotList.push({ s, tw }); continue }

      // Cold: nothing pulses or rotates, so all four soft layers share one sprite.
      this.blit(ctx, s, 'body', tw, s.a);
      ctx.globalAlpha = 1;
      this.drawStar(ctx, s, now, { part: 'core' });
    }

    // Hot stars after the field, so they sit on top of it.
    for (const { s, tw } of hotList) {
      const pulse = this.still ? 1 : 1 + 0.2 * Math.sin(now / 520 + s.phase);
      this.blit(ctx, s, 'pulsing', tw * pulse, s.a);

      if (s.r > 9) {
        const rot = this.still ? 0 : now / 9000;
        ctx.save();
        ctx.translate(s.x, s.y);
        ctx.rotate(rot + s.spin);
        ctx.translate(-s.x, -s.y);
        this.blit(ctx, s, 'glint', tw * pulse, s.a);
        ctx.restore();
      }

      this.blit(ctx, s, 'glow', tw, s.a);
      ctx.globalAlpha = 1;
      this.drawStar(ctx, s, now, { part: 'core' });
    }

    if (hovered) this.drawStar(ctx, hovered, now);

    // How many of `shown` actually got drawn. Only a diagnostic, but the whole
    // point of culling is invisible when it works, and this is how you check it
    // from the console rather than by trusting the frame rate.
    this.drawn = drawn;

    // Averaged over half a second. Per-frame deltas swing far too much to read,
    // and the panel only repaints twice a second anyway.
    this.fpsFrames++;
    if (!this.fpsSince) this.fpsSince = now;
    else if (now - this.fpsSince >= 500) {
      this.fps = Math.round((this.fpsFrames * 1000) / (now - this.fpsSince));
      this.fpsFrames = 0;
      this.fpsSince = now;
    }

    // Last, so updateMeteor above measured against the previous frame.
    this.lastNow = now;
  }

  // ---------- interaction ----------

  // Consume at most one pointer sample per frame.
  //
  // The "is the pointer over the canvas" test is why the sample carries it: the
  // mousemove listener lives on `window` so a drag that leaves the canvas keeps
  // panning, but that also means the detail panel, the settings sheet, the
  // legend and the zoom controls sit *over* the canvas without blocking it, and
  // moving across any of them was raising tooltips for whatever star was behind.
  updateHover() {
    if (!this.pointerDirty) return;
    this.pointerDirty = false;
    const p = this.pointer;
    if (!p) return;

    if (!p.over) {
      if (this.hoverStar) {
        this.hoverStar = null;
        this.dirty = true;
        this.canvas.classList.remove('pointing');
        this.onHover(null);
      }
      return;
    }

    const w = this.screenToWorld(p.x, p.y);
    const s = this.starAtWorld(w.x, w.y);
    // Only a *change* of hovered star changes the picture — the hovered one is
    // drawn 1.2x through the direct path. Moving the pointer within the same
    // star, or across empty sky, repaints nothing. onHover still fires either
    // way: the tooltip follows the cursor, and it is DOM, not canvas.
    if (s !== this.hoverStar) {
      this.hoverStar = s;
      this.dirty = true;
    }
    this.canvas.classList.toggle('pointing', !!s);
    this.onHover(s ? s.g : null, p.x, p.y);
  }

  // Called when the list view covers the canvas. Clearing hover on the way out
  // matters: without it the last-hovered star stays enlarged behind the list and
  // its tooltip is left on screen.
  setInteractive(on) {
    this.interactive = on;
    if (!on) {
      this.clearDrag?.();
      this.pointer = null;
      this.pointerDirty = false;
      this.hoverStar = null;
      this.dirty = true;
      this.canvas.classList.remove('pointing', 'grabbing');
      this.onHover(null);
    }
  }

  screenToWorld(mx, my) {
    return { x: (mx - this.view.x) / this.view.s, y: (my - this.view.y) / this.view.s };
  }

  // The world-space rect currently on screen -- the inverse of the transform
  // frame() sets for the world layer, in the coordinates stars are laid out in.
  viewBounds() {
    const s = this.view.s;
    return {
      x0: -this.view.x / s,
      y0: -this.view.y / s,
      x1: (this.W - this.view.x) / s,
      y1: (this.H - this.view.y) / s,
    };
  }

  // Whether a star can put ink on screen.
  //
  // Nothing downstream knows a star is off screen: blit hands the whole sprite
  // to drawImage and lets the canvas clip it, which still costs the transform
  // and the fill setup, and the sprites are large. Measured in the browser, the
  // frame was ~3ms of JavaScript against ~60ms of rasterization while zooming,
  // so what the frame is actually made of is blits -- and when the sky is zoomed
  // in, most of them land entirely outside the canvas.
  //
  // The margin has to clear the widest soft layer, not the star: sprites pad to
  // 4.8x the radius and the hot pulse scales that by another 1.2, so a big star
  // culled on its centre alone would pop in at the edge with its halo already
  // sliced off. Being generous here costs nothing -- the win is the dense middle
  // of a zoomed sky, not the last star at the border. It also keeps culling and
  // hit-testing in agreement: starAtWorld reaches 1.7x a radius, far inside this,
  // so nothing hoverable is ever left undrawn.
  onScreen(s, b) {
    const m = s.r * 6 + 8;
    return s.x + m >= b.x0 && s.x - m <= b.x1 && s.y + m >= b.y0 && s.y - m <= b.y1;
  }

  clampScale(s) { return Math.min(MAX_SCALE, Math.max(MIN_SCALE, s)) }

  zoomAt(mx, my, factor) {
    const w = this.screenToWorld(mx, my);
    this.viewTarget.s = this.clampScale(this.viewTarget.s * factor);
    this.viewTarget.x = mx - w.x * this.viewTarget.s;
    this.viewTarget.y = my - w.y * this.viewTarget.s;
  }

  reset() { this.viewTarget = { x: 0, y: 0, s: 1 } }

  starAtWorld(wx, wy) {
    let best = null, bd = 1e9;
    for (const s of this.shown) {
      const d = Math.hypot(wx - s.x, wy - s.y);
      if (d < Math.max(s.r * 1.7, 14 / this.view.s) && d < bd) { best = s; bd = d }
    }
    return best;
  }

  bindEvents() {
    const canvas = this.canvas;

    canvas.addEventListener('wheel', (e) => {
      if (!this.interactive) return;
      e.preventDefault();
      this.zoomAt(e.clientX, e.clientY, Math.exp(-e.deltaY * 0.0028));
    }, { passive: false });

    let drag = null, moved = false;

    // setInteractive lives outside this closure but has to be able to drop a
    // half-finished drag: the guarded mouseup below returns early once
    // interaction is off, so a button released after switching to List view
    // never cleared `drag`, and coming back to Sky the first mouse movement
    // panned the map with no button held.
    this.clearDrag = () => { drag = null; moved = false; canvas.classList.remove('grabbing') };

    canvas.addEventListener('mousedown', (e) => {
      if (!this.interactive) return;
      drag = { mx: e.clientX, my: e.clientY, vx: this.viewTarget.x, vy: this.viewTarget.y };
      moved = false;
      canvas.classList.add('grabbing');
    });

    addEventListener('mousemove', (e) => {
      if (!this.interactive) return;
      if (drag) {
        const dx = e.clientX - drag.mx, dy = e.clientY - drag.my;
        if (Math.hypot(dx, dy) > 4) moved = true;
        this.viewTarget.x = drag.vx + dx;
        this.viewTarget.y = drag.vy + dy;
        this.view.x = this.viewTarget.x; // no easing mid-drag
        this.view.y = this.viewTarget.y;
        return;
      }
      // Record and defer. Hit-testing scans every star, and a high-polling-rate
      // mouse fires this several hundred times a second -- all but the last
      // between any two paints, so the rest was work nobody could see. frame()
      // tests once, against the eased view it is about to draw.
      this.pointer = { x: e.clientX, y: e.clientY, over: e.target === canvas };
      this.pointerDirty = true;
    });

    addEventListener('mouseup', (e) => {
      if (!this.interactive) return;
      if (drag && !moved) {
        const w = this.screenToWorld(e.clientX, e.clientY);
        const s = this.starAtWorld(w.x, w.y);
        if (s) this.onSelect(s.g);
      }
      drag = null;
      canvas.classList.remove('grabbing');
    });

    canvas.addEventListener('mouseleave', () => {
      // Drop the pending sample too, or the next frame re-hovers the star the
      // pointer just left.
      this.pointer = null;
      this.pointerDirty = false;
      if (this.hoverStar) this.dirty = true;
      this.hoverStar = null;
      this.onHover(null);
    });

    canvas.addEventListener('dblclick', () => this.reset());

    // Touch: one finger pans, two pinch.
    const touches = {};
    canvas.addEventListener('touchstart', (e) => {
      for (const t of e.changedTouches) touches[t.identifier] = { x: t.clientX, y: t.clientY };
    }, { passive: true });

    canvas.addEventListener('touchmove', (e) => {
      e.preventDefault();
      if (e.touches.length === 1) {
        const t = e.touches[0], p = touches[t.identifier];
        if (p) {
          this.viewTarget.x += t.clientX - p.x;
          this.viewTarget.y += t.clientY - p.y;
          this.view.x = this.viewTarget.x;
          this.view.y = this.viewTarget.y;
          touches[t.identifier] = { x: t.clientX, y: t.clientY };
        }
      } else if (e.touches.length === 2) {
        const [a, b] = e.touches;
        const pa = touches[a.identifier], pb = touches[b.identifier];
        if (pa && pb) {
          const d0 = Math.hypot(pa.x - pb.x, pa.y - pb.y);
          const d1 = Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
          this.zoomAt((a.clientX + b.clientX) / 2, (a.clientY + b.clientY) / 2, d1 / d0);
          touches[a.identifier] = { x: a.clientX, y: a.clientY };
          touches[b.identifier] = { x: b.clientX, y: b.clientY };
        }
      }
    }, { passive: false });

    // A tap with no drag opens the star under it.
    canvas.addEventListener('touchend', (e) => {
      const t = e.changedTouches[0];
      const start = touches[t.identifier];
      if (start && Math.hypot(t.clientX - start.x, t.clientY - start.y) < 8) {
        const w = this.screenToWorld(t.clientX, t.clientY);
        const s = this.starAtWorld(w.x, w.y);
        if (s) this.onSelect(s.g);
      }
      for (const ct of e.changedTouches) delete touches[ct.identifier];
    }, { passive: true });
  }
}

// groupSignals collapses signals that share a primary URL.
//
// The synthesizer runs per topic, so one story matching two topics produces two
// signal rows. They are the same star: §6 wants it placed between the
// constellations and listed under both topics, not drawn twice.
// passesFilter is the filter predicate, kept pure so it can be tested without a
// canvas. `radius` and `alpha` are passed in rather than recomputed because they
// are the caller's encodings — the whole point is that the filter and the drawing
// agree.
export function passesFilter(g, { filters, hotThreshold, bigCut, radius, alpha }) {
  if (!filters.size) return true;
  if (filters.has('big') && radius >= bigCut) return true;
  if (filters.has('gold') && g.sourceCount >= hotThreshold) return true;
  // "Dimmer — older signal" is exactly starAlpha falling below full, which
  // happens the day after a signal appears.
  if (filters.has('dim') && alpha < 1) return true;
  return false;
}

// Midpoint of the observed size range: "in the upper half of the size scale",
// which is what the eye judges, rather than "in the top half by rank".
//
// A field of identically sized stars has no larger ones, so return Infinity and
// let the filter match nothing rather than split an even field arbitrarily.
export function bigCutFor(radii) {
  if (!radii.length) return Infinity;
  const lo = Math.min(...radii), hi = Math.max(...radii);
  return hi - lo < 0.5 ? Infinity : lo + (hi - lo) / 2;
}

export function groupSignals(signals) {
  const byURL = new Map();
  for (const s of signals) {
    const key = s.primary_url || `signal-${s.id}`;
    let g = byURL.get(key);
    if (!g) {
      g = {
        key,
        ids: [],
        topicIds: [],
        title: s.title,
        score: s.score,
        sourceCount: s.source_count,
        ageDays: s.age_days,
        primaryURL: s.primary_url,
        reason: s.reason,
      };
      byURL.set(key, g);
    }
    g.ids.push(s.id);
    if (!g.topicIds.includes(s.topic_id)) g.topicIds.push(s.topic_id);
    // Keep the strongest reading of a story that appears under several topics.
    g.score = Math.max(g.score, s.score);
    g.sourceCount = Math.max(g.sourceCount, s.source_count);
    g.ageDays = Math.min(g.ageDays, s.age_days);
  }
  return [...byURL.values()];
}

// constellationEdges joins a topic's stars into an atlas-style figure.
//
// A minimum spanning tree over the stars, by screen distance: the shortest set of
// edges that still connects every one of them.
//
// It replaced a polyline through `pts` in array order — which is the API's score
// order, not a spatial one. On a 45-star topic that drew 44 segments zigzagging
// back and forth across its own constellation, joining stars that are adjacent in
// score and far apart on screen, and several topics' joins crossed the entire
// canvas. It was only tolerable because it was drawn at 0.4 alpha, which hid it
// rather than fixing it. §6 asks for "atlas-style links", and an atlas joins
// neighbours.
//
// Prim's, O(n²), once per rebuild rather than per frame. n is one topic's star
// count — 45 at today's top end — which is nothing beside relax(), already
// quadratic over every star on the canvas.
//
// Returns pairs of the caller's own star objects, so the drawing code reads
// current positions rather than a copy that could drift out of date.
export function constellationEdges(pts) {
  const n = pts.length;
  if (n < 2) return [];

  const inTree = new Array(n).fill(false);
  const best = new Array(n).fill(Infinity);
  const from = new Array(n).fill(-1);
  const edges = [];

  best[0] = 0;
  for (let iter = 0; iter < n; iter++) {
    // Nearest star not yet connected.
    let k = -1;
    for (let i = 0; i < n; i++) {
      if (!inTree[i] && (k === -1 || best[i] < best[k])) k = i;
    }
    if (k === -1) break;
    inTree[k] = true;
    if (from[k] !== -1) edges.push([pts[from[k]], pts[k]]);

    for (let i = 0; i < n; i++) {
      if (inTree[i]) continue;
      const d = Math.hypot(pts[i].x - pts[k].x, pts[i].y - pts[k].y);
      if (d < best[i]) { best[i] = d; from[i] = k }
    }
  }
  return edges;
}

// relax pushes overlapping stars apart.
//
// §6's first known build problem: a constellation past ~15 stars collides with
// itself. This is a few iterations of pairwise repulsion, run once per layout
// rather than per frame — the positions are static, so paying for this every
// frame would be waste.
export function relax(stars, iterations = 80) {
  if (stars.length < 2) return;

  // Break degenerate arrangements first. Pairwise repulsion pushes along the
  // line between two stars, so a set that is coincident or exactly collinear can
  // only ever spread along that one axis — 20 stars in a row would need 570px of
  // width and never escape into the space above and below. A deterministic
  // golden-angle nudge gives them a second dimension to relax into.
  const GOLDEN_ANGLE = 2.39996323;
  const seen = new Map();
  stars.forEach((s, i) => {
    const key = `${Math.round(s.x)},${Math.round(s.y)}`;
    const n = seen.get(key) || 0;
    seen.set(key, n + 1);
    if (n === 0) return;
    const angle = i * GOLDEN_ANGLE;
    const radius = (s.r + 6) * Math.sqrt(n);
    s.x += Math.cos(angle) * radius;
    s.y += Math.sin(angle) * radius;
  });

  for (let iter = 0; iter < iterations; iter++) {
    let moved = false;

    for (let i = 0; i < stars.length; i++) {
      for (let j = i + 1; j < stars.length; j++) {
        const a = stars[i], b = stars[j];
        const minDist = a.r + b.r + 10;
        let dx = b.x - a.x, dy = b.y - a.y;
        let dist = Math.hypot(dx, dy);

        if (dist >= minDist) continue;

        // Still coincident after the pre-nudge: push apart in a direction that
        // varies with the pair, rather than dividing by zero or sending every
        // such pair along the same axis.
        if (dist < 0.001) {
          const angle = (i * 7 + j) * 1.1;
          dx = Math.cos(angle); dy = Math.sin(angle); dist = 1;
        }

        const push = (minDist - dist) / 2;
        const ux = (dx / dist) * push, uy = (dy / dist) * push;
        a.x -= ux; a.y -= uy;
        b.x += ux; b.y += uy;
        moved = true;
      }
    }

    // Pull back toward the constellation centre so relaxation cannot drift a
    // star out of the topic it belongs to.
    // A busy constellation genuinely needs more room than a quiet one, so the
    // leash grows with the number of stars rather than being a flat number that
    // silently caps how many can ever be separated.
    const maxDrift = Math.max(150, Math.sqrt(stars.length) * 38);

    for (const s of stars) {
      const dx = s.x - s.home.x, dy = s.y - s.home.y;
      const dist = Math.hypot(dx, dy);
      if (dist > maxDrift) {
        s.x = s.home.x + (dx / dist) * maxDrift;
        s.y = s.home.y + (dy / dist) * maxDrift;
      }
    }

    if (!moved) break;
  }
}
