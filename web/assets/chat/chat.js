// The Ask screen.
//
// Two things shape this file. First, it loads none of the star map: no canvas, no
// requestAnimationFrame loop, no sprite cache. Second, retrieval is local and
// takes milliseconds while the model calls take seconds, so the page shows what it
// found before it shows what it thinks -- see ask() below, which fires the free
// search and the expensive answer at the same moment.

const el = (id) => document.getElementById(id);

const state = {
  ready: false,
  problem: '',
  callsToday: 0,
  capDay: 0,
  busy: false,
  // The in-flight request, so the send button can call it off. Null when idle.
  controller: null,
  // Whether the transcript has anything in it, which is what the Clear button waits
  // for -- an empty conversation with a Clear button is a control that does nothing.
  hasTurns: false,
};

const EXAMPLES = [
  'What mattered most this week?',
  'Anything new on agent harnesses?',
  'What did I miss yesterday?',
];

// ---------- boot ----------

init();

async function init() {
  bindComposer();
  bindClear();

  // Focused HERE, before the history fetch -- not only after it.
  //
  // init() awaits /api/chat/history, so a caret placed at the end of this function
  // arrives however long that request takes: the page is on screen and typeable, and
  // the first keystrokes go nowhere. The textarea exists at this point (a module script
  // runs after the document is parsed), so there is nothing to wait for.
  focusFromHash();

  const history = await getJSON('/api/chat/history?limit=20');
  if (history) {
    state.ready = !!history.ready;
    state.problem = history.problem || '';
    state.callsToday = history.chat_calls_today ?? 0;
    state.capDay = history.chat_cap_day ?? 0;

    // Oldest first on screen, though the API returns newest first: a transcript
    // reads downwards, and the composer sits at the bottom.
    const turns = [...(history.turns || [])].reverse();
    for (const turn of turns) renderStoredTurn(turn);
    if (turns.length) {
      el('intro').hidden = true;
      state.hasTurns = true;
      scrollToEnd();
    }
  }

  renderExamples();
  renderBudget();
  renderHint();
  renderClear();

  // And again, because renderHint() decides whether the composer is usable at all: it
  // disables the textarea when chat has no model, and a disabled element cannot take
  // focus. This is the call that lands once the answer to that is known.
  focusFromHash();
}

// focusFromHash puts the caret in the composer when the fragment asked for it.
//
// The star map's ask dock links to #ask, and the caret belongs where the click was
// aiming. Only on that fragment: arriving any other way -- a bookmark, Back from an
// article, a reload while reading -- must not throw a keyboard over the transcript on a
// phone. Bound to hashchange as well, for a link followed while already on this page,
// where the document does not reload and init() never runs again.
function focusFromHash() {
  if (location.hash === '#ask') focusComposer();
}

window.addEventListener('hashchange', focusFromHash);

function focusComposer() {
  const q = el('q');
  // Disabled means chat has no usable model, and the gold hint under the composer says
  // so. Focusing it would be impossible anyway; the message is the useful thing.
  if (!q.disabled) q.focus();
}

function bindComposer() {
  const q = el('q');

  // Grow with the question, up to a point. Cheap: only on input, and capped.
  q.addEventListener('input', () => {
    q.style.height = 'auto';
    q.style.height = Math.min(q.scrollHeight, 160) + 'px';
  });

  q.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      el('composer').requestSubmit();
    }
  });

  el('composer').addEventListener('submit', (e) => {
    e.preventDefault();
    // While a question is in flight this button is the stop button, so a submit is a
    // cancel. Handled here rather than by swapping the handler: one path, and the
    // Enter key gets the same behaviour as the click for free.
    if (state.busy) {
      stopAsking();
      return;
    }
    const question = q.value.trim();
    if (!question) return;
    q.value = '';
    q.style.height = 'auto';
    ask(question);
  });
}

function bindClear() {
  el('clear').onclick = async () => {
    // Same guard as deleting a topic or a source. This one cannot be undone either,
    // and the transcript is the only record of what was asked.
    if (!confirm('Clear the whole conversation? The questions and answers are deleted.')) return;

    const btn = el('clear');
    btn.disabled = true;
    const res = await fetch('/api/chat/history', { method: 'DELETE' });
    btn.disabled = false;
    if (!res.ok) return;

    // The thread is rebuilt rather than emptied, because #intro lives inside it and
    // an empty screen with no invitation on it reads as a broken page.
    for (const turn of [...el('thread').querySelectorAll('.turn')]) turn.remove();
    el('intro').hidden = false;
    state.hasTurns = false;
    renderClear();
    focusComposer();
  };
}

