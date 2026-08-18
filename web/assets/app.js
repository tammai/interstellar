// Entry point: loads the sky, keeps status in view, and wires the two views
// together.

import { Sky, SKY_LAYERS } from './sky.js';
import { Panel } from './panel.js';
import { renderList } from './list.js';

const el = (id) => document.getElementById(id);

const state = {
  topics: [],
  groups: [],
  days: 7,
  hotThreshold: 3,
  view: 'sky',
  running: false,
  filters: new Set(),
};

const canvas = el('sky');
const tip = el('tip');
const panel = new Panel(el('panel'));

const sky = new Sky(canvas, {
  onSelect: (group) => panel.open(group),
  onHover: (group, x, y) => showTip(group, x, y),
});

// Publish the header's real height so the detail panel can start below it
// rather than on top of it. Measured rather than hardcoded: the header wraps to
// a second row under 640px, and a stale offset there would put the panel back
// over the controls it exists to stay clear of.
function syncHeaderHeight() {
  const header = document.querySelector('header');
  if (!header) return;
  document.documentElement.style.setProperty('--header-h', `${Math.ceil(header.offsetHeight)}px`);
}
syncHeaderHeight();
addEventListener('resize', syncHeaderHeight);

// ---------- data ----------

async function loadSky() {
  try {
    const res = await fetch('/api/sky');
    if (!res.ok) throw new Error(`sky: ${res.status}`);
    const data = await res.json();

    state.topics = data.topics || [];
    state.hotThreshold = data.hot_threshold || 3;
    panel.hotThreshold = state.hotThreshold;

    sky.setData(data);
    state.groups = sky.groups;

    state.observationDate = data.observation_date;
    // The window the sky covers, for the count's tooltip. §5.4's 7 days is the
    // default, not a constant -- sky_days is a setting.
    state.days = data.days || 7;
    updateCount();
    renderCurrentView();
    updateEmptyState();
  } catch (err) {
    console.error(err);
  }
}

async function loadStatus() {
  try {
    const res = await fetch('/api/status');
    if (!res.ok) return;
    const status = await res.json();

    const wasRunning = state.running;
    state.running = status.run?.stage && status.run.stage !== 'idle';

    setRefreshBusy(state.running, status.run?.stage);
    renderRunState(status);

    // A run that just finished has new stars to show.
    if (wasRunning && !state.running) loadSky();

    renderBanner(status);
    el('btnSettings').querySelector('.nudge')?.remove();
    if (status.setup_hint) {
      const dot = document.createElement('span');
      dot.className = 'nudge';
      el('btnSettings').append(dot);
    }

    // Poll quickly while working, slowly when idle. A run is the only time
    // anything changes on its own.
    scheduleStatus(state.running ? 1500 : 20000);
  } catch {
    scheduleStatus(20000);
  }
}

let statusTimer = null;
function scheduleStatus(ms) {
  clearTimeout(statusTimer);
  statusTimer = setTimeout(loadStatus, ms);
}

async function refresh() {
  if (state.running) return;
  // Disabled before the request goes out, not when the poll comes back. There is a
  // second or two between the click and the first status showing `running`, and a
  // second click inside it is the one that got through -- POST /api/refresh answers
  // 409, so nothing broke, but the button not reacting to a click is its own bug.
  setRefreshBusy(true);
  try {
    const res = await fetch('/api/refresh', { method: 'POST' });
    if (res.status === 409) return; // already going; the poller will show it
    state.running = true;
    scheduleStatus(600);
  } catch (err) {
    console.error(err);
    setRefreshBusy(false); // nothing was started, so the button must come back
  }
}

// setRefreshBusy is the one place the refresh button's state is set, so "spinning"
// and "unclickable" cannot drift apart.
function setRefreshBusy(busy, stage) {
  const btn = el('btnRefresh');
  btn.classList.toggle('busy', busy);
  btn.disabled = busy;
  btn.title = busy ? `Running${stage ? `: ${stage}` : ''}` : 'Refresh now';
  btn.setAttribute('aria-label', busy ? `Running${stage ? `: ${stage}` : ''}` : 'Refresh now');
}

// ---------- chrome ----------

