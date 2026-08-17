// Tests for the layout logic in sky.js.
//
// These encode §6 decisions that a screenshot cannot check: whether one story
// under several topics becomes one star, whether a crowded constellation stops
// overlapping, what each legend filter actually selects, and which stars are
// allowed to lift a constellation's label. Run with `make test-ui`.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  groupSignals, relax, passesFilter, bigCutFor, Sky,
  loadShow, SKY_LAYERS, constellationEdges,
} from './assets/sky.js';

const signal = (over = {}) => ({
  id: 1, topic_id: 1, title: 'A story', reason: 'because',
  score: 4, source_count: 1, age_days: 0,
  primary_url: 'https://example.test/a',
  ...over,
});

test('signals with distinct urls stay distinct stars', () => {
  const groups = groupSignals([
    signal({ id: 1, primary_url: 'https://example.test/a' }),
    signal({ id: 2, primary_url: 'https://example.test/b' }),
  ]);
  assert.equal(groups.length, 2);
});

test('one story under two topics becomes a single star listed under both', () => {
  const groups = groupSignals([
    signal({ id: 1, topic_id: 3, score: 5, source_count: 4, age_days: 0 }),
    signal({ id: 2, topic_id: 7, score: 4, source_count: 2, age_days: 1 }),
  ]);

  assert.equal(groups.length, 1, 'the same article must not be drawn twice');
  const [g] = groups;
  assert.deepEqual(g.topicIds, [3, 7]);
  assert.deepEqual(g.ids, [1, 2], 'both signal ids are kept, so the panel can name both topics');

  // The strongest reading wins: a story that is a 5 for one topic is not
  // demoted because it is a 4 for another.
  assert.equal(g.score, 5);
  assert.equal(g.sourceCount, 4);
  assert.equal(g.ageDays, 0, 'the freshest sighting decides how bright it is');
});

test('the primary topic is the first one, which the API orders by score', () => {
  const groups = groupSignals([
    signal({ id: 1, topic_id: 9, score: 5 }),
    signal({ id: 2, topic_id: 2, score: 4 }),
  ]);
  assert.equal(groups[0].topicIds[0], 9);
});

test('a signal with no url still gets its own star', () => {
  const groups = groupSignals([
    signal({ id: 1, primary_url: '' }),
    signal({ id: 2, primary_url: '' }),
  ]);
  assert.equal(groups.length, 2, 'missing urls must not collapse unrelated signals');
});

test('empty input yields no stars', () => {
  assert.deepEqual(groupSignals([]), []);
});

// --- relax: §6's first known build problem ---

const star = (x, y, r = 10) => ({ x, y, r, home: { x, y } });

function minSeparation(stars) {
  let min = Infinity;
  for (let i = 0; i < stars.length; i++) {
    for (let j = i + 1; j < stars.length; j++) {
      min = Math.min(min, Math.hypot(stars[i].x - stars[j].x, stars[i].y - stars[j].y));
    }
  }
  return min;
}

test('relax separates a crowded constellation', () => {
  // Twenty stars stacked almost exactly on top of each other: past the ~15 §6
  // warns about.
  const stars = Array.from({ length: 20 }, (_, i) => star(500 + i * 0.1, 400));
  assert.ok(minSeparation(stars) < 1, 'precondition: they start overlapping');

  relax(stars);

  // Every pair should now clear the sum of their radii.
  assert.ok(minSeparation(stars) > 15, `still overlapping: ${minSeparation(stars)}`);
});

test('relax keeps stars near their own constellation', () => {
  const stars = Array.from({ length: 30 }, () => star(500, 400));
  relax(stars);

  // The leash scales with star count, so compute the same bound the code uses.
  const maxDrift = Math.max(150, Math.sqrt(stars.length) * 38);
  for (const s of stars) {
    const drift = Math.hypot(s.x - s.home.x, s.y - s.home.y);
    assert.ok(drift <= maxDrift + 1,
      `a star drifted ${Math.round(drift)}px from its constellation; it would read as belonging to another`);
  }
});