function renderClear() {
  el('clear').hidden = !state.hasTurns;
}

function renderExamples() {
  const box = el('examples');
  box.replaceChildren();
  for (const text of EXAMPLES) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'example';
    b.textContent = text;
    b.onclick = () => { if (!state.busy) ask(text) };
    box.append(b);
  }
}

// ---------- asking ----------

async function ask(question) {
  state.busy = true;
  state.controller = new AbortController();
  el('intro').hidden = true;
  setSendMode('stop');

  const turn = openTurn(question);
  scrollToEnd();

  // The free half of the work, started at the same instant as the expensive half.
  //
  // /api/search is a local FTS5 query -- no model, no network beyond this process
  // -- so it comes back in milliseconds and gives the reader something true to
  // look at while the answer is still being written. These are the same words the
  // planner will refine, so the provisional list is usually the final one.
  getJSON('/api/search?limit=8&q=' + encodeURIComponent(question)).then((found) => {
    if (turn.answered || !found?.hits?.length) return;
    turn.setStatus(`Reading ${found.hits.length} result${found.hits.length === 1 ? '' : 's'}…`);
    turn.setSources(found.hits, { provisional: true });
  });

  const res = await postJSON('/api/ask', { question }, state.controller.signal);
  turn.answered = true;

  if (res.aborted) {
    // Stopped on purpose, so it is not a failure. The question stays on screen with
    // the sources the free search already found -- that part is real and was not
    // waiting on the model.
    turn.setStatus('');
    turn.stopped();
    // The model may have been called before the connection closed, and llm_calls is
    // the only place that knows. Re-read it rather than guessing at the budget.
    refreshBudget();
    finishAsking();
    return;
  }

  if (!res.ok) {
    turn.fail(res.error || 'Something went wrong.');
    finishAsking(res.body);
    return;
  }

  const data = res.body;
  turn.setStatus('');
  turn.setPlan(data.query, data.planned);
  turn.setSources(data.hits || [], { provisional: false });
  turn.setAnswer(data.answer || '', data.hits || []);
  turn.setMeta(data);

  state.callsToday = data.chat_calls_today ?? state.callsToday;
  state.capDay = data.chat_cap_day ?? state.capDay;
  state.hasTurns = true;
  finishAsking();
}

// stopAsking abandons the request in flight.
//
// The server sees the connection close and cancels its own context, so a stop is not
// just the browser looking away -- pipeline.Asker already treats a cancelled context as
// a hard stop rather than something to retry.
function stopAsking() {
  state.controller?.abort();
}

function finishAsking() {
  state.busy = false;
  state.controller = null;
  setSendMode('send');
  renderBudget();
  renderHint();
  renderClear();
  scrollToEnd();
}

// setSendMode is the only place the button's icon, label and job change together --
// the same reason the star map has one setRefreshBusy.
function setSendMode(mode) {
  const btn = el('send');
  const stopping = mode === 'stop';
  btn.classList.toggle('stop', stopping);
  btn.disabled = false;
  btn.setAttribute('aria-label', stopping ? 'Stop this request' : 'Send question');
  btn.title = stopping ? 'Stop' : 'Send';
}

async function refreshBudget() {
  const history = await getJSON('/api/chat/history?limit=1');
  if (!history) return;
  state.callsToday = history.chat_calls_today ?? state.callsToday;
  state.capDay = history.chat_cap_day ?? state.capDay;
  renderBudget();
}