// renderRunState says in words what the spinner only implies: which stage is running,
// how much of the day's call budget is gone, and whether the instance can run at all.
//
// The same four facts the Settings header carries, deliberately worded the same, and
// deliberately NOT shared code -- app.js imports nothing from settings.js, which is
// what keeps the sky's six assets six.
//
// A failing role is named rather than counted, and that is what lets Ask appear here
// at all: the rule that keeps chat off the star map's BANNER exists so a dead Ask
// screen cannot dress itself up as a broken morning run. "Ask failing" cannot be
// mistaken for one.
function renderRunState(status) {
  const box = el('runState');
  if (!box) return;
  box.replaceChildren();

  // Short on screen, whole sentence on hover. This line shares a header row with the
  // wordmark, the date, the view toggle and three buttons, so every word has to earn
  // its width -- and the words that got cut ("Running", "LLM", "to run") are the ones
  // the spinner beside it, the number itself and the banner already say.
  const add = (text, title, cls) => {
    const span = document.createElement('span');
    if (cls) span.className = cls;
    span.textContent = text;
    span.title = title;
    box.append(span);
  };

  if (state.running) add(`${status.run.stage}…`, `Run in progress: ${status.run.stage}`, 'rs-run');
  if (status.calls_today != null) {
    add(`${status.calls_today} / ${status.call_cap_day} calls`,
      'LLM calls today, against the daily cap');
  }
  if (status.ready === false) {
    add('not ready', 'not ready to run — a role has no model or no key', 'rs-warn');
  }

  const failing = (status.failures || []).map((f) => (f.role === 'chat' ? 'Ask' : f.role));
  if (failing.length) {
    add(`${failing.join(', ')} failing`,
      'every call for this role failed on its most recent day — see the Model screen', 'rs-warn');
  }
}

function renderBanner(status) {
  const banner = el('banner');
  const text = el('bannerText');
  const action = el('bannerAction');

  const message = status.banner || status.setup_hint;
  if (!message) {
    banner.classList.remove('on');
    return;
  }

  banner.classList.add('on');
  banner.classList.toggle('setup', !status.banner);
  text.textContent = message;

  // A banner that says what is wrong but not where to fix it is only half a
  // message. Settings is its own document now, so this navigates rather than
  // opening an overlay -- and it navigates to the screen that can fix the thing
  // the banner is complaining about, which the fragment addresses directly.
  action.hidden = false;
  action.textContent = 'Open settings';
  action.onclick = () => { location.href = status.banner ? 'settings/#model' : 'settings/' };
}

// The header count reports what is on screen. Leaving it at the unfiltered total
// while a filter hides most of them makes the number look wrong.
//
// It counts STORIES, and says so, because it cannot honestly say "signals". A signal
// is one story for ONE topic (spec §4), so a story matching two topics arrives from
// the API as two rows -- with two titles and two reasons, written separately by the
// synthesizer -- and groupSignals merges them by primary_url into the one shared star
// §6 asks for. Measured on a real instance: 110 rows, 99 stars, every one of the 11
// collapses a two-topic story. Labelling 99 of those "signals" reads as a cap or a
// lost run, which is exactly how it was reported.
//
// "Stories" is also the only word that survives the view switch: the list renders a
// shared story under BOTH of its topic groups, so the row count there is the 110 and
// the star count is the 99, while the number of stories is 99 in both places. The
// full arithmetic is in the tooltip rather than the header, which has no room for it.
function updateCount() {
  const total = state.groups.length;
  const visible = state.groups.filter((g) => sky.passes(g)).length;
  const shown = state.filters.size ? `${visible} of ${total}` : String(total);
  const n = state.filters.size ? visible : total;

  const box = el('obsDate');
  box.textContent =
    `Observation · ${formatDate(state.observationDate)} · ${shown} ${n === 1 ? 'story' : 'stories'}`;

  const rows = state.groups.reduce((sum, g) => sum + g.ids.length, 0);
  box.title = rows === total
    ? `${rows} signal${rows === 1 ? '' : 's'} over ${state.days} days`
    : `${rows} signals over ${state.days} days — ${rows - total} of these stories ` +
      `matched more than one topic, and each shares one star between them`;
}

