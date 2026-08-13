// Settings: the gear popover and the four screens behind it (§6).
//
// The Model screen is the substantial one. §3.1 promises that either role can
// run on any model from any provider, switched here without touching code or
// restarting, and that any swap is verifiable in about ten seconds. The test
// button is that promise.

const el = (id) => document.getElementById(id);

// The three fetchers internal/sources registers. Shared by the add and edit forms
// so they cannot drift.
const SOURCE_KINDS = [
  ['rss', 'RSS / Atom feed'],
  ['hn_search', 'Hacker News search'],
  ['youtube_rss', 'YouTube channel'],
];

export class Settings {
  constructor({ onChanged, onRefreshRequested }) {
    this.onChanged = onChanged;
    this.onRefreshRequested = onRefreshRequested;
    this.status = null;
    this.settings = null;
    this.quality = null;    // last /api/topics/stats response, for the popover row
    this.qualityDays = 7;
    this.screen = null;
    this.modelSig = null;   // signature of the last Model render, see refreshModel

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
    if (this.screen === 'model') this.refreshModel();
  }

  // The Model screen draws nothing from `status` -- it was re-rendered on every
  // poll purely to keep the per-provider key badges live. But renderModel goes
  // through sheetFrame, which replaceChildren()s the sheet and builds a fresh
  // .sheet-body, and .sheet-body is the scroller. So every status poll threw
  // away the scroll position: every 20s while idle, every 1.5s during a run.
  //
  // The badges still need to go green the moment a key resolves, so the refresh
  // stays -- it just has to earn the rebuild.
  async refreshModel() {
    // Never rebuild under the user's hands. A poll landing mid-edit would take
    // the field, its caret and whatever was typed into it.
    const active = document.activeElement;
    if (active && this.sheet.contains(active) && /^(INPUT|SELECT|TEXTAREA)$/.test(active.tagName)) {
      return;
    }

    await this.loadSettings();
    if (this.modelSignature() === this.modelSig) return;   // identical DOM; leave it alone

    await this.rerenderCurrent();
  }

  // Everything renderModel draws. /api/settings carries no timestamp or other
  // volatile field, so an unchanged signature really does mean an identical
  // render.
  modelSignature() {
    return this.settings ? JSON.stringify(this.settings) : '';
  }

  toggle() {
    if (this.popover.classList.contains('open')) this.closePopover();
    else this.open();
  }

