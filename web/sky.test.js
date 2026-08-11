// Tests for the two pure functions in sky.js.
//
// Both encode §6 decisions that a screenshot cannot check: whether one story
// under several topics becomes one star, and whether a crowded constellation
// stops overlapping. Run with `make test-ui`.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { groupSignals, relax } from './assets/sky.js';

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
