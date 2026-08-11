// Settings: the gear popover and the four screens behind it (§6).
//
// The Model screen is the substantial one. §3.1 promises that either role can
// run on any model from any provider, switched here without touching code or
// restarting, and that any swap is verifiable in about ten seconds. The test
// button is that promise.

const el = (id) => document.getElementById(id);

export class Settings {
  constructor({ onChanged, onRefreshRequested }) {
    this.onChanged = onChanged;
    this.onRefreshRequested = onRefreshRequested;
    this.status = null;
    this.settings = null;
    this.screen = null;

    this.buildDOM();
    this.bindGlobal();
  }

  // ---------- shell ----------

  buildDOM() {
    this.popover = document.createElement('div');
    this.popover.id = 'settings';
    this.popover.setAttribute('role', 'menu');
    this.popover.setAttribute('aria-label', 'Settings');
    document.body.append(this.popover);

    this.sheet = document.createElement('div');
    this.sheet.id = 'sheet';
    this.sheet.setAttribute('role', 'dialog');
    this.sheet.setAttribute('aria-modal', 'true');
    this.sheet.hidden = true;
    document.body.append(this.sheet);
  }

  bindGlobal() {
    document.addEventListener('click', (e) => {
      if (this.popover.classList.contains('open') &&
          !this.popover.contains(e.target) &&
          !el('btnSettings').contains(e.target)) {
        this.closePopover();
      }
    });
    addEventListener('keydown', (e) => {
      if (e.key !== 'Escape') return;
      if (this.screen) this.closeSheet();
      else if (this.popover.classList.contains('open')) this.closePopover();
    });
  }

  setStatus(status) {
    this.status = status;
    if (this.popover.classList.contains('open')) this.renderPopover();
    if (this.screen === 'model') this.renderModel();
  }

  toggle() {
    if (this.popover.classList.contains('open')) this.closePopover();
    else this.open();
  }

  async open(screen) {
    await this.loadSettings();
    this.popover.classList.add('open');
    el('btnSettings').setAttribute('aria-expanded', 'true');
    this.renderPopover();
    if (screen) this.openSheet(screen);
  }

  closePopover() {
    this.popover.classList.remove('open');
    el('btnSettings').setAttribute('aria-expanded', 'false');
  }

  async loadSettings() {
    try {
      const res = await fetch('/api/settings');
      if (res.ok) this.settings = await res.json();
    } catch (err) {
      console.error(err);
    }
  }