  async open(screen) {
    await this.loadSettings();
    // The popover's quality row needs a number to show. Fetched alongside
    // settings rather than on every status poll: it changes once a run, and the
    // popover is the only thing reading it until the screen opens.
    this.quality = await fetchJSON(`/api/topics/stats?days=${this.qualityDays}`);
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
    this.popover.replaceChildren();

    // Counts come from /api/status. Until the first status lands they are
    // genuinely unknown, and an em dash says that honestly -- but it used to be
    // hardcoded on both rows, so it never became a number no matter what the
    // server said.
    const count = (n, one, many) =>
      n == null ? '—' : `${n} ${n === 1 ? one : many}`;

    const h = document.createElement('h3');
    h.textContent = 'Configuration';
    this.popover.append(h);

    const rows = [
      { key: 'topics', label: 'Topics', value: count(this.status?.topics_count, 'topic', 'topics') },
      { key: 'sources', label: 'Sources', value: count(this.status?.sources_count, 'source', 'sources') },
      {
        key: 'model', label: 'Model',
        value: s ? shortModel(s.classifier_model) + ' · ' + shortModel(s.synthesizer_model) : '—',
        warn: this.status && !this.status.ready,
      },
      {
        key: 'schedule', label: 'Schedule',
        value: s ? `${s.schedule_ingest} · ${s.schedule_synthesize}` : '—',
      },
      // The hit rate is the one number here that is measured rather than
      // configured, so the row shows it directly -- the point is to notice it in
      // passing, not to have to go looking.
      {
        key: 'quality', label: 'Signal quality',
        value: this.quality?.ready ? formatRate(this.quality.hit_rate) + ' matched' : 'no data yet',
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

    this.renderScreen(screen);
  }

  renderScreen(screen) {
    switch (screen) {
      case 'topics': return this.renderTopics();
      case 'sources': return this.renderSources();
      case 'model': return this.renderModel();
      case 'schedule': return this.renderSchedule();
      case 'quality': return this.renderQuality();
    }
    return Promise.resolve();
  }

  // Re-render the open screen in place, keeping the reader where they were.
  //
  // Every render goes through sheetFrame, which replaceChildren()s the sheet and
  // builds a fresh .sheet-body -- and .sheet-body is the scroller, so a rebuild
  // always lands back at the top. Adding a topic to a list you had scrolled to
  // the bottom of threw you back to the first one, which is precisely the moment
  // you least want to lose your place.
  //
  // Restoring after the await matters: these renders fetch before they append,
  // so scrollTop set any earlier lands on an empty element and does not stick.
  async rerenderCurrent() {
    if (!this.screen) return;
    const prev = this.sheet.querySelector('.sheet-body')?.scrollTop ?? 0;
    await this.renderScreen(this.screen);
    const body = this.sheet.querySelector('.sheet-body');
    if (body) body.scrollTop = prev;
  }

  closeSheet() {
    this.screen = null;
    this.sheet.classList.remove('open');
    setTimeout(() => { if (!this.screen) this.sheet.hidden = true }, 240);
  }

  // Every screen gets the same frame: a sticky head carrying the title and the
  // close button, and a scrolling body. The sheets are long \u2014 Model in particular
  // \u2014 and losing the heading part-way down leaves you unsure which screen you are
  // editing.
  sheetFrame(title, subtitle) {
    this.sheet.replaceChildren();

    const head = document.createElement('div');
    head.className = 'sheet-head';

    const close = document.createElement('button');
    close.className = 'p-close';
    close.setAttribute('aria-label', 'Close');
    close.textContent = '\u00d7'; // U+00D7, inside the bundled latin subset
    close.onclick = () => this.closeSheet();

    const h = document.createElement('h2');
    h.className = 'sheet-title';
    h.textContent = title;

    head.append(close, h);
    this.sheet.append(head);

    const body = document.createElement('div');
    body.className = 'sheet-body';

    // The subtitle scrolls with the body. It is orientation you read once, not
    // something worth permanently occupying the top of a small screen.
    if (subtitle) {
      const p = document.createElement('p');
      p.className = 'sheet-sub';
      p.textContent = subtitle;
      body.append(p);
    }

    this.sheet.append(body);
    return body;
  }

  // A labelled switch. Returns the element; the caller supplies what flipping it
  // means. Kept here rather than duplicated per screen because topics and sources
  // must not drift apart on something this visible.
  switchControl(initial, label, onFlip) {
    // Mutable, and deliberately so: the switch is not re-rendered after a flip,
    // so its own idea of the current state is the only one there is.
    let on = initial;

    const btn = document.createElement('button');
    btn.className = 'switch';
    btn.setAttribute('role', 'switch');
    btn.setAttribute('aria-checked', String(on));
    btn.setAttribute('aria-label', label);

    const track = document.createElement('span');
    track.className = 'track';
    const knob = document.createElement('span');
    knob.className = 'knob';
    track.append(knob);

    const text = document.createElement('span');
    text.className = 'lbl';
    text.textContent = on ? 'Active' : 'Paused';

    btn.append(track, text);
    btn.onclick = async () => {
      // Flip immediately, then reconcile. The switch is the one control where a
      // network round-trip before any movement makes the UI feel broken.
      const next = !on;
      btn.setAttribute('aria-checked', String(next));
      text.textContent = next ? 'Active' : 'Paused';
      btn.disabled = true;
      const ok = await onFlip(next);
      btn.disabled = false;

      if (ok) {
        on = next;
      } else {
        // Snap back and say so. A switch that silently returns to where it was
        // looks like a UI bug rather than a failed request.
        btn.setAttribute('aria-checked', String(on));
        text.textContent = 'Failed';
        setTimeout(() => { text.textContent = on ? 'Active' : 'Paused' }, 1600);
      }
    };
    return btn;
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
      if (created) { this.rerenderCurrent(); this.onChanged() }
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

    // No re-render on flip: rebuilding the list would discard whatever the owner
    // has typed into a sibling card's definition and reset the scroll position.
    const toggle = this.switchControl(topic.active, `${topic.title} active`, async (next) => {
      const ok = await fetchJSON(`/api/topics/${topic.id}`, {
        method: 'PATCH', body: JSON.stringify({ active: next }),
      });
      if (ok) { topic.active = next; this.onChanged() }
      return !!ok;
    });

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
      this.rerenderCurrent(); this.onChanged();
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
    for (const [value, label] of SOURCE_KINDS) {
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
      if (ok) { this.rerenderCurrent(); this.onChanged() }
    };

    add.append(kind, name, value, create);
    body.append(add);

    body.append(this.pasteLink());
    body.append(this.sourceTransfer());
  }

  // Paste a link. The sanctioned route for the places the spec puts permanently
  // out of reach — LinkedIn on ToS grounds, X on pricing — since pasting a URL you
  // are already looking at breaks neither.
  pasteLink() {
    const section = document.createElement('section');
    section.className = 'sheet-section transfer';

    const h = document.createElement('h3');
    h.textContent = 'Save a link';
    section.append(h);

    const note = document.createElement('p');
    note.className = 'note';
    note.textContent = 'Paste anything worth keeping — including from places no feed can reach. ' +
      'It is fetched now for its title, and classified on the next run.';
    section.append(note);

    const row = document.createElement('div');
    row.className = 'card add-source';

    const url = document.createElement('input');
    url.className = 'field';
    url.type = 'url';
    url.placeholder = 'https://…';
    url.setAttribute('aria-label', 'Link to save');

    const result = document.createElement('div');
    result.className = 'test-result';

    const save = document.createElement('button');
    save.className = 'btn-primary';
    save.textContent = 'Save';

    const submit = async () => {
      const value = url.value.trim();
      if (!value) return;

      save.disabled = true;
      result.className = 'test-result';
      result.textContent = 'Reading the page…';

      const res = await fetch('/api/inbox', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url: value }),
      });
      const data = await res.json().catch(() => ({}));
      save.disabled = false;

      if (!res.ok) {
        result.className = 'test-result bad';
        result.textContent = data.error || 'Could not save that link.';
        return;
      }

      result.className = 'test-result ok';
      // The title is the confirmation that matters: it proves the page was really
      // read, rather than a URL having been written down.
      result.textContent = `${data.new ? 'Saved' : 'Already saved'}: ${data.title}`;
      url.value = '';
      this.onChanged();
    };