// openTurn appends a question and returns handles for filling in the rest as it
// arrives. Returning setters rather than re-rendering the thread means an answer
// landing never disturbs anything the reader is already looking at.
function openTurn(question) {
  const wrap = document.createElement('article');
  wrap.className = 'turn';

  const q = document.createElement('div');
  q.className = 'q';
  q.textContent = question;

  const status = document.createElement('div');
  status.className = 'status';
  status.textContent = 'Searching…';

  const plan = document.createElement('div');
  plan.className = 'plan';
  plan.hidden = true;

  const answer = document.createElement('div');
  answer.className = 'a';
  answer.hidden = true;

  // Sources collapse once they arrive. A turn's answer is the thing being read;
  // eight source cards under every question turn the transcript into a list of
  // links with the conversation buried between them, and the second question
  // pushes the first answer off screen. The count stays visible because that is
  // the part read at a glance -- "answered from 8" is a different claim from
  // "answered from 1".
  //
  // <details> rather than a button and a class: it is collapsible, keyboard
  // operable and screen-reader announced without a line of state, and this screen
  // deliberately carries no framework. Citations are unaffected either way --
  // [n] links straight to the article, not to the row.
  const fold = document.createElement('details');
  fold.className = 'src-fold';
  fold.hidden = true;

  const foldLabel = document.createElement('summary');
  foldLabel.className = 'src-fold-label';

  const sources = document.createElement('ol');
  sources.className = 'sources';
  fold.append(foldLabel, sources);

  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.hidden = true;

  wrap.append(q, status, plan, answer, fold, meta);
  el('thread').append(wrap);

  return {
    answered: false,
    setStatus(text) {
      status.textContent = text;
      status.hidden = !text;
    },
    setPlan(query, planned) {
      const line = describePlan(query, planned);
      plan.textContent = line;
      plan.hidden = !line;
    },
    setSources(hits, { provisional }) {
      sources.replaceChildren();
      fold.hidden = hits.length === 0;
      fold.classList.toggle('provisional', provisional);
      // The open state is set once, at creation, and never written here: this
      // runs twice per turn (the provisional list, then the planned one), and
      // reasserting it would snap the fold shut under a reader who had just
      // opened it to watch the sources land.
      foldLabel.textContent = `${hits.length} source${hits.length === 1 ? '' : 's'}`;
      for (const hit of hits) sources.append(sourceRow(hit));
    },
    setAnswer(text, hits) {
      answer.replaceChildren(...renderCitations(text, hits));
      answer.hidden = !text;
    },
    setMeta(data) {
      const bits = [];
      // Sub-second in milliseconds: a cached or empty-result answer comes back in
      // single digits, and "0.0s" reads as a broken timer rather than a fast one.
      if (data.elapsed_ms) {
        bits.push(data.elapsed_ms < 1000
          ? `${data.elapsed_ms}ms`
          : `${(data.elapsed_ms / 1000).toFixed(1)}s`);
      }
      if (data.calls) bits.push(`${data.calls} call${data.calls === 1 ? '' : 's'}`);
      const tokens = (data.tokens_in || 0) + (data.tokens_out || 0);
      if (tokens) bits.push(`${tokens} tokens`);
      meta.textContent = bits.join(' · ');
      meta.hidden = bits.length === 0;
    },
    fail(message) {
      status.hidden = true;
      answer.hidden = false;
      answer.classList.add('failed');
      answer.textContent = message;
    },
    // Stopped is not failed. Same muted treatment as the plan line, because nothing
    // went wrong -- the reader changed their mind, and the sources already listed
    // above are still worth looking at.
    stopped() {
      status.hidden = false;
      status.textContent = 'Stopped.';
    },
  };
}

// describePlan says what was actually searched.
//
// A wrong answer here is almost always a wrong plan, and a plan the reader cannot
// see is a plan they cannot correct. Showing it turns "that answer is wrong" into
// "it only looked at last week".
function describePlan(query, planned) {
  if (!query) return '';
  const bits = [];
  if (query.text) bits.push(`“${query.text}”`);
  if (query.kind === 'signals') bits.push('signals only');
  if (query.kind === 'items') bits.push('articles only');
  if (query.from && query.to) bits.push(`${query.from} → ${query.to}`);
  else if (query.from) bits.push(`since ${query.from}`);
  else if (query.to) bits.push(`up to ${query.to}`);
  if (!bits.length) return '';

  const prefix = planned ? 'Searched' : 'Searched (no plan — used your words)';
  return `${prefix}: ${bits.join(' · ')}`;
}

function sourceRow(hit) {
  const li = document.createElement('li');

  const a = document.createElement('a');
  a.href = hit.url || '#';
  a.target = '_blank';
  a.rel = 'noopener noreferrer';
  a.textContent = hit.title || 'Untitled';
  li.append(a);

  const meta = document.createElement('span');
  meta.className = 'src-meta';
  const bits = [];
  if (hit.topic_title) bits.push(hit.topic_title);
  if (hit.date) bits.push(hit.date);
  if (hit.kind === 'signal' && hit.source_count) {
    bits.push(`${hit.source_count} source${hit.source_count === 1 ? '' : 's'}`);
  }
  if (hit.kind === 'item' && hit.source_name) bits.push(hit.source_name);
  meta.textContent = bits.join(' · ');
  li.append(meta);

  return li;
}

