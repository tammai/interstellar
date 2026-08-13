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
      scrollToEnd();
    }
  }

  renderExamples();
  renderBudget();
  renderHint();
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
    const question = q.value.trim();
    if (!question || state.busy) return;
    q.value = '';
    q.style.height = 'auto';
    ask(question);
  });
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
  el('intro').hidden = true;
  el('send').disabled = true;

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

  const res = await postJSON('/api/ask', { question });
  turn.answered = true;

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
  finishAsking();
}

function finishAsking() {
  state.busy = false;
  el('send').disabled = false;
  renderBudget();
  renderHint();
  scrollToEnd();
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

  const sources = document.createElement('ol');
  sources.className = 'sources';
  sources.hidden = true;

  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.hidden = true;

  wrap.append(q, status, plan, answer, sources, meta);
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
      sources.hidden = hits.length === 0;
      sources.classList.toggle('provisional', provisional);
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

async function postJSON(url, body) {
  try {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const parsed = await res.json().catch(() => ({}));
    if (!res.ok) return { ok: false, error: parsed.error, body: parsed };
    return { ok: true, body: parsed };
  } catch (err) {
    console.error(err);
    return { ok: false, error: 'Could not reach the server.' };
  }
}
