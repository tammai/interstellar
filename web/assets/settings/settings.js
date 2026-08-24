// Settings: its own document at /settings/, and the six screens behind the tab
// strip (§6, as amended by rev 5 decision #20).
//
// It used to be a gear popover over the star map with a sheet sliding in from the
// right. Two things were wrong with that. The sheet covered the sky it was
// configuring, and every screen was two clicks away behind a menu that listed
// them one line each -- so the shape of the configuration was never visible, only
// recitable. Tabs state all six at once and cost one click. The move also takes
// this file off the star map's critical path entirely: app.js no longer imports
// 1700 lines to render a canvas.
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

// Tab order is deliberate: it runs from what you change most often to what you
// only read. Topics and Sources are the daily edits, Model and Schedule the rare
// ones, and Signal quality is a measurement rather than a setting at all.
const TABS = [
  ['topics', 'Topics'],
  ['sources', 'Sources'],
  ['model', 'Model'],
  ['schedule', 'Schedule'],
  ['weekly', 'Weekly'],
  ['quality', 'Signal quality'],
];

const DEFAULT_TAB = 'topics';

// The screen is addressed by the fragment, so the browser's Back button steps
// between tabs, a tab can be linked to, and a reload lands where you were. It is
// also how the sky's banner opens this page straight on Model.
function screenFromHash() {
  const want = location.hash.replace(/^#/, '');
  return TABS.some(([key]) => key === want) ? want : DEFAULT_TAB;
}

class Settings {
  constructor() {
    this.status = null;
    this.settings = null;
    this.quality = null;    // last /api/topics/stats response
    this.qualityDays = 7;
    this.screen = null;
    this.modelSig = null;   // signature of the last Model render, see refreshModel
    this.tabButtons = new Map();

    this.screenEl = el('screen');
    this.buildTabs();

    addEventListener('hashchange', () => this.showScreen(screenFromHash()));
  }

  // ---------- shell ----------

  buildTabs() {
    const strip = el('tabs');
    for (const [key, label] of TABS) {
      // A real link, not a button: middle-click and copy-link work, and the
      // fragment it points at is the same thing showScreen reads back.
      const tab = document.createElement('a');
      tab.className = 'tab';
      tab.href = `#${key}`;
      tab.id = `tab-${key}`;
      tab.setAttribute('role', 'tab');
      tab.setAttribute('aria-selected', 'false');
      tab.setAttribute('aria-controls', 'screen');
      tab.tabIndex = -1;          // roving; the selected tab gets 0 in markSelected

      const text = document.createElement('span');
      text.textContent = label;
      tab.append(text);

      tab.addEventListener('keydown', (e) => this.onTabKey(e, key));
      strip.append(tab);
      this.tabButtons.set(key, tab);
    }
  }

  // Arrow keys move between tabs, per the tablist pattern: with roving tabindex
  // Tab alone reaches the strip once and then leaves for the screen, which is the
  // point -- otherwise getting from the header to a topic definition is six tabs
  // of chrome.
  onTabKey(e, key) {
    const keys = TABS.map(([k]) => k);
    const i = keys.indexOf(key);
    let next = null;
    if (e.key === 'ArrowRight') next = keys[(i + 1) % keys.length];
    else if (e.key === 'ArrowLeft') next = keys[(i - 1 + keys.length) % keys.length];
    else if (e.key === 'Home') next = keys[0];
    else if (e.key === 'End') next = keys[keys.length - 1];
    if (!next) return;
    e.preventDefault();
    location.hash = `#${next}`;
    this.tabButtons.get(next)?.focus();
  }

  markSelected(screen) {
    for (const [key, tab] of this.tabButtons) {
      const on = key === screen;
      tab.classList.toggle('on', on);
      tab.setAttribute('aria-selected', String(on));
      tab.tabIndex = on ? 0 : -1;
    }
    this.screenEl.setAttribute('aria-labelledby', `tab-${screen}`);
  }

  // ---------- status ----------

  setStatus(status) {
    this.status = status;
    this.renderRunState();
    // A tab whose screen cannot work says so on the tab itself. The popover
    // carried this as a "needs attention" row; here it has to survive being on a
    // different screen, because nothing else would ever bring you back to Model.
    //
    // Two different questions, one dot: readiness is whether the configuration
    // COULD work, failures are whether it DID. A keyless provider always passes
    // the first and can fail every call of the second, which is the case this dot
    // used to be blind to.
    const broken = status && (!status.ready || (status.failures || []).length > 0);
    this.tabButtons.get('model')?.classList.toggle('attention', !!broken);
    if (this.screen === 'model') this.refreshModel();
  }

  // The header line: what the popover's footer note said, minus the "no restart"
  // reassurance, which now lives on the screens that actually change a schedule.
  renderRunState() {
    const box = el('runState');
    const s = this.status;
    if (!s) { box.textContent = ''; return }

    box.replaceChildren();
    // Shortened to match the sky's header exactly, hover text included. The two are
    // separate implementations on purpose (app.js imports nothing from here), so the
    // only thing keeping them one voice is that a change to either is made to both.
    const add = (text, title, cls) => {
      const span = document.createElement('span');
      if (cls) span.className = cls;
      span.textContent = text;
      span.title = title;
      box.append(span);
    };

    const running = s.run?.stage && s.run.stage !== 'idle';
    if (running) add(`${s.run.stage}…`, `Run in progress: ${s.run.stage}`, 'rs-run');
    if (s.calls_today != null) {
      add(`${s.calls_today} / ${s.call_cap_day} calls`, 'LLM calls today, against the daily cap');
    }
    if (s.ready === false) {
      add('not ready', 'not ready to run — a role has no model or no key', 'rs-warn');
    }
    // Named, not counted: "1 role failing" sends you looking, "Ask failing" tells
    // you where. The provider's own words are on the Model screen, beside the
    // field that changes them.
    const failing = (s.failures || []).map((f) => f.role === 'chat' ? 'Ask' : f.role);
    if (failing.length) {
      add(`${failing.join(', ')} failing`,
        'every call for this role failed on its most recent day — see the Model screen', 'rs-warn');
    }
  }

  // The Model screen draws nothing from `status` -- it was re-rendered on every
  // poll purely to keep the per-provider key badges live. But renderModel goes
  // through screenFrame, which replaceChildren()s the screen. So every status poll
  // threw away the scroll position: every 20s while idle, every 1.5s during a run.
  //
  // The badges still need to go green the moment a key resolves, so the refresh
  // stays -- it just has to earn the rebuild.
  async refreshModel() {
    // Never rebuild under the user's hands. A poll landing mid-edit would take
    // the field, its caret and whatever was typed into it.
    const active = document.activeElement;
    if (active && this.screenEl.contains(active) && /^(INPUT|SELECT|TEXTAREA)$/.test(active.tagName)) {
      return;
    }

    await this.loadSettings();
    if (this.modelSignature() === this.modelSig) return;   // identical DOM; leave it alone

    await this.rerenderCurrent();
  }

  // Everything renderModel draws -- which now includes the failing-role notes, so
  // they are in here too. Neither half carries a timestamp or any other field
  // that moves on its own, so an unchanged signature really does mean an
  // identical render.
  modelSignature() {
    return JSON.stringify([this.settings ?? null, this.status?.failures ?? []]);
  }

  async loadSettings() {
    try {
      const res = await fetch('/api/settings');
      if (res.ok) this.settings = await res.json();
    } catch (err) {
      console.error(err);
    }
  }

  // Something on a screen changed the instance. Nothing here has to repaint a
  // sky -- that is a different document, and it reloads when you go back to it --
  // but the header's call count and readiness are stale the moment a save lands.
  changed() {
    loadStatus();
  }

  // ---------- screens ----------

  async showScreen(screen) {
    if (screen === this.screen) return;
    this.screen = screen;
    this.markSelected(screen);
    // A fresh screen starts at the top, and the reader's focus goes with it:
    // without this, changing tab leaves the caret wherever it was in the strip
    // and a screen reader announces nothing.
    this.screenEl.scrollTop = 0;
    await this.renderScreen(screen);
  }

  renderScreen(screen) {
    switch (screen) {
      case 'topics': return this.renderTopics();
      case 'sources': return this.renderSources();
      case 'model': return this.renderModel();
      case 'schedule': return this.renderSchedule();
      case 'quality': return this.renderQuality();
      case 'weekly': return this.renderWeekly();
    }
    return Promise.resolve();
  }

  // Re-render the open screen in place, keeping the reader where they were.
  //
  // Every render goes through screenFrame, which replaceChildren()s the screen, so
  // a rebuild always lands back at the top. Adding a topic to a list you had
  // scrolled to the bottom of threw you back to the first one, which is precisely
  // the moment you least want to lose your place.
  //
  // Restoring after the await matters: these renders fetch before they append, so
  // scrollTop set any earlier lands on an empty element and does not stick.
  async rerenderCurrent() {
    if (!this.screen) return;
    const prev = this.screenEl.scrollTop;
    await this.renderScreen(this.screen);
    this.screenEl.scrollTop = prev;
  }

  // Every screen gets the same frame: a title, an optional line of orientation,
  // and a column that stops widening at a readable measure. There is no close
  // button and no sticky head -- the tab strip above is always visible and always
  // says which screen this is, which is the job the sheet's sticky title was
  // doing.
  screenFrame(title, subtitle) {
    const inner = document.createElement('div');
    inner.className = 'screen-inner';

    const h = document.createElement('h1');
    h.className = 'screen-title';
    h.textContent = title;
    inner.append(h);

    if (subtitle) {
      const p = document.createElement('p');
      p.className = 'screen-sub';
      p.textContent = subtitle;
      inner.append(p);
    }

    this.screenEl.replaceChildren(inner);
    return inner;
  }

  // A labelled switch. Returns the element; the caller supplies what flipping it
  // means. Kept here rather than duplicated per screen because topics and sources
  // must not drift apart on something this visible.
  // A delivery channel and its settings in one card, with the switch in the title.
  //
  // The switch owns the fields below it: off disables them rather than hiding them, so
  // the card still says what the channel WOULD use -- which is the thing you want to
  // read when working out why nothing arrived. Flipping it enables them in place; a
  // rebuild here would go through screenFrame and replaceChildren() away the very
  // confirmation the flip produced, the trap the Model screen hit twice.
  //
  // The trade this accepts: the fields cannot be filled in before the channel is
  // switched on. "Send a test" still works either way, so the verify-before-you-commit
  // path is switch on, fill in, test -- one extra click, in exchange for a card whose
  // state is never ambiguous.
  channelCard(title, label, on, fields, save) {
    const card = document.createElement('div');
    card.className = 'card channel';

    const head = document.createElement('div');
    head.className = 'channel-head';

    const heading = document.createElement('span');
    heading.className = 'channel-title';
    heading.textContent = title;

    const setEnabled = (enabled) => {
      card.classList.toggle('off', !enabled);
      for (const el of fields.querySelectorAll('input, select, textarea')) {
        el.disabled = !enabled;
      }
    };

    const toggle = this.switchControl(on, label, async (next) => {
      const ok = await save(next);
      if (ok) setEnabled(next);
      return ok;
    });

    head.append(heading, toggle);

    const bodyEl = document.createElement('div');
    bodyEl.className = 'channel-body';
    bodyEl.append(fields);

    card.append(head, bodyEl);
    setEnabled(on);
    return card;
  }

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
    const body = this.screenFrame('Topics',
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
      if (created) { this.rerenderCurrent(); this.changed() }
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
      if (ok) { topic.active = next; this.changed() }
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
      if (ok) this.changed();
    };

    const del = document.createElement('button');
    del.className = 'btn-danger';
    del.textContent = 'Delete';
    del.onclick = async () => {
      // Deleting a topic drops its matches and signals with it, so confirm.
      if (!confirm(`Delete "${topic.title}"? Its signals go too.`)) return;
      await fetch(`/api/topics/${topic.id}`, { method: 'DELETE' });
      this.rerenderCurrent(); this.changed();
    };

    // Suggestions land IN the card, and the screen is never rebuilt around them.
    // Two reasons, both already load-bearing on this screen: rerenderCurrent()
    // would replaceChildren() away the result it was asked to show, and it would
    // also discard whatever the owner has typed into a sibling card's definition
    // -- which is why the active toggle above deliberately does not re-render
    // either.
    const suggest = document.createElement('button');
    suggest.className = 'btn-quiet';
    suggest.textContent = 'Suggest sources';
    suggest.title = 'Propose feeds and Hacker News keywords for this topic, from its definition';
    let panel = null;
    suggest.onclick = async () => {
      if (topic.needs_setup) {
        panel = this.suggestPanel(card, panel);
        panel.replaceChildren(noteLine(
          'Write a definition first — suggestions come from what you mean by the topic, not from its name.',
          'bad'));
        return;
      }
      suggest.disabled = true;
      suggest.textContent = 'Looking…';
      panel = this.suggestPanel(card, panel);
      panel.replaceChildren(noteLine('Asking the model, then fetching every feed it proposes…'));

      const res = await fetchJSON(`/api/topics/${topic.id}/sources/suggest`, { method: 'POST' });
      suggest.disabled = false;
      suggest.textContent = 'Suggest sources';

      if (!res) {
        panel.replaceChildren(noteLine('Could not get suggestions. Check the synthesizer model on the Model screen.', 'bad'));
        return;
      }
      this.renderSuggestions(panel, res);
      if (res.added > 0) this.changed();
    };

    actions.append(suggest, toggle, save, del);
    meta.append(status, actions);
    card.append(title, def, meta);
    return card;
  }

  // suggestPanel is the card's own result area, created once and reused so a
  // second press replaces the first result rather than stacking under it.
  suggestPanel(card, existing) {
    if (existing) return existing;
    const panel = document.createElement('div');
    panel.className = 'suggestions';
    card.append(panel);
    return panel;
  }

  renderSuggestions(panel, res) {
    panel.replaceChildren();

    const feeds = res.feeds || [];
    const keywords = res.keywords || [];
    if (!feeds.length && !keywords.length) {
      panel.append(noteLine('The model proposed nothing for this definition.'));
      return;
    }

    if (res.added > 0) {
      // Lead with what to do next. A row was written and it is switched off, so
      // the feature has not finished until the owner goes and looks at it.
      panel.append(noteLine(
        `${res.added} feed${res.added === 1 ? '' : 's'} added, switched off. ` +
        'Check them on the Sources screen and switch on the ones you want.', 'ok'));
    }

    for (const f of feeds) {
      panel.append(suggestionRow(f));
    }

    for (const k of keywords) {
      const row = suggestionRow(k);
      // A keyword is offered, not added: there is one Hacker News row and it is
      // live, so writing to it would put a guess into tomorrow's run unreviewed.
      // One click is the review.
      if (k.status === 'proposed') {
        const add = document.createElement('button');
        add.className = 'btn-quiet sg-add';
        add.textContent = 'Add keyword';
        add.onclick = async () => {
          add.disabled = true;
          const outcome = await this.addHNKeyword(k.query, k.min_points);
          add.textContent = outcome === 'added' ? 'Added'
            : outcome === 'already' ? 'Already watching'
            : outcome === 'no-row' ? 'No Hacker News source'
            : 'Failed';
          if (outcome === 'added') this.changed();
        };
        row.append(add);
      }
      panel.append(row);
    }
  }

  // The ONE place a Hacker News keyword is added, used by the Sources screen's
  // add field and by a suggestion's Add button.
  //
  // Two rules live here and both are easy to get wrong from a call site. A second
  // Hacker News row would undo migration 007 one keyword at a time -- source_count
  // counts DISTINCT source_id, so two HN rows make one story look like two
  // outlets -- so the keyword joins the row that is already there. And PATCH
  // REPLACES config wholesale, so the write has to carry the rest of it forward:
  // sending only {queries} drops firehose_min_points, which silently switches off
  // the keyword-less pass that exists to catch the story no keyword anticipated.
  // Nothing would look broken; the sky would just quietly stop finding those.
  //
  // `query` is the one key deliberately dropped: hnQueries has already folded the
  // legacy single-keyword shape into the list, so keeping it would watch that
  // keyword twice.
  async addHNKeyword(query, minPoints = 0) {
    const text = String(query || '').trim();
    if (!text) return 'rejected';

    const sources = await fetchJSON('/api/sources') || [];
    const hn = sources.find((s) => s.kind === 'hn_search');
    if (!hn) return 'no-row';

    const cfg = hn.config || {};
    const queries = hnQueries(cfg);
    if (queries.some((q) => q.query.toLowerCase() === text.toLowerCase())) return 'already';
    queries.push(minPoints > 0 ? { query: text, min_points: minPoints } : { query: text });

    const config = { ...cfg, queries };
    delete config.query;

    const ok = await fetchJSON(`/api/sources/${hn.id}`, {
      method: 'PATCH', body: JSON.stringify({ config }),
    });
    return ok ? 'added' : 'failed';
  }

  // ---------- sources ----------

  async renderSources() {
    const body = this.screenFrame('Sources',
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
    // Adding a Hacker News keyword is not adding a source, so the name field goes
    // away for it: the keyword joins the existing Hacker News row.
    const syncAdd = () => {
      const hn = kind.value === 'hn_search' && !!existingHN();
      value.placeholder = kind.value === 'hn_search' ? 'Search keyword'
        : kind.value === 'youtube_rss' ? 'Channel ID (UC...)' : 'Feed URL';
      name.hidden = hn;
      create.textContent = hn ? 'Add keyword' : 'Add source';
    };
    const existingHN = () => sources.find((s) => s.kind === 'hn_search');
    kind.onchange = syncAdd;

    const create = document.createElement('button');
    create.className = 'btn-primary';
    create.textContent = 'Add source';
    create.onclick = async () => {
      const text = value.value.trim();
      if (!text) return;

      // A keyword joins the Hacker News row that is already there; see
      // addHNKeyword for why that matters and what else the write has to carry.
      if (kind.value === 'hn_search' && existingHN()) {
        const outcome = await this.addHNKeyword(text);
        if (outcome === 'already') { value.value = ''; return }
        if (outcome === 'added') { this.rerenderCurrent(); this.changed() }
        return;
      }

      const config = kind.value === 'hn_search' ? { queries: [{ query: text }] }
        : kind.value === 'youtube_rss' ? { channel_id: text }
        : { url: text };
      const ok = await fetchJSON('/api/sources', {
        method: 'POST',
        body: JSON.stringify({ kind: kind.value, name: name.value, config }),
      });
      if (ok) { this.rerenderCurrent(); this.changed() }
    };
    syncAdd();

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
    section.className = 'screen-section transfer';

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
      this.changed();
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
    section.className = 'screen-section transfer';

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
      this.changed();
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
    // A suggested row arrives switched off, so without this it is a paused feed
    // nobody remembers adding. The tag is the difference between "why is this
    // here" and "right, I asked for that".
    if (source.config?.suggested_for) name.append(quietTag('suggested'));

    const detail = document.createElement('div');
    detail.className = 'card-detail';
    detail.textContent = describeSource(source);

    const meta = document.createElement('div');
    meta.className = 'card-detail dim';
    meta.textContent = source.last_run_at
      ? `${source.item_count ?? 0} items · last run ${formatWhen(source.last_run_at)}`
      : `${source.item_count ?? 0} items · never completed a run`;
    if (!source.last_run_at) meta.classList.add('warn');

    // Hidden until something goes wrong; settings.css's blanket [hidden] rule is what
    // keeps it out of the flow.
    const failure = document.createElement('div');
    failure.className = 'test-result bad';
    failure.hidden = true;

    main.append(name, detail, meta, failure);

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
    // The response is checked rather than discarded, and the failure is reported
    // into the card rather than swallowed. A delete that 500s used to look exactly
    // like one that worked -- the screen re-rendered and the row was simply still
    // there, with nothing on the page saying why.
    del.onclick = async () => {
      if (!confirm(`Delete "${source.name}"?`)) return;
      const ok = await fetchJSON(`/api/sources/${source.id}`, { method: 'DELETE' });
      if (!ok) {
        // Not rerenderCurrent(): it would replace the card this just wrote into.
        failure.textContent = 'Could not delete this source. It is still here.';
        failure.hidden = false;
        return;
      }
      this.rerenderCurrent(); this.changed();
    };

    actions.append(toggle, editBtn, del);
    // The editor is built on first click, not here. Building one per card meant 68
    // hidden forms and 272 form controls in one scrolling screen — controls are
    // among the most expensive elements to lay out, and it was the screen's scroll
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

    // Hacker News is one source carrying many keywords, so it gets a list editor
    // instead of the single location field. Each keyword keeps its own points
    // floor: a broad term needs a higher bar than a niche one, and one shared
    // threshold either floods the sky or silences half the list.
    // The keyword-less pass. Its own control rather than a keyword row, because
    // it is not a keyword: it is "everything above this score, whatever it is
    // about", and the classifier decides. Deliberately set HIGH while the keyword
    // floor below is set LOW -- they cover opposite failures.
    const firehose = document.createElement('div');
    firehose.className = 'firehose';

    const fireLabel = document.createElement('label');
    fireLabel.className = 'field-label';
    fireLabel.textContent = 'Also take everything scoring at least';

    const firePoints = document.createElement('input');
    firePoints.className = 'field';
    firePoints.type = 'number';
    firePoints.min = '0';
    firePoints.placeholder = 'off';
    firePoints.setAttribute('aria-label', 'Firehose minimum points, blank for off');

    const fireNote = document.createElement('p');
    fireNote.className = 'note';
    fireNote.textContent = 'points, with no keyword at all — catches the big story ' +
      'no keyword anticipated. Leave blank to switch it off.';

    firehose.append(fireLabel, firePoints, fireNote);

    const floorLabel = document.createElement('label');
    floorLabel.className = 'field-label';
    floorLabel.textContent = 'Keywords';

    const keywords = document.createElement('div');
    keywords.className = 'keywords';

    const addKeyword = (q = { query: '', min_points: 0 }) => {
      const row = document.createElement('div');
      row.className = 'keyword-row';

      const query = document.createElement('input');
      query.className = 'field';
      query.placeholder = 'Keyword';
      query.value = q.query || '';
      query.setAttribute('aria-label', 'Search keyword');

      const floor = document.createElement('input');
      floor.className = 'field';
      floor.type = 'number';
      floor.min = '0';
      floor.placeholder = 'Min points';
      floor.value = q.min_points ? String(q.min_points) : '';
      floor.setAttribute('aria-label', `Minimum points for ${q.query || 'this keyword'}`);

      const remove = document.createElement('button');
      remove.className = 'btn-quiet';
      remove.type = 'button';
      remove.textContent = 'Remove';
      remove.setAttribute('aria-label', `Remove keyword ${q.query || ''}`);
      remove.onclick = () => row.remove();

      row.append(query, floor, remove);
      keywords.append(row);
      return query;
    };

    const addBtn = document.createElement('button');
    addBtn.className = 'btn-quiet';
    addBtn.type = 'button';
    addBtn.textContent = 'Add keyword';
    addBtn.onclick = () => addKeyword().focus();

    const syncKind = () => {
      const hn = kind.value === 'hn_search';
      location.placeholder = kind.value === 'youtube_rss' ? 'Channel ID (UC...)' : 'Feed URL';
      location.hidden = hn;
      keywords.hidden = !hn;
      addBtn.hidden = !hn;
      firehose.hidden = !hn;
      floorLabel.hidden = !hn;
    };
    kind.onchange = syncKind;

    const fill = () => {
      const cfg = source.config || {};
      name.value = source.name;
      kind.value = source.kind;
      location.value = cfg.url || cfg.channel_id || '';
      keywords.replaceChildren();
      for (const q of hnQueries(cfg)) addKeyword(q);
      firePoints.value = cfg.firehose_min_points ? String(cfg.firehose_min_points) : '';
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
      let config;
      if (kind.value === 'hn_search') {
        // Read the rows rather than a model: the rows ARE the state, so a keyword
        // typed and not blurred still saves. Blank ones are dropped -- an empty
        // query asks Algolia for the entire front page.
        const queries = [];
        for (const row of keywords.querySelectorAll('.keyword-row')) {
          const [q, floor] = row.querySelectorAll('input');
          const text = q.value.trim();
          if (!text) continue;
          const entry = { query: text };
          // Only send min_points when it has one, so clearing the box falls back
          // to the global setting rather than pinning the keyword at zero.
          if (floor.value !== '') entry.min_points = Number(floor.value);
          queries.push(entry);
        }
        config = { queries };
        // Blank means off, and must be sent as absent rather than 0 so the shape
        // stays the one the Go side treats as "no firehose".
        if (firePoints.value !== '' && Number(firePoints.value) > 0) {
          config.firehose_min_points = Number(firePoints.value);
        }
        // The source-level floor every keyword without its own bar falls back to.
        // Preserved rather than rebuilt, so editing keywords does not silently
        // reset it to the global setting.
        if (source.config && source.config.min_points) {
          config.min_points = source.config.min_points;
        }
      } else if (kind.value === 'youtube_rss') {
        config = { channel_id: value };
      } else {
        config = { url: value };
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
      this.changed();
    };

    pair.append(name, kind);
    rowActions.append(cancel, save);
    box.append(pair, location, firehose, floorLabel, keywords, addBtn, rowActions);
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

    const body = this.screenFrame('Model',
      'Either role can run on any provider. Changes take effect on the next run — nothing to restart.');

    // Providers: three columns, mark then endpoint then state.
    //
    // The mark replaces the name because a row of six identical text labels is
    // six things to read where a glyph is one thing to recognise -- and the name
    // is not lost, it is the mark's accessible label and its tooltip. For a
    // provider the owner added, providerMark falls back to a monogram of that
    // same name, so an unrecognised entry still identifies itself.
    //
    // The badge says whether the key actually resolves. The registry stores the
    // variable NAME; the value never leaves the environment, which is why this
    // shows a state rather than a secret.
    const provSection = section(body, 'Providers');
    for (const p of s.providers) {
      const row = document.createElement('div');
      row.className = 'card prov-row';

      const body2 = document.createElement('div');
      body2.className = 'prov-body';

      const url = document.createElement('div');
      url.className = 'prov-url';
      url.textContent = p.base_url;

      const key = document.createElement('div');
      key.className = 'prov-key';
      if (p.keyless) {
        key.textContent = 'no key required';
      } else {
        // The variable name is what the owner has to set on the container, so it
        // is typed the way they will type it, not woven into a sentence.
        const code = document.createElement('code');
        code.textContent = p.api_key_env;
        key.append(code);
      }

      body2.append(url, key);

      const badge = document.createElement('span');
      badge.className = 'badge ' + (p.resolved ? 'ok' : 'bad');
      badge.textContent = p.keyless ? 'ready' : (p.resolved ? 'key set' : 'key missing');

      row.append(providerMark(p.name), body2, badge);
      provSection.append(row);
    }

    const provNote = document.createElement('p');
    provNote.className = 'note';
    provNote.textContent =
      'Keys are read from the environment by name and never stored. To add one, set the variable on the container and restart it — everything else here changes live.';
    provSection.append(provNote);

    // Roles. All three of them -- chat included, which it was not.
    //
    // The Ask screen has always had its own role, its own settings key and its own
    // entry in the test endpoint; it just had no section here, so `chat_model` was
    // reachable only by PUTting JSON at the API by hand. An owner whose credit ran
    // out at the provider Phase 0 seeds could not move the Ask screen off it from
    // the UI at all. §3.1 says either role can be switched in settings without
    // touching code; a role the settings screen does not draw fails that.
    for (const [role, title, current, fallback, hint] of [
      ['classifier', 'Classifier', s.classifier_model, s.classifier_fallback,
        'Scores every item against every topic. This is where most of the cost goes, so a cheap model earns its place here.'],
      ['synthesizer', 'Synthesizer', s.synthesizer_model, s.synthesizer_fallback,
        'Groups matched items into stories. The hardest judgment in the pipeline.'],
      // Named for the screen it serves, not for the settings key behind it: the
      // owner knows this as Ask. The key stays `chat_model`, which is what the
      // note below says so the two can be connected.
      ['chat', 'Ask', s.chat_model, s.chat_fallback,
        'Answers questions on the Ask screen, in prose, from your own signals. ' +
        'Budgeted separately from the runs and deliberately outside the readiness check — ' +
        'clearing this stops Ask working, and must never stop the morning run.'],
    ]) {
      const roleSection = section(body, title);

      // No provider mark here, though it was tried: the field two lines below
      // already says `ollama/glm-5.2` in full, so a mark beside the hint was a
      // second answer to a question nothing had asked, floating next to a
      // paragraph with no label of its own.
      const note = document.createElement('p');
      note.className = 'note';
      note.textContent = hint;
      roleSection.append(note);

      // What actually happened, if it went wrong. The provider's own words, next
      // to the field that changes them -- the whole failure mode this addresses
      // is that the answer lived only in the database while the screen said the
      // configuration was fine.
      const failure = (this.status?.failures || []).find((f) => f.role === role);
      if (failure) {
        const bad = document.createElement('div');
        bad.className = 'test-result bad';
        bad.textContent =
          `Every call failed on ${failure.run_date} (${failure.failed}) — ` +
          `${failure.ref}: ${failure.error}`;
        roleSection.append(bad);
      }

      // Labels, not placeholders. A placeholder is gone the moment the field has
      // a value, so a populated fallback chain was two identical monospace boxes
      // with nothing saying which was which.
      const input = document.createElement('input');
      input.className = 'field mono';
      input.value = current;
      input.placeholder = 'provider/model';
      input.id = `${role}-model`;

      const fb = document.createElement('input');
      fb.className = 'field mono';
      fb.value = (fallback || []).join(', ');
      fb.placeholder = 'none';
      fb.id = `${role}-fallback`;

      roleSection.append(
        labelledField(input, 'Model', 'A provider from the list above, then a model it serves.'),
        labelledField(fb, 'Fallback chain',
          'Tried in order when the model above fails. Comma separated, and may cross providers — every attempt is logged separately.'),
      );

      // Chat's daily allowance belongs here rather than on Schedule, for the same
      // reason it is a separate budget at all: a question is not a run. Schedule's
      // two caps govern the morning pass; this one governs an evening of curiosity,
      // and keeping them apart is what stops one starving the other.
      let chatCap = null;
      if (role === 'chat') {
        chatCap = document.createElement('input');
        chatCap.className = 'field';
        chatCap.type = 'number';
        chatCap.min = '0';
        chatCap.value = s.max_chat_calls_per_day;
        chatCap.id = 'chat-cap';
        roleSection.append(labelledField(chatCap, 'Max questions per day',
          'Its own allowance, separate from the run caps on the Schedule screen, so questions cannot eat what tomorrow morning needs.'));
      }

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
        if (chatCap) payload.max_chat_calls_per_day = Number(chatCap.value);
        const res = await fetch('/api/settings', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
        if (res.ok) {
          // Chat is not a run, and telling someone their new Ask model "applies
          // on the next run" invites them to wait for 09:30 before asking
          // anything. It applies to the next question.
          result.textContent = role === 'chat'
            ? 'Saved. It applies to your next question.'
            : 'Saved. It applies on the next run.';
          result.className = 'test-result ok';

          // Adopt the saved state as what this screen already shows, before
          // changed() kicks off a status load. Otherwise the poll finds settings
          // that differ from the last render, rebuilds the screen, and wipes the
          // confirmation within milliseconds of the reader seeing it -- and the
          // rebuild is redundant anyway, since the fields already hold exactly
          // what was just saved.
          await this.loadSettings();
          this.modelSig = this.modelSignature();

          this.changed();
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
      roleSection.append(actions, result);
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

    const body = this.screenFrame('Schedule',
      `Times are local to the container (${s.timezone}). The aim is a fresh sky before you look at it.`);

    // Two groups of four columns. The split is not cosmetic: the first group is
    // everything about the run itself -- when it happens and what it may spend --
    // and those four are read together, because moving a time and raising a cap
    // are the same decision about how much work happens each morning. The rest
    // tune what gets gathered and how far back the sky reaches, and are touched
    // once and then rarely.
    //
    // Help text is written short deliberately. In a quarter-width column a
    // sentence of explanation becomes eight ragged lines, and the field it
    // belongs to disappears under it.
    const groups = [
      ['The run', [
        ['schedule_ingest', 'Ingest', s.schedule_ingest, 'time', 'When feeds are fetched.'],
        ['schedule_synthesize', 'Synthesize', s.schedule_synthesize, 'time', 'When they are classified and composed.'],
        ['max_calls_per_run', 'Max LLM calls per run', s.max_calls_per_run, 'number', 'A runaway run stops here.'],
        ['max_calls_per_day', 'Max LLM calls per day', s.max_calls_per_day, 'number', 'Survives a restart.'],
      ]],
      ['What it gathers', [
        ['sky_days', 'Days of sky', s.sky_days, 'number', 'How far back the star map reaches.'],
        ['hn_min_points', 'HN minimum points', s.hn_min_points, 'number', 'Below this, Hacker News is noise.'],
        ['transcripts_per_run', 'Videos transcribed per run', s.transcripts_per_run, 'number',
          'Captions summarized, so a video is judged on what was said. 0 turns it off.'],
      ]],
    ];

    // One node holding every input, so the save below still reads them all with a
    // single querySelectorAll no matter how the groups are arranged.
    const grid = document.createElement('div');

    for (const [title, fields] of groups) {
      const wrap = section(grid, title);
      const row = document.createElement('div');
      row.className = 'grid quad';

      for (const [key, label, value, type, help] of fields) {
        const cell = document.createElement('label');
        cell.className = 'field-wrap';

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

        cell.append(l, input, h);
        row.append(cell);
      }
      wrap.append(row);
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
        this.changed();
      } else {
        const err = await res.json().catch(() => ({}));
        result.textContent = err.error || 'Could not save';
        result.className = 'test-result bad';
      }
    };

    // "Run now" used to close the sheet, because the sheet was covering the sky
    // it had just told to refresh. Nothing is covered here, so the button reports
    // where it stands instead: this page has no stars to show, and bouncing the
    // reader back to the star map to find out whether the run started is worse
    // than one line of text.
    const now = document.createElement('button');
    now.className = 'btn-quiet';
    now.textContent = 'Run now';
    now.onclick = async () => {
      now.disabled = true;
      result.className = 'test-result';
      result.textContent = 'Starting…';
      const res = await fetch('/api/refresh', { method: 'POST' }).catch(() => null);
      now.disabled = false;
      if (!res) {
        result.className = 'test-result bad';
        result.textContent = 'Could not reach the server.';
        return;
      }
      result.className = 'test-result ok';
      // 409 is not a failure: it means a run is already going, which is the state
      // the button was asking for.
      result.textContent = res.status === 409
        ? 'A run is already going. Watch it in the header.'
        : 'Started. Watch it in the header, and the sky when it finishes.';
      this.changed();
    };

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
    const body = this.screenFrame('Signal quality',
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

    await this.renderModelReliability(body);
    await this.renderHistory(body);
  }

  // Per-model reliability, directly under the per-topic hit rate.
  //
  // It goes here rather than on the Model screen because it is the other
  // explanation for the table above it. A model that answers one item of a
  // thirty-item batch, with valid JSON and a 200, makes every topic read as a
  // definition drawn too tight -- same numbers, wrong cause, and nothing in the
  // hit-rate table can tell them apart. Reading them a screen away from each
  // other is how that went unnoticed for days.
  //
  // The Model screen stays settings-only for a second reason: its render is
  // guarded by a signature check and refuses to rebuild while a field has focus,
  // and a measurement that refreshes on a poll has no business inside that.
  async renderModelReliability(body) {
    // Same window as the hit rate above, deliberately: the two numbers are being
    // compared, so a picker that moved one and not the other would invite exactly
    // the wrong reading.
    const data = await fetchJSON(`/api/models/stats?days=${this.qualityDays}`);
    if (!data) return;

    const wrap = section(body, 'Model reliability');

    const rows = data.rows || [];
    if (!rows.length) {
      const none = document.createElement('p');
      none.className = 'note';
      none.textContent = 'No model calls in this window yet.';
      wrap.append(none);
      return;
    }

    const table = document.createElement('div');
    table.className = 'quality-table';
    table.setAttribute('role', 'table');
    table.setAttribute('aria-label',
      `Per-model reliability over ${data.days} ${data.days === 1 ? 'day' : 'days'}`);

    const head = document.createElement('div');
    head.className = 'qt-row qt-head';
    head.setAttribute('role', 'row');
    for (const [label, cls] of [['Model', 'qt-topic'], ['Calls', 'qt-n'],
                                ['Parsed', 'qt-rate'], ['Answered', 'qt-rate'], ['Repairs', 'qt-n']]) {
      const cell = document.createElement('span');
      cell.className = cls;
      cell.setAttribute('role', 'columnheader');
      cell.textContent = label;
      head.append(cell);
    }
    table.append(head);

    for (const row of rows) {
      table.append(modelRow(row));
    }
    wrap.append(table);

    const note = document.createElement('p');
    note.className = 'note';
    note.textContent = data.ready
      ? 'Parsed is how often a reply yielded usable JSON. Answered is how many of the items a call asked about came back with a verdict — ' +
        'a model can score 100% parsed and still drop most of a batch, which is the failure this column exists to catch. ' +
        'Both are blank for calls that are not a list of anything: a chat answer, a transcript summary, a test call.'
      : 'Nothing in this window carried a measurable reply yet. Parsed and Answered fill in once a classify or compose pass runs.';
    wrap.append(note);
  }

  // Constellation history, on the same screen as the hit rate.
  //
  // Its own tab would be a seventh screen for two tables that answer the same
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

  // The weekly round-up, and getting it off the instance.
  //
  // Delivery is off by default and this screen says so plainly: spec decision #10
  // made the web UI the front door on purpose, so email exists here to be switched
  // on if "I forgot to open it" ever actually happens.
  async renderWeekly() {
    await this.loadSettings();
    const s = this.settings;
    if (!s) return;

    const body = this.screenFrame('Weekly round-up',
      'One written pass over the week\'s signals. Reading it here needs nothing configured; ' +
      'having it emailed needs a relay and an API key in the environment.');

    const data = await fetchJSON('/api/weekly');

    // --- the round-up itself ---
    const latest = section(body, 'Latest');
    if (data?.latest) {
      const meta = document.createElement('p');
      meta.className = 'note';
      meta.style.marginTop = '0';
      const bits = [`${data.latest.week_start} → ${data.latest.week_end}`,
                    `${data.latest.signals} signals`,
                    `${data.latest.topics} topics`];
      if (data.latest.sent_at) bits.push(`sent via ${data.latest.sent_via}`);
      else if (data.latest.send_error) bits.push(`send failed: ${data.latest.send_error}`);
      meta.textContent = bits.join(' · ');

      const prose = document.createElement('p');
      prose.className = 'weekly-body';
      prose.textContent = data.latest.body;

      latest.append(meta, prose);
    } else {
      const none = document.createElement('p');
      none.className = 'note';
      none.textContent = 'None yet. One is written automatically on the day below, ' +
        'or press the button to write last week\'s now.';
      latest.append(none);
    }

    const runResult = document.createElement('div');
    runResult.className = 'test-result';

    const runNow = document.createElement('button');
    runNow.className = 'btn-quiet';
    runNow.textContent = 'Write last week now';
    runNow.onclick = async () => {
      runNow.disabled = true;
      runResult.className = 'test-result';
      runResult.textContent = 'Writing…';
      // deliver is deliberately not set: reading the prose must not mail anyone.
      const res = await fetchJSON('/api/weekly/run', { method: 'POST', body: '{}' });
      runNow.disabled = false;
      if (!res) {
        runResult.className = 'test-result bad';
        runResult.textContent = 'Could not write it. Check the synthesizer model.';
        return;
      }
      runResult.className = 'test-result ok';
      runResult.textContent = res.written
        ? `Written from ${res.signals} signals. Not sent — ${res.deliver_note || 'delivery not requested'}.`
        : `Nothing written: ${res.skipped || 'no signals that week'}.`;

      // Update the prose in place rather than calling rerenderCurrent(). A rebuild
      // goes through screenFrame, which replaceChildren()s the screen -- destroying
      // the confirmation set two lines above. Same trap the Model screen hit:
      // a save whose own refresh wipes the message telling you it worked.
      if (res.written) {
        const fresh = await fetchJSON('/api/weekly');
        const prose = body.querySelector('.weekly-body');
        if (fresh?.latest && prose) prose.textContent = fresh.latest.body;
      }
    };

    // Sends what is on screen. Deliberately a different endpoint from the button
    // above rather than the same one with deliver:true -- that one composes first, so
    // it would replace the prose being read with a different draft, bill a synthesizer
    // call to send something already written, and send nothing at all on a week it
    // decides not to rewrite.
    const sendNow = document.createElement('button');
    sendNow.className = 'btn-quiet';
    sendNow.textContent = 'Send it now';
    sendNow.disabled = !data?.latest;
    if (!data?.latest) sendNow.title = 'Nothing written yet';
    sendNow.onclick = async () => {
      const to = data.delivery?.email_enabled ? (data.delivery.email_to || 'the configured address') : null;
      const channels = [to && `email to ${to}`, data.delivery?.ping_enabled && 'the webhook']
        .filter(Boolean).join(' and ');
      if (!channels) {
        runResult.className = 'test-result bad';
        runResult.textContent = 'Both channels are off — switch one on below first.';
        return;
      }
      // Same guard as deleting a topic, for the same reason: it leaves the instance
      // and cannot be recalled.
      if (!confirm(`Send the round-up for ${data.latest.week_start} by ${channels}?`)) return;

      sendNow.disabled = true;
      runResult.className = 'test-result';
      runResult.textContent = 'Sending…';
      const res = await fetchJSON('/api/weekly/send', {
        method: 'POST', body: JSON.stringify({ week: data.latest.week_start }),
      });
      sendNow.disabled = false;
      if (!res) {
        runResult.className = 'test-result bad';
        runResult.textContent = 'Could not send it.';
        return;
      }
      const lines = [];
      if (res.email_sent) lines.push('Emailed.');
      else if (res.email_error) lines.push(`Email: ${res.email_error}`);
      if (res.ping_sent) lines.push('Webhook pinged.');
      else if (res.ping_error) lines.push(`Webhook: ${res.ping_error}`);
      if (!lines.length) lines.push(res.deliver_note || 'Nothing was sent.');
      runResult.className = 'test-result' + (res.delivered ? ' ok' : ' bad');
      runResult.textContent = lines.join(' · ');
    };

    const runActions = document.createElement('div');
    runActions.className = 'card-actions wide';
    runActions.append(runNow, sendNow);
    latest.append(runActions, runResult);

    // --- when ---
    const when = section(body, 'When');
    const grid = document.createElement('div');
    grid.className = 'grid';
    grid.append(
      field('schedule_weekly_day', 'Day', s.schedule_weekly_day, 'text',
        'A weekday name, or empty to stop writing it.'),
      field('schedule_weekly_time', 'Time', s.schedule_weekly_time, 'time',
        `Local to the container (${s.timezone}).`),
    );
    when.append(grid);

    // --- delivery ---
    const out = section(body, 'Send it somewhere');

    const note = document.createElement('p');
    note.className = 'note';
    note.style.marginTop = '0';
    note.textContent = 'Both off by default — the app is the front door. ' +
      'Switch a channel on to edit what it uses. ' +
      'The SMTP password is read from the named environment variable at send time and never stored.';
    out.append(note);

    // One card per channel, each owning its own fields. The switch sits in the card's
    // title because that is what it governs: with both toggles collected at the bottom
    // of one long form, nothing said which of the seven fields either of them turned
    // on, and the webhook URL sat in a grid of SMTP settings it has nothing to do with.
    //
    // Two columns inside the email card, fixed. These are pairs by nature -- the two
    // addresses, then the host and its port, then the username and the variable holding
    // its password -- and a grid that reflowed to three would break every pair apart.
    const emailFields = document.createElement('div');
    emailFields.className = 'grid duo';
    emailFields.append(
      field('digest_to', 'Email to', s.digest_to, 'text', 'Comma-separated for more than one.'),
      field('digest_from', 'Email from', s.digest_from, 'text',
        'A display name is fine: Interstellar <you@your-domain>. Must be a Resend-verified sender.'),
      field('smtp_host', 'SMTP host', s.smtp_host, 'text', 'smtp.resend.com'),
      field('smtp_port', 'SMTP port', s.smtp_port, 'number', '587 for STARTTLS, 465 for TLS.'),
      field('smtp_username', 'SMTP username', s.smtp_username, 'text', 'Resend expects "resend".'),
      field('smtp_password_env', 'Password env var', s.smtp_password_env, 'text',
        s.smtp_password_resolved ? 'Set in the environment.' : 'NOT set — nothing will send.'),
    );

    if (!s.smtp_password_resolved) {
      const warn = document.createElement('p');
      warn.className = 'test-result bad';
      warn.style.margin = '4px 0 10px';
      warn.textContent = `${s.smtp_password_env || 'The password variable'} is not set in the environment. ` +
        'Adding it needs a container restart, because the environment is fixed at boot.';
      emailFields.append(warn);
    }

    const webhookFields = document.createElement('div');
    webhookFields.className = 'grid duo';
    webhookFields.append(
      field('ping_url', 'Webhook URL', s.ping_url, 'text',
        'Posted a small JSON payload. The round-up itself stays in the app.'),
    );

    out.append(
      this.channelCard('Email', 'Email the round-up', s.digest_enabled, emailFields, async (next) => {
        const ok = await fetchJSON('/api/settings', {
          method: 'PUT', body: JSON.stringify({ digest_enabled: next }),
        });
        if (!ok) return false;
        this.settings = ok;
        return true;
      }),
      this.channelCard('Webhook', 'Ping a webhook', s.ping_enabled, webhookFields, async (next) => {
        const ok = await fetchJSON('/api/settings', {
          method: 'PUT', body: JSON.stringify({ ping_enabled: next }),
        });
        if (!ok) return false;
        this.settings = ok;
        return true;
      }),
    );

    const testResult = document.createElement('div');
    testResult.className = 'test-result';

    const save = document.createElement('button');
    save.className = 'btn-primary';
    save.textContent = 'Save';
    save.onclick = async () => {
      const payload = {};
      for (const input of body.querySelectorAll('input[data-key]')) {
        payload[input.dataset.key] = input.type === 'number' ? Number(input.value) : input.value;
      }
      const res = await fetch('/api/settings', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.ok) {
        testResult.className = 'test-result ok';
        testResult.textContent = 'Saved. The schedule was rebuilt — no restart needed.';
        this.changed();
      } else {
        const err = await res.json().catch(() => ({}));
        testResult.className = 'test-result bad';
        testResult.textContent = err.error || 'Could not save';
      }
    };

    // The same idea as the per-role model test: a setting you cannot verify is a
    // setting you do not trust, and waiting a week to find out is not verification.
    const test = document.createElement('button');
    test.className = 'btn-quiet';
    test.textContent = 'Send a test';
    test.onclick = async () => {
      test.disabled = true;
      testResult.className = 'test-result';
      testResult.textContent = 'Sending…';
      const res = await fetchJSON('/api/delivery/test', { method: 'POST', body: '{}' });
      test.disabled = false;
      if (!res) {
        testResult.className = 'test-result bad';
        testResult.textContent = 'Could not run the test.';
        return;
      }
      // Red when something actually failed, not merely when something is off. A
      // webhook nobody set up used to report "Ping: no webhook URL set" and paint
      // the whole line red next to a perfectly good email — the report has to
      // separate "broken" from "not configured" or it trains you to ignore it.
      const lines = [];
      lines.push(res.email ? 'Email sent.' : `Email: ${res.email_error}`);
      if (res.ping) lines.push('Ping sent.');
      else if (res.ping_error) lines.push(`Ping: ${res.ping_error}`);
      else if (res.ping_note) lines.push(`Webhook: ${res.ping_note}.`);
      const failed = !!(res.email_error || res.ping_error);
      testResult.className = 'test-result' + (failed ? ' bad' : ' ok');
      testResult.textContent = lines.join(' · ');
    };

    const actions = document.createElement('div');
    actions.className = 'card-actions wide';
    actions.append(save, test);
    out.append(actions, testResult);
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

// ---------- provider marks ----------

// The vendors' own artwork, bundled. A row of six endpoints is six shapes to
// recognise rather than six words to read, and a mark only does that job if it is
// the mark people already know -- a drawn approximation is a shape you have to
// learn, which is the thing it was meant to save you.
//
// Fetched once from each vendor and committed to web/assets/img/providers/, never
// hotlinked: §6 requires the UI to render offline, and a logo CDN is exactly the
// dependency that rule exists to refuse. See that directory's README for where
// each file came from and the trademark position.
//
// `file` is relative to this document (/settings/). `color` is only the monogram
// fallback's colour -- the artwork carries its own.
const PROVIDER_MARKS = {
  deepseek:   { file: '../img/providers/deepseek.svg' },
  anthropic:  { file: '../img/providers/anthropic.svg' },
  google:     { file: '../img/providers/google.svg' },
  openrouter: { file: '../img/providers/openrouter.svg' },
  litellm:    { file: '../img/providers/litellm.svg' },
  ollama:     { file: '../img/providers/ollama.svg' },
};

// The registry name is the owner's to choose, so an entry called
// "anthropic-work" or "google-vertex" should still get its mark. Exact match
// first, then the longest preset name the entry starts with -- longest so a
// shorter key cannot claim a name that merely begins with it.
function markFor(name) {
  const key = (name || '').toLowerCase();
  if (PROVIDER_MARKS[key]) return PROVIDER_MARKS[key];
  const hit = Object.keys(PROVIDER_MARKS)
    .filter((k) => key.startsWith(k))
    .sort((a, b) => b.length - a.length)[0];
  return hit ? PROVIDER_MARKS[hit] : null;
}

// The first column of a provider row. Always labelled with the provider's name:
// the artwork is what you recognise, the label is what a screen reader reads and
// what a hover confirms.
function providerMark(name) {
  const tile = document.createElement('span');
  tile.className = 'prov-mark';
  tile.title = name;

  const mark = markFor(name);
  if (mark) {
    // An <img>, not inlined markup: the files are full-colour brand artwork with
    // their own gradients, so there is nothing for currentColor to do, and this
    // keeps 14KB of path data out of a script the browser has to parse.
    const img = document.createElement('img');
    img.src = mark.file;
    // The alt IS the provider name -- with the name gone from the row, this and
    // the tooltip are the only places it is stated, so it is never empty.
    img.alt = name;
    // Not lazy, deliberately. Six 22px marks at the top of the screen have nothing
    // to defer, and deferring them meant the first row could still be blank after
    // the page had otherwise settled.
    img.decoding = 'async';
    tile.append(img);
    return tile;
  }

  // No preset: a monogram of the name, which is the most identifying thing left.
  // Two letters rather than one, because "litellm-eu" and "litellm-us" differ
  // later than the first character and both are plausible entries.
  tile.setAttribute('role', 'img');
  tile.setAttribute('aria-label', name);
  const mono = document.createElement('span');
  mono.className = 'prov-mono';
  mono.textContent = (name || '?').slice(0, 2).toUpperCase();
  tile.append(mono);
  return tile;
}

// ---------- helpers ----------

// field builds a labelled input carrying its settings key, the shape the Schedule
// and Weekly screens both save from.
function field(key, label, value, type, help) {
  const wrap = document.createElement('label');
  wrap.className = 'field-wrap';

  const l = document.createElement('span');
  l.className = 'field-label';
  l.textContent = label;

  const input = document.createElement('input');
  input.className = 'field';
  input.type = type;
  input.value = value ?? '';
  input.dataset.key = key;

  const h = document.createElement('span');
  h.className = 'field-help';
  h.textContent = help;

  wrap.append(l, input, h);
  return wrap;
}

// Wraps an already-built control in a label and a line of help. field() below
// builds its own input from a settings key; this one takes a control the caller
// needs a reference to, which is every field with a Save button beside it.
function labelledField(control, label, help) {
  const wrap = document.createElement('label');
  wrap.className = 'field-wrap';
  wrap.htmlFor = control.id;

  const l = document.createElement('span');
  l.className = 'field-label';
  l.textContent = label;

  const h = document.createElement('span');
  h.className = 'field-help';
  h.textContent = help;

  wrap.append(l, control, h);
  return wrap;
}

function section(parent, title) {
  const wrap = document.createElement('section');
  wrap.className = 'screen-section';
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

// noteLine is a one-line message inside a card. `tone` matches the tone classes
// already on this screen -- 'ok' | 'bad' | '' -- rather than inventing a third
// vocabulary for the same two colours.
function noteLine(text, tone = '') {
  const p = document.createElement('p');
  p.className = 'note' + (tone ? ' ' + tone : '');
  p.textContent = text;
  return p;
}

// suggestionRow is one candidate and what became of it.
//
// The status is a word, not a colour: 'rejected' with the reason next to it is the
// most useful line on the panel -- "is not a feed" and "fetched but returned no
// items" are what tell the owner the model guessed, and that the guess was caught
// before it became a row.
function suggestionRow(s) {
  const row = document.createElement('div');
  row.className = 'sg-row sg-' + s.status;

  const label = document.createElement('span');
  label.className = 'sg-what';
  if (s.kind === 'hn_search') {
    label.textContent = `"${s.query}"`;
    if (s.min_points) label.append(quietTag(`${s.min_points} points`));
  } else {
    label.textContent = s.name || s.url;
    // The feed's own title, when it disagrees with the name the model gave it.
    // This is the check on "is this the publication it claimed" -- the same
    // question the scraped-channel-ID trap answers wrongly.
    if (s.feed_title && s.name && s.feed_title !== s.name) {
      label.append(quietTag(`feed says "${s.feed_title}"`));
    }
    if (s.items) label.append(quietTag(`${s.items} items`));
  }

  const detail = document.createElement('span');
  detail.className = 'sg-detail';
  // Where it landed, when that is not where it pointed. Worth showing: it is the
  // difference between one masthead and two rows for it.
  detail.textContent = s.final_url || s.url || '';

  const status = document.createElement('span');
  status.className = 'sg-status';
  status.textContent = s.status === 'added' ? 'added, off'
    : s.status === 'proposed' ? '' : s.status;
  if (s.reason) status.title = s.reason;

  row.append(label, detail, status);
  if (s.reason) {
    const why = document.createElement('span');
    why.className = 'sg-reason';
    why.textContent = s.reason;
    row.append(why);
  }
  return row;
}

// modelRow is one provider/model/role's window.
//
// The role rides in the name cell as a tag rather than taking a column of its
// own, which keeps this table on the same five-column grid as the hit rate above
// it -- two tables that look like one thing, because they are being read as one
// thing.
function modelRow(row) {
  const tr = document.createElement('div');
  tr.className = 'qt-row';
  tr.setAttribute('role', 'row');

  const name = document.createElement('span');
  name.className = 'qt-topic';
  name.setAttribute('role', 'cell');
  // provider/model, never model alone: the same model name reached through two
  // providers is two different things to diagnose, and a fallback firing has to
  // read as its own line.
  name.textContent = row.provider ? `${row.provider}/${row.model}` : row.model;
  name.append(quietTag(row.role));
  if (row.failed > 0) name.append(quietTag(`${row.failed} failed`));

  const cells = [
    [String(row.calls), 'qt-n'],
    [formatRate(row.parse_rate), 'qt-rate'],
    [formatRate(row.completeness), 'qt-rate'],
    [String(row.repairs || (row.replies ? '0' : '—')), 'qt-n'],
  ];

  tr.append(name);
  for (const [text, cls] of cells) {
    const cell = document.createElement('span');
    cell.className = cls;
    cell.setAttribute('role', 'cell');
    cell.textContent = text;
    tr.append(cell);
  }

  const rates = tr.querySelectorAll('.qt-rate');

  // Judge only with enough behind the number, same discipline as qualityRow: one
  // unparseable reply out of three is a bad afternoon, not a verdict on a model.
  if (row.replies >= 20 && row.parse_rate >= 0 && row.parse_rate < 0.98) {
    rates[0].classList.add('bad');
    rates[0].title = 'This model is not reliably returning JSON. Each failure costs a repair call, and the ones that fail twice lose the batch. Consider moving the role up the ladder.';
  }
  // Incompleteness is the quieter and more expensive failure: nothing errors, the
  // sky just fills in thinner than it should.
  if (row.items_asked >= 50 && row.completeness >= 0 && row.completeness < 0.9) {
    rates[1].classList.add('bad');
    rates[1].title = 'This model is answering fewer items than it was asked about. Batches are being silently truncated — the items it skipped were read, paid for, and never scored. Move the role to another model.';
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

// hnQueries folds either config shape into the list the editor works on, mirroring
// sources.HNConfig.Normalized on the Go side. The single-query shape is what every
// row carried before migration 007, and a hand-edited config may still use it.
function hnQueries(cfg = {}) {
  const out = [];
  for (const q of cfg.queries || []) {
    if ((q?.query || '').trim()) out.push({ query: q.query.trim(), min_points: q.min_points || 0 });
  }
  if ((cfg.query || '').trim()) out.push({ query: cfg.query.trim(), min_points: cfg.min_points || 0 });
  return out;
}

function describeSource(source) {
  const cfg = source.config || {};
  if (source.kind === 'hn_search') {
    const queries = hnQueries(cfg);
    const fire = cfg.firehose_min_points > 0
      ? ` · everything over ${cfg.firehose_min_points} points`
      : '';
    if (!queries.length) return `Hacker News${fire || ' · no keywords yet'}`;
    // The first few in full, then a count. The whole list is a paragraph at
    // twenty keywords, and the card is one line.
    const shown = queries.slice(0, 3).map((q) => `"${q.query}"`).join(', ');
    const rest = queries.length > 3 ? ` +${queries.length - 3} more` : '';
    return `Hacker News · ${shown}${rest}${fire}`;
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

// ---------- boot ----------

const settings = new Settings();

// Status drives the header line, the Model tab's attention dot, the Schedule
// screen's "next runs" and the wording Signal quality uses when there is nothing
// to show. Polled on the same cadence as the star map's: fast while a run is
// going, because that is the only time anything changes on its own.
let statusTimer = null;

async function loadStatus() {
  try {
    const res = await fetch('/api/status');
    if (!res.ok) { scheduleStatus(20000); return }
    const status = await res.json();
    settings.setStatus(status);
    const running = status.run?.stage && status.run.stage !== 'idle';
    scheduleStatus(running ? 1500 : 20000);
  } catch {
    scheduleStatus(20000);
  }
}

function scheduleStatus(ms) {
  clearTimeout(statusTimer);
  statusTimer = setTimeout(loadStatus, ms);
}

settings.showScreen(screenFromHash());
loadStatus();

// The same deliberate handle app.js keeps on the star map: this is a single-user
// tool the owner runs on their own machine, and being able to ask the open screen
// what it thinks the settings are is worth more than hiding it.
window.interstellarSettings = settings;
