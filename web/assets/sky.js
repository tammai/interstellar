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
    this.ctx = canvas.getContext('2d');
    this.onSelect = onSelect;
    this.onHover = onHover;

    this.topics = [];
    this.groups = [];       // one entry per rendered star
    this.stars = [];
    this.dustFar = [];
    this.dustNear = [];
    this.hoverStar = null;
    this.hotThreshold = 3;

    this.view = { x: 0, y: 0, s: 1 };        // screen = world * s + offset
    this.viewTarget = { x: 0, y: 0, s: 1 };
    this.sprites = new Map();
    this.useSprites = false;                 // decided by measurement, see maybeEnableSprites
    this.frameSamples = [];

    this.bindEvents();
    addEventListener('resize', () => this.layout());

    // Size the canvas before the first frame. The render loop starts now but
    // data arrives over the network, so without this the first frame can run
    // with no dimensions -- and a canvas call with a NaN argument throws inside
    // the rAF callback, which skips the re-schedule at the end of frame() and
    // kills the loop permanently. It is a race, so it fails intermittently.
    this.layout();
    requestAnimationFrame((t) => this.frame(t));
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
      };
    }).filter(Boolean);

    relax(this.stars);

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

    this.sprites.clear();
  }

  // ---------- rendering ----------

  tapered(x1, y1, x2, y2, rgb, alpha, w) {
    const ctx = this.ctx;
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

  drawStar(s, now) {
    const ctx = this.ctx;
    const hot = this.isHot(s.g);
    const c = hot ? HOT : s.temp;

    // Organic twinkle from two incommensurate sines, so it never visibly loops.
    const tw = REDUCED_MOTION ? 1
      : 0.9 + 0.06 * Math.sin(now / 860 + s.phase) + 0.04 * Math.sin(now / 347 + s.ph2);
    const pulse = hot && !REDUCED_MOTION ? 1 + 0.2 * Math.sin(now / 520 + s.phase) : 1;
    const hover = this.hoverStar === s ? 1.2 : 1;
    const R = s.r * tw * hover;
    const a = s.a;

    // 1. chromatic outer halo
    const haloR = R * (hot ? 4.6 : 3.2) * pulse;
    let g = ctx.createRadialGradient(s.x, s.y, 0, s.x, s.y, haloR);
    g.addColorStop(0, `rgba(${c.mid},${0.5 * a})`);
    g.addColorStop(0.45, `rgba(${c.halo},${0.16 * a})`);
    g.addColorStop(1, `rgba(${c.halo},0)`);
    ctx.fillStyle = g;
    ctx.beginPath(); ctx.arc(s.x, s.y, haloR, 0, 7); ctx.fill();

    // 2. diffraction spikes — the vertical pair longer, like refractor optics
    if (s.r > 5.5) {
      const L = R * (hot ? 3.4 : 2.7) * pulse;
      const rgb = hot ? HOT.mid : c.mid;
      const sa = (hot ? 0.6 : 0.45) * a;
      this.tapered(s.x, s.y, s.x, s.y - L * 1.15, rgb, sa, 1.1);
      this.tapered(s.x, s.y, s.x, s.y + L * 1.15, rgb, sa, 1.1);
      this.tapered(s.x, s.y, s.x - L, s.y, rgb, sa * 0.85, 1);
      this.tapered(s.x, s.y, s.x + L, s.y, rgb, sa * 0.85, 1);

      // Secondary diagonals on the largest stars; on hot ones they rotate.
      if (s.r > 9) {
        const rot = hot && !REDUCED_MOTION ? now / 9000 : 0;
        const d = L * 0.5;
        const ca = Math.cos(Math.PI / 4 + rot + s.spin);
        const sb = Math.sin(Math.PI / 4 + rot + s.spin);
        this.tapered(s.x, s.y, s.x + ca * d, s.y + sb * d, rgb, sa * 0.5, 0.8);
        this.tapered(s.x, s.y, s.x - ca * d, s.y - sb * d, rgb, sa * 0.5, 0.8);
        this.tapered(s.x, s.y, s.x - sb * d, s.y + ca * d, rgb, sa * 0.5, 0.8);
        this.tapered(s.x, s.y, s.x + sb * d, s.y - ca * d, rgb, sa * 0.5, 0.8);
      }
    }

    // 3. mid glow
    g = ctx.createRadialGradient(s.x, s.y, 0, s.x, s.y, R * 1.15);
    g.addColorStop(0, `rgba(${c.core},${0.95 * a})`);
    g.addColorStop(0.6, `rgba(${c.mid},${0.55 * a})`);
    g.addColorStop(1, `rgba(${c.mid},0)`);
    ctx.fillStyle = g;
    ctx.beginPath(); ctx.arc(s.x, s.y, R * 1.15, 0, 7); ctx.fill();

    // 4. hard bright core — the pinprick that makes it read as a star, not a blob
    ctx.fillStyle = `rgba(${c.core},${a})`;
    ctx.beginPath(); ctx.arc(s.x, s.y, Math.max(R * 0.28, 1.4), 0, 7); ctx.fill();
    ctx.fillStyle = `rgba(255,255,255,${0.9 * a})`;
    ctx.beginPath(); ctx.arc(s.x - R * 0.07, s.y - R * 0.07, Math.max(R * 0.12, 0.7), 0, 7); ctx.fill();
  }

  // A star pre-rendered once into an offscreen canvas, then blitted.
  //
  // Only used when measurement says the direct path is too slow (§6: "measure;
  // if needed, pre-render"). Twinkle and pulse are dropped in this mode, which is
  // the trade: past ~100 stars, a steady frame rate matters more than shimmer.
  spriteFor(s) {
    const hot = this.isHot(s.g);
    const key = `${Math.round(s.r * 2)}|${TEMPS.indexOf(s.temp)}|${hot ? 1 : 0}|${Math.round(s.a * 10)}`;
    let sprite = this.sprites.get(key);
    if (sprite) return sprite;

    const pad = Math.ceil(s.r * (hot ? 4.6 : 3.2) * 1.3) + 4;
    const size = pad * 2;
    const off = document.createElement('canvas');
    off.width = off.height = size * this.dpr;
    const octx = off.getContext('2d');
    octx.scale(this.dpr, this.dpr);

    const saved = { ctx: this.ctx, hoverStar: this.hoverStar };
    this.ctx = octx;
    this.hoverStar = null;
    this.drawStar({ ...s, x: pad, y: pad }, 0);
    this.ctx = saved.ctx;
    this.hoverStar = saved.hoverStar;

    sprite = { canvas: off, pad, size };
    this.sprites.set(key, sprite);
    return sprite;
  }

  drawConstellations() {
    const ctx = this.ctx;
    const fs = 11.5 / this.view.s;

    for (const t of this.topics) {
      const pts = this.stars.filter((s) => s.g.topicIds.includes(t.id));
      const centre = this.centres.get(t.id);
      if (!centre) continue;

      if (pts.length > 1) {
        ctx.strokeStyle = 'rgba(56,78,140,.4)';
        ctx.lineWidth = 1 / this.view.s;
        ctx.setLineDash([1 / this.view.s, 5 / this.view.s]);
        ctx.beginPath();
        pts.forEach((p, i) => (i ? ctx.lineTo(p.x, p.y) : ctx.moveTo(p.x, p.y)));
        ctx.stroke();
        ctx.setLineDash([]);
      }

      // A shared star gets its own dashed tie to each constellation it belongs
      // to, so "between constellations" reads as membership, not as drift.
      for (const p of pts) {
        if (!p.shared) continue;
        ctx.strokeStyle = 'rgba(56,78,140,.28)';
        ctx.lineWidth = 1 / this.view.s;
        ctx.setLineDash([1 / this.view.s, 4 / this.view.s]);
        ctx.beginPath();
        ctx.moveTo(p.x, p.y);
        ctx.lineTo(centre.x, centre.y);
        ctx.stroke();
        ctx.setLineDash([]);
      }

      // Anchor the label to the topic's own centre, and let only its exclusive
      // stars lift it. A shared star sits between constellations, so letting it
      // pull the label would drag two topics' labels toward each other until
      // they overprint — and would move a constellation's name away from where
      // the reader learned it (§6, spatial memory).
      const own = pts.filter((p) => !p.shared);
      const labelX = centre.x;
      const labelY = (own.length ? Math.min(centre.y, ...own.map((p) => p.y)) : centre.y)
        - 26 / this.view.s;

      ctx.font = `${fs}px 'Noto Sans', system-ui, sans-serif`;
      ctx.textAlign = 'center';
      ctx.fillStyle = pts.length ? 'rgba(158,195,255,.8)' : 'rgba(158,195,255,.35)';
      ctx.fillText(t.title.toUpperCase(), labelX, labelY);
      ctx.fillStyle = 'rgba(74,86,124,.8)';
      ctx.fillText(pts.length ? `· ${pts.length} ·` : '· no signal ·', labelX, labelY + 14 / this.view.s);
    }
  }

  drawDust(list, parallax, now) {
    const ctx = this.ctx;
    ctx.save();
    ctx.setTransform(this.dpr, 0, 0, this.dpr, this.view.x * parallax * this.dpr, this.view.y * parallax * this.dpr);
    const sc = 1 + (this.view.s - 1) * parallax;
    ctx.scale(sc, sc);
    for (const d of list) {
      const tw = REDUCED_MOTION ? 1 : 0.55 + 0.45 * Math.sin((now / 1500) * d.sp + d.ph);
      ctx.fillStyle = `rgba(200,215,245,${d.a * tw})`;
      ctx.beginPath(); ctx.arc(d.x, d.y, d.r, 0, 7); ctx.fill();
    }
    ctx.restore();
  }

  frame(now) {
    // Always re-schedule first. Anything that throws below would otherwise stop
    // the loop for good, turning a transient glitch into a permanently frozen
    // sky — the worst possible failure for the product's front door.
    requestAnimationFrame((t) => this.frame(t));

    if (!this.dpr || !this.W || !this.H) return;

    const started = performance.now();
    const ctx = this.ctx;

    // Ease toward the target. Reduced motion snaps instead.
    const e = REDUCED_MOTION ? 1 : 0.18;
    this.view.x += (this.viewTarget.x - this.view.x) * e;
    this.view.y += (this.viewTarget.y - this.view.y) * e;
    this.view.s += (this.viewTarget.s - this.view.s) * e;

    ctx.setTransform(this.dpr, 0, 0, this.dpr, 0, 0);
    ctx.clearRect(0, 0, this.W, this.H);

    const bg = ctx.createLinearGradient(0, 0, this.W, this.H);
    bg.addColorStop(0, '#060A18');
    bg.addColorStop(0.55, '#0A1026');
    bg.addColorStop(1, '#0B0F24');
    ctx.fillStyle = bg;
    ctx.fillRect(0, 0, this.W, this.H);

    const neb = ctx.createRadialGradient(this.W * 0.44, this.H * 0.28, 0,
      this.W * 0.44, this.H * 0.28, Math.max(this.W, this.H) * 0.6);
    neb.addColorStop(0, 'rgba(38,53,94,.30)');
    neb.addColorStop(1, 'rgba(38,53,94,0)');
    ctx.fillStyle = neb;
    ctx.fillRect(0, 0, this.W, this.H);

    this.drawDust(this.dustFar, 0.25, now);
    this.drawDust(this.dustNear, 0.55, now);

    // World layer.
    ctx.setTransform(this.dpr, 0, 0, this.dpr, this.view.x * this.dpr, this.view.y * this.dpr);
    ctx.scale(this.view.s, this.view.s);

    // Celestial graticule.
    ctx.strokeStyle = 'rgba(20,30,63,.55)';
    ctx.lineWidth = 1 / this.view.s;
    for (let i = 1; i <= 4; i++) {
      ctx.beginPath();
      ctx.arc(this.W * 0.5, this.H * 2.1, this.H * 1.55 + i * 90, Math.PI * 1.28, Math.PI * 1.72);
      ctx.stroke();
    }
    ctx.strokeStyle = 'rgba(20,30,63,.30)';
    for (let i = 1; i <= 5; i++) {
      const x = (this.W / 6) * i;
      ctx.beginPath(); ctx.moveTo(x, 70); ctx.lineTo(x, this.H - 60); ctx.stroke();
    }

    this.drawConstellations();

    if (this.useSprites) {
      for (const s of this.stars) {
        const sprite = this.spriteFor(s);
        const scale = this.hoverStar === s ? 1.2 : 1;
        const w = sprite.size * scale;
        ctx.drawImage(sprite.canvas, s.x - (sprite.pad * scale), s.y - (sprite.pad * scale), w, w);
      }
    } else {
      for (const s of this.stars) this.drawStar(s, now);
    }

    this.maybeEnableSprites(performance.now() - started);
  }

  // §6 says measure before optimizing. Sample real frame cost and switch to
  // pre-rendered sprites only if the direct path is actually too slow on this
  // machine, rather than guessing from a star count.
  maybeEnableSprites(ms) {
    if (this.useSprites || this.stars.length < 60) return;
    this.frameSamples.push(ms);
    if (this.frameSamples.length < 30) return;

    const sorted = [...this.frameSamples].sort((a, b) => a - b);
    const median = sorted[Math.floor(sorted.length / 2)];
    this.frameSamples = [];
    if (median > 8) {
      this.useSprites = true;
      console.info(`interstellar: ${Math.round(median)}ms median frame with ${this.stars.length} stars — switching to sprites`);
    }
  }

  // ---------- interaction ----------

  screenToWorld(mx, my) {
    return { x: (mx - this.view.x) / this.view.s, y: (my - this.view.y) / this.view.s };
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
    for (const s of this.stars) {
      const d = Math.hypot(wx - s.x, wy - s.y);
      if (d < Math.max(s.r * 1.7, 14 / this.view.s) && d < bd) { best = s; bd = d }
    }
    return best;
  }

  bindEvents() {
    const canvas = this.canvas;

    canvas.addEventListener('wheel', (e) => {
      e.preventDefault();
      this.zoomAt(e.clientX, e.clientY, Math.exp(-e.deltaY * 0.0012));
    }, { passive: false });

    let drag = null, moved = false;

    canvas.addEventListener('mousedown', (e) => {
      drag = { mx: e.clientX, my: e.clientY, vx: this.viewTarget.x, vy: this.viewTarget.y };
      moved = false;
      canvas.classList.add('grabbing');
    });

    addEventListener('mousemove', (e) => {
      if (drag) {
        const dx = e.clientX - drag.mx, dy = e.clientY - drag.my;
        if (Math.hypot(dx, dy) > 4) moved = true;
        this.viewTarget.x = drag.vx + dx;
        this.viewTarget.y = drag.vy + dy;
        this.view.x = this.viewTarget.x; // no easing mid-drag
        this.view.y = this.viewTarget.y;
        return;
      }
      const w = this.screenToWorld(e.clientX, e.clientY);
      const s = this.starAtWorld(w.x, w.y);
      this.hoverStar = s;
      canvas.classList.toggle('pointing', !!s);
      this.onHover(s ? s.g : null, e.clientX, e.clientY);
    });

    addEventListener('mouseup', (e) => {
      if (drag && !moved) {
        const w = this.screenToWorld(e.clientX, e.clientY);
        const s = this.starAtWorld(w.x, w.y);
        if (s) this.onSelect(s.g);
      }
      drag = null;
      canvas.classList.remove('grabbing');
    });

    canvas.addEventListener('mouseleave', () => {
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