// renderCitations turns [n] markers into links onto the matching source row.
//
// Built as DOM nodes rather than an HTML string on purpose: the answer is model
// output, and assembling markup from it -- even with a template -- is how an
// injection gets in. textContent for every span means the model cannot emit
// anything but text.
function renderCitations(text, hits) {
  const out = [];
  const re = /\[(\d+)\]/g;
  let last = 0;
  let m;

  while ((m = re.exec(text))) {
    if (m.index > last) out.push(document.createTextNode(text.slice(last, m.index)));

    const n = Number(m[1]);
    const hit = hits[n - 1];
    if (hit && hit.url) {
      const a = document.createElement('a');
      a.className = 'cite';
      a.href = hit.url;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.textContent = n;
      a.title = hit.title || '';
      out.push(a);
    } else {
      // A citation pointing at nothing stays visible as plain text. Silently
      // dropping it would make a hallucinated reference look like prose.
      out.push(document.createTextNode(m[0]));
    }
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push(document.createTextNode(text.slice(last)));
  return out;
}

// ---------- stored turns ----------

// renderStoredTurn draws history. Sources are not redrawn: signals are rewritten
// on every compose, so the ids a past turn cited may no longer exist, and showing
// a stale list as though it were live would be worse than showing none.
function renderStoredTurn(turn) {
  const wrap = document.createElement('article');
  wrap.className = 'turn past';

  const q = document.createElement('div');
  q.className = 'q';
  q.textContent = turn.question;

  const a = document.createElement('div');
  a.className = 'a';
  // No hits to resolve against, so citation markers stay as text.
  a.textContent = turn.answer;

  wrap.append(q, a);

  const plan = describePlan(safeParse(turn.query), turn.planned);
  if (plan) {
    const p = document.createElement('div');
    p.className = 'plan';
    p.textContent = plan;
    wrap.insertBefore(p, a);
  }

  const bits = [];
  if (turn.created_at) bits.push(turn.created_at.replace('T', ' ').replace('Z', ''));
  if (turn.hit_count) bits.push(`${turn.hit_count} result${turn.hit_count === 1 ? '' : 's'}`);
  if (bits.length) {
    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.textContent = bits.join(' · ');
    wrap.append(meta);
  }

  el('thread').append(wrap);
}

function safeParse(value) {
  if (!value) return null;
  if (typeof value === 'object') return value;
  try {
    return JSON.parse(value);
  } catch {
    return null;
  }
}

// ---------- chrome ----------

function renderBudget() {
  const box = el('budget');
  if (!state.capDay) {
    box.textContent = '';
    return;
  }
  const left = Math.max(0, state.capDay - state.callsToday);
  box.textContent = `${left} of ${state.capDay} calls left today`;
  box.classList.toggle('low', left <= 4);
}

function renderHint() {
  const hint = el('hint');
  if (!state.ready) {
    hint.textContent = state.problem
      ? `Chat cannot run: ${state.problem}. Set a chat model in Settings on the star map.`
      : 'Chat has no model configured. Set one in Settings on the star map.';
    hint.className = 'hint warn';
    el('send').disabled = true;
    el('q').disabled = true;
    return;

  }
  hint.textContent = 'Answers come only from your own signals and articles. Enter to send, Shift+Enter for a new line.';
  hint.className = 'hint';
}

function scrollToEnd() {
  const thread = el('thread');
  // 'auto' rather than 'smooth': a reduced-motion reader should not be scrolled
  // through an animation, and there is nothing to see on the way.
  thread.scrollTo({ top: thread.scrollHeight, behavior: 'auto' });
}

// ---------- transport ----------

async function getJSON(url) {
  try {
    const res = await fetch(url);
    if (!res.ok) return null;
    return await res.json();
  } catch (err) {
    console.error(err);
    return null;
  }
}

async function postJSON(url, body, signal) {
  try {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal,
    });
    const parsed = await res.json().catch(() => ({}));
    if (!res.ok) return { ok: false, error: parsed.error, body: parsed };
    return { ok: true, body: parsed };
  } catch (err) {
    // An abort is the reader pressing stop, not a fault, and must not be logged as
    // one or reported as "could not reach the server".
    if (err?.name === 'AbortError') return { ok: false, aborted: true };
    console.error(err);
    return { ok: false, error: 'Could not reach the server.' };
  }
}