test('relax handles exactly coincident stars without producing NaN', () => {
  // Identical coordinates would divide by zero in a naive implementation, and a
  // single NaN coordinate silently blanks the whole canvas.
  const stars = [star(100, 100), star(100, 100), star(100, 100)];
  relax(stars);

  for (const s of stars) {
    assert.ok(Number.isFinite(s.x) && Number.isFinite(s.y), 'produced a non-finite coordinate');
  }
  assert.ok(minSeparation(stars) > 0);
});

test('relax leaves a lone star alone', () => {
  const stars = [star(200, 200)];
  relax(stars);
  assert.equal(stars[0].x, 200);
  assert.equal(stars[0].y, 200);
});

// ---------- filters ----------
//
// These guard against the mistake that shipped first: a `score >= 6` threshold
// that looked reasonable and matched nothing, because the classifier returns 1-5
// and the match threshold clips it at 4.

const opts = (over = {}) => ({
  filters: new Set(), hotThreshold: 3, bigCut: 9, radius: 5, alpha: 1, ...over,
});

test('no active filter shows everything', () => {
  assert.equal(passesFilter(signal(), opts({ radius: 0, alpha: 0.1 })), true);
});

test('the big filter follows radius, not score', () => {
  const f = new Set(['big']);
  assert.equal(passesFilter(signal(), opts({ filters: f, radius: 12 })), true);
  assert.equal(passesFilter(signal(), opts({ filters: f, radius: 8 })), false);
});

test('the gold filter is the same hot test the renderer uses', () => {
  const f = new Set(['gold']);
  assert.equal(passesFilter({ sourceCount: 3 }, opts({ filters: f })), true);
  assert.equal(passesFilter({ sourceCount: 2 }, opts({ filters: f })), false);
});

test('the dim filter matches anything not at full brightness', () => {
  const f = new Set(['dim']);
  assert.equal(passesFilter(signal(), opts({ filters: f, alpha: 1 })), false);
  assert.equal(passesFilter(signal(), opts({ filters: f, alpha: 0.9 })), true);
});

// The legend now activates one filter at a time (see FILTERS in app.js), so a
// two-filter set should not reach here from the UI. The predicate still defines
// what happens if one does, and union is the safe answer: it can only ever show
// more, never blank a sky the reader did not ask to empty.
test('several filters union rather than intersect', () => {
  const f = new Set(['big', 'gold']);
  // Big but cold, and hot but small: both must survive.
  assert.equal(passesFilter({ sourceCount: 1 }, opts({ filters: f, radius: 12 })), true);
  assert.equal(passesFilter({ sourceCount: 5 }, opts({ filters: f, radius: 4 })), true);
  assert.equal(passesFilter({ sourceCount: 1 }, opts({ filters: f, radius: 4 })), false);
});

test('bigCutFor splits the middle of the observed range', () => {
  assert.equal(bigCutFor([4, 14]), 9);
  assert.equal(bigCutFor([4.5, 10.5, 13.8]), 9.15);
});

test('bigCutFor matches nothing when every star is the same size', () => {
  // Otherwise a uniform field would be arbitrarily cut in half, and "larger"
  // would point at stars indistinguishable from the rest.
  assert.equal(bigCutFor([7, 7, 7]), Infinity);
  assert.equal(bigCutFor([]), Infinity);
});

// buildConstellations replaced a per-frame recomputation inside the render
// loop. It is not pure, but it is the part of that change most able to go wrong
// silently: nothing on screen tells you the label is being lifted by the wrong
// set of stars until two topics' names overprint.

const constellationsFor = (topics, centres, shown) => {
  const self = { topics, centres: new Map(centres), shown, constellations: null };
  Sky.prototype.buildConstellations.call(self);
  return self.constellations;
};

const member = (topicIds, y, shared = false) => ({ g: { topicIds }, x: 0, y, shared });

test('an empty topic keeps its label and reads as no signal', () => {
  const [c] = constellationsFor(
    [{ id: 1, title: 'Robotics' }],
    [[1, { x: 50, y: 100 }]],
    [],
  );
  assert.equal(c.label, 'ROBOTICS');
  assert.equal(c.count, '· no signal ·');
  assert.equal(c.dim, true, 'an empty constellation is drawn dimmer, not dropped');
  assert.equal(c.minOwnY, 100, 'with no stars the label hangs off the centre');
});

