// Guards one CSS trap that has already shipped twice and cost a day.
//
// An author `display` declaration beats the user-agent `[hidden] { display: none }`
// rule — origin wins over specificity, so no amount of `[hidden]` being "more
// specific" saves you. Any element the JS hides by setting `.hidden = true` is
// therefore still laid out if its own rule declares a `display`.
//
// That is how the settings sheet became an invisible, hit-testable 560px column
// at z-index 50 down the right of the viewport, eating every click on the header
// controls and the zoom buttons and swallowing wheel events so the list would not
// scroll. The same trap put an inert "Min points" field into the RSS source form.
// Both were silent: nothing renders, nothing throws, the element is just still there.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const css = readFileSync(join(here, 'assets/style.css'), 'utf8');

const js = readdirSync(join(here, 'assets'))
  .filter((f) => f.endsWith('.js'))
  .map((f) => readFileSync(join(here, 'assets', f), 'utf8'))
  .join('\n');

// Strip comments so a selector mentioned in prose is not mistaken for a rule.
const rules = css.replace(/\/\*[\s\S]*?\*\//g, '');

/** Every selector that declares its own `display`. */
function selectorsDeclaringDisplay() {
  const found = new Set();
  const re = /([^{}]+)\{([^{}]*)\}/g;
  let m;
  while ((m = re.exec(rules))) {
    const [, selector, body] = m;
    if (!/(^|[;\s])display\s*:/.test(body)) continue;
    for (const part of selector.split(',')) {
      const s = part.trim();
      // Only the simple `#id` / `.class` heads we can cross-reference; a
      // compound like `body.list-view .nav` is not something JS toggles.
      const simple = s.match(/^([#.][A-Za-z0-9_-]+)$/);
      if (simple) found.add(simple[1]);
    }
  }
  return found;
}

/** Selectors the CSS explicitly re-hides when the attribute is present. */
function selectorsGuardingHidden() {
  const found = new Set();
  const re = /([#.][A-Za-z0-9_-]+)\[hidden\]/g;
  let m;
  while ((m = re.exec(rules))) found.add(m[1]);
  return found;
}

test('the settings sheet is actually hidden when hidden', () => {
  // The showstopper, pinned directly: #sheet declares display:flex, and
  // Settings.buildDOM only ever sets `.hidden`.
  assert.match(rules, /#sheet\[hidden\][^{]*\{[^}]*display\s*:\s*none/,
    '#sheet declares its own display, so it needs an explicit [hidden] rule or it never hides');
});

test('the min-points field is actually hidden when hidden', () => {
  assert.match(rules, /\.field\[hidden\][^{]*\{[^}]*display\s*:\s*none/,
    '.field declares display:block, so syncKind setting .hidden cannot hide it');
});

test('nothing the JS hides via .hidden declares a display without a [hidden] guard', () => {
  const declaresDisplay = selectorsDeclaringDisplay();
  const guarded = selectorsGuardingHidden();

  // Element ids the JS toggles with `.hidden`, via either `el('x').hidden = …`
  // or a local bound to getElementById/createElement then hidden.
  const toggled = new Set();
  for (const m of js.matchAll(/\bel\(['"]([A-Za-z0-9_-]+)['"]\)\.hidden\s*=/g)) {
    toggled.add(`#${m[1]}`);
  }
  for (const m of js.matchAll(/\bid\s*=\s*['"]([A-Za-z0-9_-]+)['"]/g)) {
    // Only counts if that same id is later hidden somewhere in the JS.
    if (new RegExp(`\\b${m[1]}\\b[\\s\\S]{0,4000}?\\.hidden\\s*=`).test(js)) {
      toggled.add(`#${m[1]}`);
    }
  }

  const unguarded = [...toggled].filter((s) => declaresDisplay.has(s) && !guarded.has(s));
  assert.deepEqual(unguarded, [],
    `these are hidden from JS but declare their own display, which beats the UA [hidden] rule: ${unguarded.join(', ')}`);
});
