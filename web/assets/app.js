// Entry point: loads the sky, keeps status in view, and wires the two views
// together.

import { Sky, SKY_LAYERS } from './sky.js';
import { Panel } from './panel.js';
import { renderList } from './list.js';
import { Settings } from './settings.js';

const el = (id) => document.getElementById(id);

const state = {
  topics: [],
  groups: [],
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

const settings = new Settings({
  onChanged: () => { loadSky(); loadStatus(); },
  onRefreshRequested: () => refresh(),
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

    el('btnRefresh').classList.toggle('busy', state.running);
    el('btnRefresh').setAttribute('aria-label',
      state.running ? `Running: ${status.run.stage}` : 'Refresh now');

    // A run that just finished has new stars to show.
    if (wasRunning && !state.running) loadSky();

    settings.setStatus(status);
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
  try {
    const res = await fetch('/api/refresh', { method: 'POST' });
    if (res.status === 409) return; // already going; the poller will show it
    state.running = true;
    el('btnRefresh').classList.add('busy');
    scheduleStatus(600);
  } catch (err) {
    console.error(err);
  }
}

// ---------- chrome ----------

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
  // message.
  action.hidden = false;
  action.textContent = 'Open settings';
  action.onclick = () => settings.open(status.banner ? 'model' : undefined);
}

// The header count reports what is on screen. Leaving it at the unfiltered total
// while a filter hides most of them makes the number look wrong.
function updateCount() {
  const total = state.groups.length;
  const visible = state.groups.filter((g) => sky.passes(g)).length;
  const shown = state.filters.size ? `${visible} of ${total}` : String(total);
  el('obsDate').textContent =
    `Observation · ${formatDate(state.observationDate)} · ${shown} signal${visible === 1 && !state.filters.size ? '' : 's'}`;
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
el('btnSettings').onclick = (e) => { e.stopPropagation(); settings.toggle() };

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
window.interstellar = { sky, state, panel, settings, reload: loadSky };

loadSky();
loadStatus();