test('only a topic own stars lift its label', () => {
  const shared = member([1, 2], 10, true);
  const [c] = constellationsFor(
    [{ id: 1, title: 'AI' }, { id: 2, title: 'Chips' }],
    [[1, { x: 50, y: 100 }], [2, { x: 300, y: 100 }]],
    [member([1], 40), shared],
  );

  // The shared star sits highest at y=10, but letting it raise the label would
  // drag both topics' names toward each other until they overprint (§6).
  assert.equal(c.minOwnY, 40);
  assert.equal(c.pts.length, 2, 'it is still drawn as a member of this topic');
  assert.ok(c.pts.includes(shared), 'and is still joined and counted like any other');
  assert.equal(c.count, '· 2 ·');
});

test('a star below the centre never pushes the label down', () => {
  const [c] = constellationsFor(
    [{ id: 1, title: 'AI' }],
    [[1, { x: 50, y: 100 }]],
    [member([1], 260)],
  );
  assert.equal(c.minOwnY, 100);
});

test('a topic with no centre is skipped rather than drawn at the origin', () => {
  const cs = constellationsFor([{ id: 9, title: 'Orphan' }], [], [member([9], 40)]);
  assert.deepEqual(cs, []);
});

// Culling decides what the frame costs, and it fails silently in both
// directions: too tight and stars pop in at the edge with their halos sliced,
// too loose and it stops being a saving. Neither shows up in a screenshot of a
// centred sky.

const bounds = (view, W = 1000, H = 800) =>
  Sky.prototype.viewBounds.call({ view, W, H });

const onScreen = (s, b) => Sky.prototype.onScreen.call(null, s, b);

test('viewBounds inverts the world transform', () => {
  // Compared field by field rather than deep-equal: an unpanned view divides
  // -0 by the scale, and -0 is not deep-equal to 0 even though every comparison
  // onScreen makes treats them the same.
  const same = (got, want) => {
    for (const k of ['x0', 'y0', 'x1', 'y1']) {
      assert.ok(got[k] === want[k], `${k}: got ${got[k]}, want ${want[k]}`);
    }
  };

  same(bounds({ x: 0, y: 0, s: 1 }), { x0: 0, y0: 0, x1: 1000, y1: 800 });

  // Zoomed 2x, the visible world is half as wide.
  same(bounds({ x: 0, y: 0, s: 2 }), { x0: 0, y0: 0, x1: 500, y1: 400 });

  // Panned right by 200px at 1x: the world scrolls left under the viewport.
  same(bounds({ x: 200, y: 0, s: 1 }), { x0: -200, y0: 0, x1: 800, y1: 800 });
});

test('a star in view is drawn and one far outside is not', () => {
  const b = bounds({ x: 0, y: 0, s: 1 });
  assert.equal(onScreen({ x: 500, y: 400, r: 10 }, b), true);
  assert.equal(onScreen({ x: 5000, y: 400, r: 10 }, b), false);
  assert.equal(onScreen({ x: 500, y: -900, r: 10 }, b), false);
});

test('a star just off the edge is still drawn, halo and all', () => {
  const b = bounds({ x: 0, y: 0, s: 1 });
  // Its centre is outside the viewport but its widest sprite layer reaches
  // 4.8x the radius, and the hot pulse scales that by 1.2 again. Culling on the
  // centre alone would slice the halo off at the border.
  assert.equal(onScreen({ x: -40, y: 400, r: 14 }, b), true,
    'a big star just outside must keep drawing or its halo pops at the edge');
  assert.equal(onScreen({ x: -14 * 6 - 20, y: 400, r: 14 }, b), false,
    'but once its whole reach clears the edge it costs nothing to skip');
});

test('nothing hoverable is ever culled', () => {
  // starAtWorld matches within max(r * 1.7, 14 / scale); the cull margin is
  // r * 6 + 8, so the drawn set always contains the hittable set.
  const b = bounds({ x: 0, y: 0, s: 1 });
  for (const r of [4.5, 9, 13.8]) {
    const justHittable = { x: -r * 1.7 + 0.5, y: 400, r };
    assert.equal(onScreen(justHittable, b), true, `r=${r} was hittable but not drawn`);
  }
});

// Sky layer toggles. These are view preferences rather than configuration, so
// the risks worth pinning are not "does the switch work" but the two ways a
// preference can quietly break something: a stored value reaching the renderer
// unchecked, and the motion switch defeating a §10 guarantee.

