// Entry point: loads the sky, keeps status in view, and wires the two views
// together.

import { Sky } from './sky.js';
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

    el('obsDate').textContent =
      `Observation · ${formatDate(data.observation_date)} · ${state.groups.length} signal${state.groups.length === 1 ? '' : 's'}`;

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

function updateEmptyState() {
  const empty = el('empty');
  const hasSignals = state.groups.length > 0;
  empty.classList.toggle('on', !hasSignals && state.view === 'sky');
  if (hasSignals) return;

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
      groups: state.groups,
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

el('zin').onclick = () => sky.zoomAt(innerWidth / 2, innerHeight / 2, 1.35);
el('zout').onclick = () => sky.zoomAt(innerWidth / 2, innerHeight / 2, 1 / 1.35);
el('zreset').onclick = () => sky.reset();

loadSky();
loadStatus();