    save.onclick = submit;
    url.onkeydown = (e) => { if (e.key === 'Enter') { e.preventDefault(); submit() } };

    row.append(url, save);
    section.append(row, result);
    return section;
  }

  // Export and import, for keeping two instances' lists in step.
  //
  // Import is additive and verifies by default: a bundle is a pile of URLs nobody
  // has fetched here, and CLAUDE.md's rule about that exists because a feed can
  // answer 200 and carry nothing. Anything that fails lands paused, with the
  // reason, rather than live or silently dropped.
  sourceTransfer() {
    const section = document.createElement('section');
    section.className = 'sheet-section transfer';

    const h = document.createElement('h3');
    h.textContent = 'Move this list';
    section.append(h);

    const note = document.createElement('p');
    note.className = 'note';
    note.textContent = 'Export writes a file you can diff against another instance. ' +
      'Import only adds — it never deletes what you have pruned here — and each new ' +
      'feed is fetched once to check it actually returns items.';
    section.append(note);

    const result = document.createElement('div');
    result.className = 'test-result';

    const actions = document.createElement('div');
    actions.className = 'card-actions wide';

    const exportBtn = document.createElement('a');
    exportBtn.className = 'btn-quiet';
    exportBtn.textContent = 'Export';
    exportBtn.href = '/api/sources/export';
    // download rather than a fetch-and-blob dance: the endpoint already sends
    // Content-Disposition, so the browser does the right thing on its own.
    exportBtn.setAttribute('download', 'interstellar-sources.json');

    const file = document.createElement('input');
    file.type = 'file';
    file.accept = 'application/json,.json';
    file.hidden = true;
    file.onchange = async () => {
      const chosen = file.files?.[0];
      if (!chosen) return;

      result.className = 'test-result';
      result.textContent = 'Checking each feed…';

      let bundle;
      try {
        bundle = JSON.parse(await chosen.text());
      } catch {
        result.className = 'test-result bad';
        result.textContent = 'That file is not JSON.';
        return;
      }

      const res = await fetchJSON('/api/sources/import', {
        method: 'POST',
        body: JSON.stringify({ ...bundle, verify: true }),
      });
      // Reset, or choosing the same file twice does nothing.
      file.value = '';

      if (!res) {
        result.className = 'test-result bad';
        result.textContent = 'Import failed. Check the file version.';
        return;
      }

      result.className = 'test-result ok';
      const parts = [`${res.added} added`, `${res.skipped} already here`];
      if (res.rejected) parts.push(`${res.rejected} rejected`);
      const paused = (res.results || []).filter((r) => r.status === 'added_inactive');
      if (paused.length) parts.push(`${paused.length} paused (did not verify)`);
      result.textContent = parts.join(' · ');

      this.rerenderCurrent();
      this.onChanged();
    };

    const importBtn = document.createElement('button');
    importBtn.className = 'btn-quiet';
    importBtn.textContent = 'Import…';
    importBtn.onclick = () => file.click();

    actions.append(exportBtn, importBtn, file);
    section.append(actions, result);
    return section;
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

    const toggle = this.switchControl(source.active, `${source.name} active`, async (next) => {
      const ok = await fetchJSON(`/api/sources/${source.id}`, {
        method: 'PATCH', body: JSON.stringify({ active: next }),
      });
      if (ok) source.active = next;
      return !!ok;
    });

    // Editing rather than delete-and-recreate. A source carries its item history
    // through item_sources, so recreating one to fix a typo in its URL silently
    // orphans every attribution it has ever contributed.
    const editBtn = document.createElement('button');
    editBtn.className = 'btn-quiet';
    editBtn.textContent = 'Edit';

    const del = document.createElement('button');
    del.className = 'btn-danger';
    del.textContent = 'Delete';
    del.onclick = async () => {
      if (!confirm(`Delete "${source.name}"?`)) return;
      await fetch(`/api/sources/${source.id}`, { method: 'DELETE' });
      this.rerenderCurrent(); this.onChanged();
    };

    actions.append(toggle, editBtn, del);
    // The editor is built on first click, not here. Building one per card meant 68
    // hidden forms and 272 form controls in a single scrolling sheet — controls are
    // among the most expensive elements to lay out, and it was the sheet's scroll
    // cost. Almost none of them are ever opened.
    let editor = null;
    editBtn.onclick = () => {
      if (!editor) {
        editor = this.sourceEditor(source, card, editBtn, name, detail);
        card.append(editor.box);
      }
      const opening = !card.classList.contains('editing');
      if (opening) editor.fill();
      card.classList.toggle('editing', opening);
      editBtn.textContent = opening ? 'Editing' : 'Edit';
      if (opening) editor.focus();
    };

    card.append(main, actions);
    return card;
  }

  // The edit form for one source. Built once and revealed in place, so opening it
  // never costs a request and closing it never loses the card's position.
  sourceEditor(source, card, editBtn, nameEl, detailEl) {
    const box = document.createElement('div');
    box.className = 'src-edit';

    const pair = document.createElement('div');
    pair.className = 'pair';

    const name = document.createElement('input');
    name.className = 'field';
    name.setAttribute('aria-label', 'Source name');

    const kind = document.createElement('select');
    kind.className = 'field';
    kind.setAttribute('aria-label', 'Source kind');
    for (const [value, label] of SOURCE_KINDS) {
      const opt = document.createElement('option');
      opt.value = value; opt.textContent = label;
      kind.append(opt);
    }

    const location = document.createElement('input');
    location.className = 'field';
    location.setAttribute('aria-label', 'Source location');

    // hn_search carries a second knob: its own points floor, which overrides the
    // global one so a broad query and a niche one can coexist.
    const points = document.createElement('input');
    points.className = 'field';
    points.type = 'number';
    points.min = '0';
    points.placeholder = 'Min points';
    points.setAttribute('aria-label', 'Minimum points for this query');

    const syncKind = () => {
      location.placeholder = kind.value === 'hn_search' ? 'Search query'
        : kind.value === 'youtube_rss' ? 'Channel ID (UC...)' : 'Feed URL';
      points.hidden = kind.value !== 'hn_search';
    };
    kind.onchange = syncKind;

    const fill = () => {
      const cfg = source.config || {};
      name.value = source.name;
      kind.value = source.kind;
      location.value = cfg.url || cfg.query || cfg.channel_id || '';
      points.value = cfg.min_points ?? '';
      syncKind();
    };

    const rowActions = document.createElement('div');
    rowActions.className = 'row-actions';

    const cancel = document.createElement('button');
    cancel.className = 'btn-quiet';
    cancel.textContent = 'Cancel';
    cancel.onclick = () => { card.classList.remove('editing'); editBtn.textContent = 'Edit' };

    const save = document.createElement('button');
    save.className = 'btn-primary';
    save.textContent = 'Save';
    save.onclick = async () => {
      const value = location.value.trim();
      const config = kind.value === 'hn_search' ? { query: value }
        : kind.value === 'youtube_rss' ? { channel_id: value }
        : { url: value };
      // Only send min_points when it has one, so clearing the box falls back to
      // the global setting rather than pinning the query at zero.
      if (kind.value === 'hn_search' && points.value !== '') {
        config.min_points = Number(points.value);
      }

      save.disabled = true;
      const updated = await fetchJSON(`/api/sources/${source.id}`, {
        method: 'PATCH',
        body: JSON.stringify({ kind: kind.value, name: name.value.trim(), config }),
      });
      save.disabled = false;

      if (!updated) { save.textContent = 'Failed'; setTimeout(() => (save.textContent = 'Save'), 1600); return }

      // Update the card from the server's answer, not from the form: the API
      // trims names and could reject a config shape we guessed wrong.
      Object.assign(source, updated);
      nameEl.textContent = source.name;
      detailEl.textContent = describeSource(source);
      card.classList.remove('editing');
      editBtn.textContent = 'Edit';
      this.onChanged();
    };

    pair.append(name, kind);
    rowActions.append(cancel, save);
    box.append(pair, location, points, rowActions);
    return { box, fill, focus: () => name.focus() };
  }

  // ---------- model ----------

  async renderModel() {
    await this.loadSettings();
    const s = this.settings;
    if (!s) return;
    // What this render reflects, so refreshModel can tell when a rebuild would
    // change nothing.
    this.modelSig = this.modelSignature();

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

          // Adopt the saved state as what this screen already shows, before
          // onChanged kicks off a status load. Otherwise the poll finds settings
          // that differ from the last render, rebuilds the screen, and wipes the
          // confirmation within milliseconds of the reader seeing it -- and the
          // rebuild is redundant anyway, since the fields already hold exactly
          // what was just saved.
          await this.loadSettings();
          this.modelSig = this.modelSignature();

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
      ['transcripts_per_run', 'Videos transcribed per run', s.transcripts_per_run, 'number',
        'Captions fetched and summarized so the classifier can judge a video on what was said, not its title. 0 turns it off.'],
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

  // ---------- signal quality ----------

  // The per-topic hit rate: how much of what the classifier read against a topic
  // it judged relevant.
  //
  // This is the only screen showing a measurement rather than a setting, and the
  // measurement it shows is a denominator that exists nowhere else -- `matches`
  // keeps score >= 4 verdicts only, so without this the difference between a
  // topic that matched 2 of 4 items and one that matched 2 of 400 is invisible.
  // Both look like "2 matches"; only one is a definition that needs rewriting.
  async renderQuality() {
    const body = this.sheetFrame('Signal quality',
      'How much of what the classifier read, it judged relevant. A very low rate usually means a topic definition is drawn too tight; a very high one means it is too loose and everything gets through.');

    const data = await fetchJSON(`/api/topics/stats?days=${this.qualityDays}`);
    this.quality = data;

    body.append(this.qualityWindowPicker());

    if (!data) {
      const err = document.createElement('p');
      err.className = 'test-result bad';
      err.textContent = 'Could not read the statistics.';
      body.append(err);
      return;
    }

    // Lead with the instance-wide number, then break it down.
    const summary = document.createElement('div');
    summary.className = 'quality-summary';
    if (data.ready) {
      summary.append(
        statTile(formatRate(data.hit_rate), 'matched overall'),
        statTile(String(data.items_considered), 'items read'),
        statTile(String(data.matches_kept), 'matches kept'),
        statTile(String(data.batches), 'classifier calls'),
      );
    } else {
      const none = document.createElement('p');
      none.className = 'note';
      none.textContent = this.status?.ready === false
        ? 'Nothing classified yet — finish setting up a model, then run once.'
        : 'Nothing classified in this window yet. Run a refresh, or widen the window.';
      summary.append(none);
    }
    body.append(summary);

    const rows = data.rows || [];
    if (!rows.length) {
      const none = document.createElement('p');
      none.className = 'note';
      none.textContent = 'No topics yet.';
      body.append(none);
      return;
    }

    const table = document.createElement('div');
    table.className = 'quality-table';
    table.setAttribute('role', 'table');
    table.setAttribute('aria-label',
      `Per-topic hit rate over ${data.days} ${data.days === 1 ? 'day' : 'days'}`);

    const head = document.createElement('div');
    head.className = 'qt-row qt-head';
    head.setAttribute('role', 'row');
    for (const [label, cls] of [['Topic', 'qt-topic'], ['Read', 'qt-n'],
                                ['Matched', 'qt-n'], ['Rate', 'qt-rate'], ['Signals', 'qt-n']]) {
      const cell = document.createElement('span');
      cell.className = cls;
      cell.setAttribute('role', 'columnheader');
      cell.textContent = label;
      head.append(cell);
    }
    table.append(head);

    for (const row of rows) {
      table.append(qualityRow(row));
    }
    body.append(table);

    const note = document.createElement('p');
    note.className = 'note';
    note.textContent =
      'Read counts only the batches a model answered, so a provider having a bad afternoon does not show up as a badly defined topic. ' +
      'Matches are raw verdicts; signals are what the synthesizer composed from them and what the sky actually shows.';
    body.append(note);

    await this.renderHistory(body);
  }

  // Constellation history, on the same screen as the hit rate.
  //
  // Its own screen would be a fifth sheet for two tables that answer the same
  // question — is this topic working — from opposite ends: the hit rate says what
  // the classifier is doing now, the history says what came of it over weeks.
  async renderHistory(body) {
    // The window here is a month regardless of the hit-rate picker above: a trend
    // over one day is not a trend, and the two questions want different spans.
    const data = await fetchJSON('/api/trending?days=30');
    if (!data) return;

    const wrap = section(body, 'Last 30 days');

    if (!data.total_signals) {
      const none = document.createElement('p');
      none.className = 'note';
      none.textContent = 'No signals in the last 30 days yet. This fills in as runs accumulate.';
      wrap.append(none);
      return;
    }

    const summary = document.createElement('div');
    summary.className = 'quality-summary';
    summary.append(
      statTile(String(data.total_signals), 'signals'),
      statTile(String(data.total_hot), 'hot (3+ sources)'),
      // The number that makes the other two readable: six signals over two days and
      // over three weeks are different situations.
      statTile(String(data.days_with_signals), 'days with signals'),
    );
    wrap.append(summary);

    const table = document.createElement('div');
    table.className = 'quality-table';
    table.setAttribute('role', 'table');
    table.setAttribute('aria-label', 'Topic history over 30 days');

    const head = document.createElement('div');
    head.className = 'qt-row qt-head';
    head.setAttribute('role', 'row');
    for (const [label, cls] of [['Topic', 'qt-topic'], ['Signals', 'qt-n'],
                                ['Hot', 'qt-n'], ['Trend', 'qt-rate'], ['Days', 'qt-n']]) {
      const cell = document.createElement('span');
      cell.className = cls;
      cell.setAttribute('role', 'columnheader');
      cell.textContent = label;
      head.append(cell);
    }
    table.append(head);

    for (const t of data.topics || []) {
      table.append(trendRow(t));
    }
    wrap.append(table);

    if ((data.recurring || []).length) {
      const sub = document.createElement('h3');
      sub.textContent = 'Kept coming back';
      sub.style.marginTop = '22px';
      wrap.append(sub);

      const hint = document.createElement('p');
      hint.className = 'note';
      hint.style.marginTop = '0';
      hint.textContent = 'Stories the synthesizer composed on several separate days. ' +
        'The sky shows one day at a time, so this is the only place a story that never went away is visible.';
      wrap.append(hint);

      const list = document.createElement('div');
      list.className = 'quality-table';
      for (const story of data.recurring) {
        const row = document.createElement('div');
        row.className = 'qt-row recurring';

        const title = document.createElement('span');
        title.className = 'qt-topic';
        title.textContent = story.title;
        title.append(quietTag(story.topic_title));

        const days = document.createElement('span');
        days.className = 'qt-n';
        days.textContent = `${story.days}d`;

        const span = document.createElement('span');
        span.className = 'qt-rate';
        span.textContent = `${story.first_seen.slice(5)} → ${story.last_seen.slice(5)}`;

        row.append(title, days, span);
        list.append(row);
      }
      wrap.append(list);
    }
  }

  qualityWindowPicker() {
    const wrap = document.createElement('div');
    wrap.className = 'window-picker';
    wrap.setAttribute('role', 'group');
    wrap.setAttribute('aria-label', 'Window');

    for (const days of [1, 7, 30]) {
      const btn = document.createElement('button');
      btn.className = 'btn-quiet' + (days === this.qualityDays ? ' on' : '');
      btn.setAttribute('aria-pressed', String(days === this.qualityDays));
      btn.textContent = days === 1 ? 'Today' : `${days} days`;
      btn.onclick = () => {
        if (days === this.qualityDays) return;
        this.qualityDays = days;
        this.rerenderCurrent();
      };
      wrap.append(btn);
    }
    return wrap;
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

// formatRate renders a 0..1 fraction as a percentage, and the -1 sentinel as an
// em dash. Those are genuinely different answers: a topic that has never been
// classified is not a topic that matches nothing, and rendering both as "0%"
// sends the owner off to rewrite a definition that was never used.
function formatRate(rate) {
  if (rate == null || rate < 0) return '—';
  const pct = rate * 100;
  // One decimal below 10%, where the interesting differences live: 0.4% and 2%
  // are very different definitions and both round to the same whole number.
  return (pct < 10 && pct > 0 ? pct.toFixed(1) : Math.round(pct)) + '%';
}

function statTile(value, label) {
  const tile = document.createElement('div');
  tile.className = 'stat-tile';

  const v = document.createElement('span');
  v.className = 'stat-value';
  v.textContent = value;

  const l = document.createElement('span');
  l.className = 'stat-label';
  l.textContent = label;

  tile.append(v, l);
  return tile;
}

// qualityRow is one topic. Flags the two shapes worth acting on: a definition
// that was never used, and one that is plainly mis-drawn.
function qualityRow(row) {
  // Not named `el`: that is the module's getElementById helper, and shadowing it
  // inside a function that also builds DOM is a trap waiting for the next edit.
  const tr = document.createElement('div');
  tr.className = 'qt-row';
  tr.setAttribute('role', 'row');

  const name = document.createElement('span');
  name.className = 'qt-topic';
  name.setAttribute('role', 'cell');
  name.textContent = row.title;
  if (!row.active) name.append(quietTag('paused'));
  if (row.needs_setup) name.append(quietTag('no definition'));

  const cells = [
    [String(row.items_considered || '—'), 'qt-n'],
    [String(row.matches_kept || (row.items_considered ? '0' : '—')), 'qt-n'],
    [formatRate(row.hit_rate), 'qt-rate'],
    [String(row.signals || (row.items_considered ? '0' : '—')), 'qt-n'],
  ];

  tr.append(name);
  for (const [text, cls] of cells) {
    const cell = document.createElement('span');
    cell.className = cls;
    cell.setAttribute('role', 'cell');
    cell.textContent = text;
    tr.append(cell);
  }

  // Only judge a topic once there is enough behind the number to judge it on. A
  // 0% over three items is noise; over three hundred it is a finding.
  const rate = row.hit_rate;
  if (row.items_considered >= 50 && rate >= 0) {
    const verdict = tr.querySelector('.qt-rate');
    if (rate < 0.02) {
      verdict.classList.add('bad');
      verdict.title = 'Almost nothing matches. The definition may be too narrow, or the sources may not cover this topic.';
    } else if (rate > 0.5) {
      verdict.classList.add('loose');
      verdict.title = 'More than half of everything read matches. The definition is probably too broad to be useful.';
    }
  }
  return tr;
}

// trendRow is one topic's month. Direction comes from the server so the two
// cannot disagree about what "rising" means.
function trendRow(t) {
  const tr = document.createElement('div');
  tr.className = 'qt-row';
  tr.setAttribute('role', 'row');

  const name = document.createElement('span');
  name.className = 'qt-topic';
  name.setAttribute('role', 'cell');
  name.textContent = t.title;
  if (!t.active) name.append(quietTag('paused'));

  const cells = [
    [String(t.signals || '—'), 'qt-n'],
    [String(t.hot || (t.signals ? '0' : '—')), 'qt-n'],
    [t.direction, 'qt-rate'],
    [String(t.days_with_signals || '—'), 'qt-n'],
  ];

  tr.append(name);
  for (const [text, cls] of cells) {
    const cell = document.createElement('span');
    cell.className = cls;
    cell.setAttribute('role', 'cell');
    cell.textContent = text;
    tr.append(cell);
  }

  // Only the two directions worth acting on are coloured. "rising" is not a
  // problem and does not need flagging.
  const verdict = tr.querySelector('.qt-rate');
  if (t.direction === 'stopped') {
    verdict.classList.add('bad');
    verdict.title = 'This topic produced signals earlier in the window and none recently. Either the news dried up or a source stopped working.';
  } else if (t.direction === 'quiet') {
    verdict.classList.add('quiet');
    verdict.title = 'Nothing at all in this window.';
  }
  return tr;
}

function quietTag(text) {
  const tag = document.createElement('span');
  tag.className = 'qt-tag';
  tag.textContent = text;
  return tag;
}

function shortModel(ref) {
  if (!ref) return 'unset';
  const [provider] = ref.split('/');
  return provider;
}

function describeSource(source) {
  const cfg = source.config || {};
  if (source.kind === 'hn_search') {
    const floor = cfg.min_points ? ` · ${cfg.min_points}+ points` : '';
    return `Hacker News · "${cfg.query || ''}"${floor}`;
  }
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