const fakeStore = (initial) => {
  let value = initial;
  return {
    getItem: () => value,
    setItem: (_k, v) => { value = v },
    read: () => value,
  };
};

test('the scenery is on before anything is stored, and the joins are not', () => {
  const show = loadShow(fakeStore(null));
  for (const k of ['dust', 'milkyway', 'grid', 'meteors', 'motion']) {
    assert.equal(show[k], true, `${k} should be on by default`);
  }
  // The one layer that defaults off. Pinned because it is a deliberate departure
  // from §6, not an oversight: the links are specified, the default is ours.
  assert.equal(show.lines, false, 'constellation joins default off');
});

test('a stored preference is read back', () => {
  const show = loadShow(fakeStore(JSON.stringify({ dust: false, meteors: false })));
  assert.equal(show.dust, false);
  assert.equal(show.meteors, false);
  assert.equal(show.grid, true, 'keys absent from the store keep their default');
});

test('unparseable or hostile storage cannot hide a layer', () => {
  // Unparseable is the same as unset. The failure this guards against is a
  // corrupted key blanking the sky with no way to tell why.
  for (const raw of ['not json', '[]', 'null', '{"dust":"no"}', '{"dust":0}']) {
    const show = loadShow(fakeStore(raw));
    assert.equal(show.dust, true, `${raw} must not turn dust off`);
  }
});

test('an unknown key in storage does not become a layer', () => {
  const show = loadShow(fakeStore(JSON.stringify({ nebula: false })));
  assert.equal(show.nebula, undefined, 'only layers something actually draws exist');
  assert.deepEqual(Object.keys(show).sort(), [...SKY_LAYERS].sort());
});

test('setShow rejects a layer name nothing draws', () => {
  const self = { show: loadShow(fakeStore(null)), buildLayers: () => {} };
  assert.equal(Sky.prototype.setShow.call(self, 'nebula', false), false);
  assert.equal(self.show.nebula, undefined);
});

test('turning the Milky Way off re-bakes the background, and nothing else does', () => {
  // The band is baked into bgLayer rather than drawn per frame, so it is the one
  // toggle that needs more than the next frame to take effect.
  let bakes = 0;
  const self = { show: loadShow(fakeStore(null)), buildLayers: () => { bakes++ } };

  Sky.prototype.setShow.call(self, 'dust', false);
  assert.equal(bakes, 0, 'a per-frame guard must not pay for a re-bake');

  Sky.prototype.setShow.call(self, 'milkyway', false);
  assert.equal(bakes, 1);
});

test('the motion switch can turn motion off but never on', () => {
  // §10: prefers-reduced-motion disables twinkle, pulse, glint and easing. A
  // settings toggle must not be able to override the system preference, so the
  // gate ORs them rather than letting the switch decide.
  const gate = Object.getOwnPropertyDescriptor(Sky.prototype, 'still').get;

  assert.equal(gate.call({ show: { motion: true } }), false, 'motion on, no system preference');
  assert.equal(gate.call({ show: { motion: false } }), true, 'the switch alone stills the sky');
});

// Idle skipping. The risk here is not that it fails to skip — that only costs
// frames — but that it skips a frame it needed to draw, which leaves a stale
// image on screen with nothing to indicate why.

const easer = (view, viewTarget, still = true) =>
  ({ view, viewTarget, still });

test('a settled view reports no movement, so the frame can be skipped', () => {
  const self = easer({ x: 10, y: 20, s: 2 }, { x: 10, y: 20, s: 2 });
  assert.equal(Sky.prototype.easeView.call(self), false);
});

test('the ease snaps once the remainder stops being visible', () => {
  // Asymptotic easing never reaches its target, so without a snap the sky would
  // repaint forever over thousandths of a pixel. The frame it snaps on still
  // counts as movement, because the view did change.
  const self = easer({ x: 10.1, y: 20, s: 2 }, { x: 10, y: 20, s: 2 });
  assert.equal(Sky.prototype.easeView.call(self), true);
  assert.deepEqual(self.view, { x: 10, y: 20, s: 2 }, 'snapped exactly onto the target');
  assert.equal(Sky.prototype.easeView.call(self), false, 'and is settled from then on');
});