function updateEmptyState() {
  const empty = el('empty');
  const visible = state.groups.filter((g) => sky.passes(g)).length;
  const hasSignals = visible > 0;
  empty.classList.toggle('on', !hasSignals && state.view === 'sky');
  if (hasSignals) return;

  // Distinguish "nothing arrived" from "your filter hides everything that did".
  // Telling someone the sky is empty when they filtered it themselves sends them
  // looking for a bug in the pipeline.
  if (state.groups.length && state.filters.size) {
    el('emptyTitle').textContent = 'Nothing matches this filter';
    el('emptyBody').textContent =
      `${state.groups.length} signal${state.groups.length === 1 ? '' : 's'} today, none of them matching. Switch a filter off to see them.`;
    return;
  }

  const unset = state.topics.filter((t) => t.needs_setup).length;
  if (!state.topics.length) {
    el('emptyTitle').textContent = 'No topics yet';
    el('emptyBody').textContent =
      'Add a topic in Settings and Interstellar will start watching for it.';
  } else if (unset === state.topics.length) {
    el('emptyTitle').textContent = 'Tell it what you care about';
    el('emptyBody').textContent =
      'Your topics need definitions before anything can be matched against them. Open Settings to write them.';
  } else {
    el('emptyTitle').textContent = 'Nothing yet today';
    el('emptyBody').textContent =
      'No signals in the last few days. Refresh to fetch now, or wait for the morning run.';
  }
}

function showTip(group, x, y) {
  if (!group) { tip.classList.remove('on'); return }

  const topicNames = group.topicIds
    .map((id) => state.topics.find((t) => t.id === id)?.title)
    .filter(Boolean);

  tip.querySelector('.t-topic').textContent = topicNames.join(' · ');
  tip.querySelector('.t-title').textContent = group.title;
  tip.querySelector('.t-meta').textContent =
    `score ${group.score.toFixed(1)} · ${group.sourceCount} source${group.sourceCount === 1 ? '' : 's'} · ` +
    (group.ageDays === 0 ? 'today' : `${group.ageDays}d ago`);

  tip.style.left = Math.min(x + 16, innerWidth - 320) + 'px';
  tip.style.top = Math.min(y + 14, innerHeight - 110) + 'px';
  tip.classList.add('on');
}

function renderCurrentView() {
  if (state.view === 'list') {
    renderList(el('listContent'), {
      topics: state.topics,
      // The filter is a property of the data being looked at, not of the star
      // map, so both views apply it. sky.passes is the single definition.
      groups: state.groups.filter((g) => sky.passes(g)),
      hotThreshold: state.hotThreshold,
      onSelect: (g) => panel.open(g),
    });
  }
}

function setView(view) {
  state.view = view;
  const isList = view === 'list';

  el('list').classList.toggle('on', isList);
  // Drives hiding the sky-only chrome (legend, zoom controls) in list view.
  document.body.classList.toggle('list-view', isList);
  // The pan and hover listeners are on window, not the canvas, so the list
  // overlay does not shield them. Without this, moving the mouse across a list
  // row raises the tooltip for whatever star sits behind it.
  sky.setInteractive(!isList);
  // And stop drawing entirely: the list is opaque, so every frame rendered behind
  // it is pure competition for the thread the list is trying to scroll on.
  sky.setVisible(!isList);
  el('btnSky').classList.toggle('active', !isList);
  el('btnList').classList.toggle('active', isList);
  el('btnSky').setAttribute('aria-selected', String(!isList));
  el('btnList').setAttribute('aria-selected', String(isList));

  renderCurrentView();
  updateEmptyState();
}

function formatDate(iso) {
  if (!iso) return '';
  const d = new Date(iso + 'T00:00:00');
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
}

// ---------- wiring ----------

el('btnSky').onclick = () => setView('sky');
el('btnList').onclick = () => setView('list');
el('btnRefresh').onclick = () => refresh();
// btnSettings is a plain link to settings/ — no handler, and nothing on this page
// knows what a settings screen looks like any more.

