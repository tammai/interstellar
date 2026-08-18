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
//
// The sheet itself is gone -- Settings is its own page now -- but the rule that
// bit it is a property of CSS, not of that element, so the guards stay and the
// two secondary pages carry a blanket [hidden] rule apiece.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));

// Every stylesheet and every script, including the ones in subdirectories. The
// Ask screen lives in assets/chat/ and Settings in assets/settings/, and a guard
// that only looked at the top level would silently stop covering the newest code
// -- which is exactly when this trap gets re-introduced.
function collect(ext) {
  const out = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) {
        if (entry.name === 'fonts') continue;
        walk(path);
      } else if (entry.name.endsWith(ext)) {
        out.push(readFileSync(path, 'utf8'));
      }
    }
  };
  walk(join(here, 'assets'));
  return out;
}

const css = collect('.css').join('\n');
const js = collect('.js').join('\n');

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

test('the min-points field is actually hidden when hidden', () => {
  assert.match(rules, /\.field\[hidden\][^{]*\{[^}]*display\s*:\s*none/,
    '.field declares display:block, so syncKind setting .hidden cannot hide it');
});

// The settings screens moved out of the sky's sheet and into their own document,
// and the trap moved with them: settings.css gives .field, .card, .src-edit and
// the tab strip their own `display`, and settings.js hides several of them by
// class rather than by id -- which the id-based cross-reference below cannot see.
// The blanket rule covers the whole page at once, including whatever gets added
// to it next, exactly as it does on the chat screen.
test('the settings stylesheet neutralises [hidden] for every element on the page', () => {
  const settings = readFileSync(join(here, 'assets/settings/settings.css'), 'utf8')
    .replace(/\/\*[\s\S]*?\*\//g, '');
  assert.match(settings, /\[hidden\]\s*\{[^}]*display\s*:\s*none\s*!important/,
    'settings.css declares display on .field and .src-edit and settings.js hides them via .hidden; without a blanket [hidden] rule they are still laid out');
});

// The chat screen hides elements by class, not by id — sources, plan, answer,
// status, meta are all local variables in chat.js — so the id-based cross-reference
// below cannot see them. A blanket rule covers the whole page at once, including
// whatever gets added to it next, and this pins that the rule is still there.
test('the chat stylesheet neutralises [hidden] for every element on the page', () => {
  const chat = readFileSync(join(here, 'assets/chat/chat.css'), 'utf8')
    .replace(/\/\*[\s\S]*?\*\//g, '');
  assert.match(chat, /\[hidden\]\s*\{[^}]*display\s*:\s*none\s*!important/,
    'chat.css declares display on .sources and chat.js hides it via .hidden; without a blanket [hidden] rule the empty list is still laid out');
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