test('a real pan still eases rather than snapping', () => {
  const self = easer({ x: 0, y: 0, s: 1 }, { x: 100, y: 0, s: 1 }, false);
  assert.equal(Sky.prototype.easeView.call(self), true);
  assert.equal(self.view.x, 30, 'eased at 0.3, not jumped to the target');
});

test('reduced motion snaps the view instead of easing it', () => {
  // §10: prefers-reduced-motion disables parallax easing.
  const self = easer({ x: 0, y: 0, s: 1 }, { x: 100, y: 0, s: 1 }, true);
  Sky.prototype.easeView.call(self);
  assert.equal(self.view.x, 100);
});

test('a scale change too small to move a star does not count as movement', () => {
  const self = easer({ x: 0, y: 0, s: 1 }, { x: 0, y: 0, s: 1.0001 });
  assert.equal(Sky.prototype.easeView.call(self), true, 'snaps once');
  assert.equal(self.view.s, 1.0001);
  assert.equal(Sky.prototype.easeView.call(self), false);
});

test('changing a layer marks the sky for a repaint', () => {
  const self = {
    show: loadShow(fakeStore(null)),
    buildLayers: () => {},
    dirty: false,
  };
  Sky.prototype.setShow.call(self, 'grid', false);
  assert.equal(self.dirty, true,
    'a layer switched off must repaint, or the old layer stays on screen');
});

// The constellation join. What this protects is legibility rather than
// correctness: the old polyline "worked" in the sense that it drew something.

const at = (x, y) => ({ x, y });
const lengthOf = (edges) =>
  edges.reduce((sum, [a, b]) => sum + Math.hypot(a.x - b.x, a.y - b.y), 0);

test('a topic with fewer than two stars has nothing to join', () => {
  assert.deepEqual(constellationEdges([]), []);
  assert.deepEqual(constellationEdges([at(0, 0)]), []);
});

test('every star ends up connected, with no cycle', () => {
  const pts = [at(0, 0), at(10, 0), at(20, 0), at(30, 0), at(0, 40)];
  const edges = constellationEdges(pts);
  // A spanning tree over n nodes has exactly n-1 edges.
  assert.equal(edges.length, pts.length - 1);

  const seen = new Set();
  for (const [a, b] of edges) { seen.add(a); seen.add(b) }
  assert.equal(seen.size, pts.length, 'every star is touched by an edge');
});

test('the join follows neighbours, not the order it was handed', () => {
  // Four stars in a line, deliberately supplied in a scrambled order — which is
  // what score order does. The old polyline joined them 0-2-3-1 and doubled back
  // across itself; the tree joins each to its neighbour.
  const a = at(0, 0), b = at(10, 0), c = at(20, 0), d = at(30, 0);
  const edges = constellationEdges([a, c, d, b]);

  assert.equal(lengthOf(edges), 30, 'total edge length is the span, walked once');
  for (const [p, q] of edges) {
    assert.equal(Math.hypot(p.x - q.x, p.y - q.y), 10, 'every edge joins adjacent stars');
  }
});

test('the tree is shorter than the order it replaced', () => {
  // The property that matters, stated directly: for any arrangement, the tree
  // cannot be longer than the polyline through the same stars in array order.
  const pts = [at(0, 0), at(90, 12), at(15, 30), at(80, 70), at(5, 65), at(70, 20)];

  let polyline = 0;
  for (let i = 1; i < pts.length; i++) {
    polyline += Math.hypot(pts[i].x - pts[i - 1].x, pts[i].y - pts[i - 1].y);
  }
  assert.ok(lengthOf(constellationEdges(pts)) < polyline,
    'a spanning tree over these points should beat the arbitrary order');
});

test('stars stacked on one spot do not break the join', () => {
  const edges = constellationEdges([at(5, 5), at(5, 5), at(5, 5)]);
  assert.equal(edges.length, 2, 'zero-length edges still connect every star');
});

test('edges reference the caller\'s stars, so they track relayout', () => {
  const a = at(0, 0), b = at(10, 0);
  const [[p, q]] = constellationEdges([a, b]);
  assert.ok((p === a && q === b) || (p === b && q === a),
    'the drawing code must read live positions, not a snapshot');
});