// Filters. Empty set means unfiltered, which is why there is no "All" button:
// switching the active filter off is the same statement, and one fewer control.
//
// One at a time, radio-style. They used to be independent toggles that unioned,
// on the reasoning that "the big ones and the hot ones" is a real question and
// that intersecting them would usually match nothing. Both halves were true and
// the result was still wrong: switching on "larger" while "gold" was active
// added every big-but-cold star, so gold looked like it had stopped working.
// A filter you cannot tell is doing anything is worse than one you cannot
// combine, and the intersect problem never arises if only one is ever on.
const FILTERS = [['fBig', 'big'], ['fGold', 'gold'], ['fDim', 'dim']];
for (const [id, name] of FILTERS) {
  el(id).onclick = () => {
    const wasOn = state.filters.has(name);
    state.filters.clear();
    if (!wasOn) state.filters.add(name);
    for (const [otherId, otherName] of FILTERS) {
      el(otherId).setAttribute('aria-pressed', String(state.filters.has(otherName)));
    }
    sky.setFilters(state.filters);
    updateCount();
    renderCurrentView();
    updateEmptyState();
  };
}

// Sky layers. Deliberately the same kind of control as the legend: each row names
// one thing the canvas draws and switches it. These are view preferences the Sky
// owns and persists itself — nothing here reaches a run, so there is no endpoint
// behind them and no "applies on the next run" to explain.
//
// Rows are built from the Sky's own layer list rather than a copy, so a layer
// cannot be added to the renderer and silently missing from the panel.
const LAYER_LABELS = {
  dust: 'Parallax dust',
  milkyway: 'Milky Way band',
  grid: 'Celestial grid',
  lines: 'Constellation lines',
  meteors: 'Meteors',
  motion: 'Twinkle & motion',
};

const layersEl = el('layers');
const layersList = el('layersList');

for (const key of SKY_LAYERS) {
  const btn = document.createElement('button');
  btn.className = 'row';
  btn.type = 'button';
  btn.setAttribute('aria-pressed', String(sky.show[key]));
  btn.setAttribute('aria-label', `${LAYER_LABELS[key]} — show or hide`);

  const dot = document.createElement('span');
  dot.className = 'ly-dot';
  dot.setAttribute('aria-hidden', 'true');

  const txt = document.createElement('span');
  txt.className = 'txt';
  txt.textContent = LAYER_LABELS[key];

  btn.append(dot, txt);
  btn.onclick = () => {
    const next = btn.getAttribute('aria-pressed') !== 'true';
    if (sky.setShow(key, next)) btn.setAttribute('aria-pressed', String(next));
  };
  layersList.append(btn);
}

// Frame rate, for information. It reads the count the sky already keeps rather
// than starting a second rAF loop, which would measure its own scheduling as
// much as the sky's — and `drawn` beside it is the culling diagnostic: when the
// two diverge under zoom, the cull is doing its job.
const fpsRow = document.createElement('div');
fpsRow.className = 'row note';
fpsRow.id = 'layersFps';
layersList.append(fpsRow);

// "idle" rather than a bare 0, which reads as broken: with motion off and the
// view settled the sky is deliberately not repainting, because every frame would
// be identical to the one already on screen.
const paintFps = () => {
  fpsRow.textContent = !sky.running ? 'not drawing'
    : sky.idle ? '0 fps · idle'
    : `${sky.fps || '—'} fps · ${sky.drawn} drawn`;
};

// Polled only while the panel is open. A collapsed panel costs nothing, which
// matters for a readout whose whole purpose is not to affect what it measures.
let fpsTimer = null;
el('layersToggle').onclick = () => {
  const open = layersEl.classList.toggle('open');
  el('layersToggle').setAttribute('aria-expanded', String(open));
  clearInterval(fpsTimer);
  fpsTimer = null;
  if (open) {
    paintFps();
    fpsTimer = setInterval(paintFps, 500);
  }
};

el('zin').onclick = () => sky.zoomAt(innerWidth / 2, innerHeight / 2, 1.7);
el('zout').onclick = () => sky.zoomAt(innerWidth / 2, innerHeight / 2, 1 / 1.7);
el('zreset').onclick = () => sky.reset();

// A deliberate handle, not a leak. This is a single-user tool the owner runs on
// their own machine; being able to open the console and ask the sky what it
// thinks is showing is worth more than the tidiness of hiding it. It is also what
// the browser-driven checks drive.
window.interstellar = { sky, state, panel, reload: loadSky };

loadSky();
loadStatus();