  renderPopover() {
    const s = this.settings;
    const topics = this.status?.topics_count;
    this.popover.replaceChildren();

    const h = document.createElement('h3');
    h.textContent = 'Configuration';
    this.popover.append(h);

    const rows = [
      { key: 'topics', label: 'Topics', value: '—' },
      { key: 'sources', label: 'Sources', value: '—' },
      {
        key: 'model', label: 'Model',
        value: s ? shortModel(s.classifier_model) + ' · ' + shortModel(s.synthesizer_model) : '—',
        warn: this.status && !this.status.ready,
      },
      {
        key: 'schedule', label: 'Schedule',
        value: s ? `${s.schedule_ingest} · ${s.schedule_synthesize}` : '—',
      },
    ];

    for (const row of rows) {
      const div = document.createElement('div');
      div.className = 'set-row';
      div.tabIndex = 0;
      div.setAttribute('role', 'menuitem');

      const label = document.createElement('span');
      label.textContent = row.label;

      const value = document.createElement('span');
      value.className = 'v' + (row.warn ? ' warn' : '');
      value.textContent = row.warn ? 'needs attention' : row.value;

      div.append(label, value);
      div.addEventListener('click', () => this.openSheet(row.key));
      div.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); this.openSheet(row.key) }
      });
      this.popover.append(div);
    }

    const note = document.createElement('div');
    note.className = 'set-note';
    note.textContent = this.status?.calls_today != null
      ? `${this.status.calls_today} LLM calls today, cap ${this.status.call_cap_day}. Changes apply on the next run — no restart.`
      : 'Changes apply on the next run — no restart.';
    this.popover.append(note);
  }

  // ---------- sheet ----------

  openSheet(screen) {
    this.screen = screen;
    this.sheet.hidden = false;
    requestAnimationFrame(() => this.sheet.classList.add('open'));
    this.closePopover();

    switch (screen) {
      case 'topics': this.renderTopics(); break;
      case 'sources': this.renderSources(); break;
      case 'model': this.renderModel(); break;
      case 'schedule': this.renderSchedule(); break;
    }
  }

  closeSheet() {
    this.screen = null;
    this.sheet.classList.remove('open');
    setTimeout(() => { if (!this.screen) this.sheet.hidden = true }, 240);
  }

  sheetFrame(title, subtitle) {
    this.sheet.replaceChildren();

    const close = document.createElement('button');
    close.className = 'p-close';
    close.setAttribute('aria-label', 'Close');
    close.textContent = '\u00d7'; // U+00D7, inside the bundled latin subset
    close.onclick = () => this.closeSheet();

    const h = document.createElement('h2');
    h.className = 'sheet-title';
    h.textContent = title;

    this.sheet.append(close, h);

    if (subtitle) {
      const p = document.createElement('p');
      p.className = 'sheet-sub';
      p.textContent = subtitle;
      this.sheet.append(p);
    }

    const body = document.createElement('div');
    body.className = 'sheet-body';
    this.sheet.append(body);
    return body;
  }

  // ---------- topics ----------

  async renderTopics() {
    const body = this.sheetFrame('Topics',
      'A definition is what the classifier judges against. Write it the way you would explain the topic to a colleague — what counts, and what does not.');

    const topics = await fetchJSON('/api/topics') || [];

    for (const t of topics) {
      body.append(this.topicCard(t));
    }

    const add = document.createElement('button');
    add.className = 'btn-add';
    add.textContent = '+ Add topic';
    add.onclick = async () => {
      const created = await fetchJSON('/api/topics', {
        method: 'POST',
        body: JSON.stringify({ title: 'New topic', definition: '' }),
      });
      if (created) { this.renderTopics(); this.onChanged() }
    };
    body.append(add);
  }

  topicCard(topic) {
    const card = document.createElement('div');
    card.className = 'card';

    const title = document.createElement('input');
    title.className = 'field title';
    title.value = topic.title;
    title.setAttribute('aria-label', 'Topic title');

    const def = document.createElement('textarea');
    def.className = 'field';
    def.rows = 3;
    def.placeholder = 'What belongs in this topic? Be specific — this is the whole basis for matching.';
    def.value = topic.definition;
    def.setAttribute('aria-label', 'Topic definition');

    const meta = document.createElement('div');
    meta.className = 'card-meta';
    const status = document.createElement('span');
    status.textContent = topic.needs_setup
      ? 'No definition yet'
      : `${topic.signals_7d ?? 0} signal${topic.signals_7d === 1 ? '' : 's'} in 7 days`;
    if (topic.needs_setup) status.className = 'warn';

    const actions = document.createElement('div');
    actions.className = 'card-actions';

    const toggle = document.createElement('button');
    toggle.className = 'btn-quiet';
    toggle.textContent = topic.active ? 'Active' : 'Paused';
    toggle.onclick = async () => {
      await fetchJSON(`/api/topics/${topic.id}`, {
        method: 'PATCH', body: JSON.stringify({ active: !topic.active }),
      });
      this.renderTopics(); this.onChanged();
    };

    const save = document.createElement('button');
    save.className = 'btn-primary';
    save.textContent = 'Save';
    save.onclick = async () => {
      save.disabled = true;
      const ok = await fetchJSON(`/api/topics/${topic.id}`, {
        method: 'PATCH',
        body: JSON.stringify({ title: title.value, definition: def.value }),
      });
      save.disabled = false;
      save.textContent = ok ? 'Saved' : 'Failed';
      setTimeout(() => (save.textContent = 'Save'), 1400);
      if (ok) this.onChanged();
    };

    const del = document.createElement('button');
    del.className = 'btn-danger';
    del.textContent = 'Delete';
    del.onclick = async () => {
      // Deleting a topic drops its matches and signals with it, so confirm.
      if (!confirm(`Delete "${topic.title}"? Its signals go too.`)) return;
      await fetch(`/api/topics/${topic.id}`, { method: 'DELETE' });
      this.renderTopics(); this.onChanged();
    };

    actions.append(toggle, save, del);
    meta.append(status, actions);
    card.append(title, def, meta);
    return card;
  }

  // ---------- sources ----------

  async renderSources() {
    const body = this.sheetFrame('Sources',
      'Feeds to watch. A source that keeps failing shows its last successful run here rather than disappearing quietly.');

    const sources = await fetchJSON('/api/sources') || [];

    for (const s of sources) {
      body.append(this.sourceCard(s));
    }

    const add = document.createElement('div');
    add.className = 'card add-source';

    const kind = document.createElement('select');
    kind.className = 'field';
    kind.setAttribute('aria-label', 'Source kind');
    for (const [value, label] of [
      ['rss', 'RSS / Atom feed'],
      ['hn_search', 'Hacker News search'],
      ['youtube_rss', 'YouTube channel'],
    ]) {
      const opt = document.createElement('option');
      opt.value = value; opt.textContent = label;
      kind.append(opt);
    }

    const name = document.createElement('input');
    name.className = 'field';
    name.placeholder = 'Name';
    name.setAttribute('aria-label', 'Source name');

    const value = document.createElement('input');
    value.className = 'field';
    value.placeholder = 'Feed URL';
    value.setAttribute('aria-label', 'Source location');
    kind.onchange = () => {
      value.placeholder = kind.value === 'hn_search' ? 'Search query'
        : kind.value === 'youtube_rss' ? 'Channel ID (UC...)' : 'Feed URL';
    };

    const create = document.createElement('button');
    create.className = 'btn-primary';
    create.textContent = 'Add source';
    create.onclick = async () => {
      const config = kind.value === 'hn_search' ? { query: value.value }
        : kind.value === 'youtube_rss' ? { channel_id: value.value }
        : { url: value.value };
      const ok = await fetchJSON('/api/sources', {
        method: 'POST',
        body: JSON.stringify({ kind: kind.value, name: name.value, config }),
      });
      if (ok) { this.renderSources(); this.onChanged() }
    };

    add.append(kind, name, value, create);
    body.append(add);
  }

  sourceCard(source) {
    const card = document.createElement('div');
    card.className = 'card row';

    const main = document.createElement('div');
    main.className = 'card-main';

    const name = document.createElement('div');
    name.className = 'card-name';
    name.textContent = source.name;

    const detail = document.createElement('div');
    detail.className = 'card-detail';
    detail.textContent = describeSource(source);

    const meta = document.createElement('div');
    meta.className = 'card-detail dim';
    meta.textContent = source.last_run_at
      ? `${source.item_count ?? 0} items · last run ${formatWhen(source.last_run_at)}`
      : `${source.item_count ?? 0} items · never completed a run`;
    if (!source.last_run_at) meta.classList.add('warn');

    main.append(name, detail, meta);

    const actions = document.createElement('div');
    actions.className = 'card-actions';

    const toggle = document.createElement('button');
    toggle.className = 'btn-quiet';
    toggle.textContent = source.active ? 'Active' : 'Paused';
    toggle.onclick = async () => {
      await fetchJSON(`/api/sources/${source.id}`, {
        method: 'PATCH', body: JSON.stringify({ active: !source.active }),
      });
      this.renderSources();
    };

    const del = document.createElement('button');
    del.className = 'btn-danger';
    del.textContent = 'Delete';
    del.onclick = async () => {
      if (!confirm(`Delete "${source.name}"?`)) return;
      await fetch(`/api/sources/${source.id}`, { method: 'DELETE' });
      this.renderSources(); this.onChanged();
    };

    actions.append(toggle, del);
    card.append(main, actions);
    return card;
  }

  // ---------- model ----------

  async renderModel() {
    await this.loadSettings();
    const s = this.settings;
    if (!s) return;

    const body = this.sheetFrame('Model',
      'Either role can run on any provider. Changes take effect on the next run — nothing to restart.');

    // Providers, each with a badge saying whether its key actually resolves.
    // The registry stores the variable NAME; the value never leaves the
    // environment, which is why this shows a state rather than a secret.
    const provSection = section(body, 'Providers');
    for (const p of s.providers) {
      const row = document.createElement('div');
      row.className = 'card row';

      const main = document.createElement('div');
      main.className = 'card-main';

      const name = document.createElement('div');
      name.className = 'card-name';
      name.textContent = p.name;

      const url = document.createElement('div');
      url.className = 'card-detail';
      url.textContent = p.base_url;

      const key = document.createElement('div');
      key.className = 'card-detail dim';
      key.textContent = p.keyless ? 'no key required' : p.api_key_env;

      main.append(name, url, key);

      const badge = document.createElement('span');
      badge.className = 'badge ' + (p.resolved ? 'ok' : 'bad');
      badge.textContent = p.keyless ? 'ready' : (p.resolved ? 'key set' : 'key missing');

      row.append(main, badge);
      provSection.append(row);
    }

    const provNote = document.createElement('p');
    provNote.className = 'note';
    provNote.textContent =
      'Keys are read from the environment by name and never stored. To add one, set the variable on the container and restart it — everything else here changes live.';
    provSection.append(provNote);

    // Roles.
    for (const [role, current, fallback] of [
      ['classifier', s.classifier_model, s.classifier_fallback],
      ['synthesizer', s.synthesizer_model, s.synthesizer_fallback],
    ]) {
      const roleSection = section(body, role === 'classifier' ? 'Classifier' : 'Synthesizer');

      const hint = document.createElement('p');
      hint.className = 'note';
      hint.textContent = role === 'classifier'
        ? 'Scores every item against every topic. This is where most of the cost goes, so a cheap model earns its place here.'
        : 'Groups matched items into stories. The hardest judgment in the pipeline.';
      roleSection.append(hint);

      const input = document.createElement('input');
      input.className = 'field mono';
      input.value = current;
      input.placeholder = 'provider/model';
      input.setAttribute('aria-label', `${role} model`);

      const fb = document.createElement('input');
      fb.className = 'field mono';
      fb.value = (fallback || []).join(', ');
      fb.placeholder = 'fallbacks, comma separated (may use other providers)';
      fb.setAttribute('aria-label', `${role} fallback chain`);

      const actions = document.createElement('div');
      actions.className = 'card-actions wide';

      const result = document.createElement('div');
      result.className = 'test-result';

      const save = document.createElement('button');
      save.className = 'btn-primary';
      save.textContent = 'Save';
      save.onclick = async () => {
        const payload = {};
        payload[`${role}_model`] = input.value.trim();
        payload[`${role}_fallback`] = fb.value.split(',').map((v) => v.trim()).filter(Boolean);
        const res = await fetch('/api/settings', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
        if (res.ok) {
          result.textContent = 'Saved. It applies on the next run.';
          result.className = 'test-result ok';
          this.onChanged();
        } else {
          const err = await res.json().catch(() => ({}));
          result.textContent = err.error || 'Could not save';
          result.className = 'test-result bad';
        }
      };

      // §3.1: verify a swap in about ten seconds, without waiting for a run.
      const test = document.createElement('button');
      test.className = 'btn-quiet';
      test.textContent = 'Test call';
      test.onclick = async () => {
        test.disabled = true;
        result.textContent = 'Calling…';
        result.className = 'test-result';
        const out = await fetchJSON('/api/model/test', {
          method: 'POST', body: JSON.stringify({ role }),
        });
        test.disabled = false;
        if (!out) { result.textContent = 'Request failed'; result.className = 'test-result bad'; return }
        if (out.ok) {
          result.textContent = `${out.provider}/${out.model} answered in ${out.latency_ms}ms (${out.tokens_in}→${out.tokens_out} tokens)`;
          result.className = 'test-result ok';
        } else {
          result.textContent = out.error;
          result.className = 'test-result bad';
        }
      };

      actions.append(save, test);
      roleSection.append(input, fb, actions, result);
    }

    // The escalation ladder is an operator runbook, not code (§3.1).
    const ladder = section(body, 'When to step up');
    const list = document.createElement('ul');
    list.className = 'ladder';
    for (const line of [
      'Classifier failing the JSON parse gate → step up a tier (a Google flash-class model, then an Anthropic Haiku-class one).',
      'Digest reading poorly → step the synthesizer up (an Anthropic Sonnet-class model).',
      'Each step is a change on this screen. Nothing to rebuild, nothing to restart.',
    ]) {
      const li = document.createElement('li');
      li.textContent = line;
      list.append(li);
    }
    ladder.append(list);
  }

  // ---------- schedule ----------

  async renderSchedule() {
    await this.loadSettings();
    const s = this.settings;
    if (!s) return;

    const body = this.sheetFrame('Schedule',
      `Times are local to the container (${s.timezone}). The aim is a fresh sky before you look at it.`);

    const grid = document.createElement('div');
    grid.className = 'grid';

    const fields = [
      ['schedule_ingest', 'Ingest', s.schedule_ingest, 'time', 'When to fetch feeds.'],
      ['schedule_synthesize', 'Synthesize', s.schedule_synthesize, 'time', 'When to classify and compose.'],
      ['max_calls_per_run', 'Max LLM calls per run', s.max_calls_per_run, 'number', 'A runaway run stops here.'],
      ['max_calls_per_day', 'Max LLM calls per day', s.max_calls_per_day, 'number', 'Survives a restart.'],
      ['sky_days', 'Days of sky', s.sky_days, 'number', 'How far back the star map reaches.'],
      ['hn_min_points', 'HN minimum points', s.hn_min_points, 'number', 'Below this, Hacker News is noise.'],
    ];

    for (const [key, label, value, type, help] of fields) {
      const wrap = document.createElement('label');
      wrap.className = 'field-wrap';

      const l = document.createElement('span');
      l.className = 'field-label';
      l.textContent = label;

      const input = document.createElement('input');
      input.className = 'field';
      input.type = type;
      input.value = value;
      input.dataset.key = key;

      const h = document.createElement('span');
      h.className = 'field-help';
      h.textContent = help;

      wrap.append(l, input, h);
      grid.append(wrap);
    }
    body.append(grid);

    const result = document.createElement('div');
    result.className = 'test-result';

    const save = document.createElement('button');
    save.className = 'btn-primary';
    save.textContent = 'Save';
    save.onclick = async () => {
      const payload = {};
      for (const input of grid.querySelectorAll('input')) {
        payload[input.dataset.key] = input.type === 'number'
          ? Number(input.value) : input.value;
      }
      const res = await fetch('/api/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.ok) {
        result.textContent = 'Saved. The schedule was rebuilt — no restart needed.';
        result.className = 'test-result ok';
        this.onChanged();
      } else {
        const err = await res.json().catch(() => ({}));
        result.textContent = err.error || 'Could not save';
        result.className = 'test-result bad';
      }
    };

    const now = document.createElement('button');
    now.className = 'btn-quiet';
    now.textContent = 'Run now';
    now.onclick = () => { this.onRefreshRequested(); this.closeSheet() };

    const actions = document.createElement('div');
    actions.className = 'card-actions wide';
    actions.append(save, now);
    body.append(actions, result);

    if (this.status?.next_runs?.length) {
      const next = document.createElement('p');
      next.className = 'note';
      next.textContent = 'Next runs: ' +
        this.status.next_runs.map((t) => new Date(t).toLocaleString()).join(' · ');
      body.append(next);
    }
  }
}

// ---------- helpers ----------

function section(parent, title) {
  const wrap = document.createElement('section');
  wrap.className = 'sheet-section';
  const h = document.createElement('h3');
  h.textContent = title;
  wrap.append(h);
  parent.append(wrap);
  return wrap;
}

async function fetchJSON(url, options = {}) {
  try {
    const res = await fetch(url, {
      ...options,
      headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
    });
    if (!res.ok) return null;
    if (res.status === 204) return true;
    return await res.json();
  } catch (err) {
    console.error(err);
    return null;
  }
}

function shortModel(ref) {
  if (!ref) return 'unset';
  const [provider] = ref.split('/');
  return provider;
}

function describeSource(source) {
  const cfg = source.config || {};
  if (source.kind === 'hn_search') return `Hacker News · "${cfg.query || ''}"`;
  if (source.kind === 'youtube_rss') return `YouTube · ${cfg.channel_id || cfg.url || 'no channel set'}`;
  return cfg.url || '';
}

function formatWhen(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const hours = (Date.now() - d.getTime()) / 36e5;
  if (hours < 1) return 'just now';
  if (hours < 24) return `${Math.round(hours)}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}
