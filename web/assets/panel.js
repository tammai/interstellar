// The detail panel: frosted glass, opens with a slight overshoot from the right
// edge, Esc closes (§6).

export class Panel {
  constructor(el, { hotThreshold = 3 } = {}) {
    this.el = el;
    this.hotThreshold = hotThreshold;
    this.lastFocus = null;

    el.querySelector('.p-close').addEventListener('click', () => this.close());
    addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && this.isOpen()) this.close();
    });
  }

  isOpen() { return this.el.classList.contains('open') }

  // open renders immediately from what the sky already knows, then fills in the
  // members and attribution when the detail request lands. Waiting for the
  // network before showing anything would make a click feel broken.
  async open(group) {
    this.lastFocus = document.activeElement;

    const hot = group.sourceCount >= this.hotThreshold;
    this.el.querySelector('.p-title').textContent = group.title;
    this.el.querySelector('.p-hot').innerHTML = hot
      ? `<div class="p-hotflag">✦ hot · ${group.sourceCount} sources</div>` : '';
    this.el.querySelector('.p-stats').innerHTML = `
      <div class="p-stat"><div class="n${hot ? ' gold' : ''}">${group.score.toFixed(1)}</div><div class="l">Relevance</div></div>
      <div class="p-stat"><div class="n">${group.sourceCount}</div><div class="l">Sources</div></div>
      <div class="p-stat"><div class="n">${group.ageDays === 0 ? 'Today' : group.ageDays + 'd'}</div><div class="l">Age</div></div>`;
    this.el.querySelector('.p-reason').innerHTML =
      `<em>Why it matched —</em> ${escapeHTML(group.reason || '')}`;

    const link = this.el.querySelector('.p-link');
    link.href = group.primaryURL || '#';

    this.el.querySelector('.p-topic').textContent = '';
    this.el.querySelector('.p-members').replaceChildren();
    this.el.querySelector('.p-srcs').replaceChildren();

    this.el.classList.add('open');
    this.el.querySelector('.p-close').focus({ preventScroll: true });

    // One fetch per constituent signal: a story under two topics has two rows,
    // and the panel should name both.
    try {
      const details = await Promise.all(
        group.ids.map((id) => fetch(`/api/signals/${id}`).then((r) => (r.ok ? r.json() : null)))
      );
      const ok = details.filter(Boolean);
      if (!ok.length || !this.isOpen()) return;

      this.el.querySelector('.p-topic').textContent =
        ok.map((d) => d.topic_title).join(' · ');

      const seen = new Set();
      const members = [];
      for (const d of ok) {
        for (const m of d.members || []) {
          if (seen.has(m.item_id)) continue;
          seen.add(m.item_id);
          members.push(m);
        }
      }
      this.renderMembers(members);

      const sources = [...new Set(ok.flatMap((d) => d.seen_on || []))];
      this.renderSources(sources);
    } catch {
      // A failed detail fetch leaves the headline and link, which is still
      // useful. No error card for something the user can simply retry.
    }
  }

  renderMembers(members) {
    const host = this.el.querySelector('.p-members');
    host.replaceChildren();
    if (!members.length) return;

    const label = document.createElement('span');
    label.className = 's-label';
    label.textContent = members.length === 1 ? 'Article' : `${members.length} articles`;
    host.append(label);

    for (const m of members) {
      const a = document.createElement('a');
      a.className = 'p-member';
      a.href = m.url;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.textContent = m.title;

      if (m.sources?.length) {
        const src = document.createElement('span');
        src.className = 'm-src';
        src.textContent = m.sources.join(' · ');
        a.append(src);
      }
      host.append(a);
    }
  }

  renderSources(sources) {
    const host = this.el.querySelector('.p-srcs');
    host.replaceChildren();
    if (!sources.length) return;

    const label = document.createElement('span');
    label.className = 's-label';
    label.textContent = 'Seen on';
    host.append(label, document.createTextNode(sources.join(' · ')));
  }

  close() {
    this.el.classList.remove('open');
    // Return focus where it came from, so keyboard users are not dumped at the
    // top of the document.
    if (this.lastFocus?.isConnected) this.lastFocus.focus({ preventScroll: true });
  }
}

function escapeHTML(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}
