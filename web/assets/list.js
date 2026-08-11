// List view: the same information as the sky, in the same visual language, for
// when you want to read rather than look (§6).

const HOT_GOLD = 'background:var(--hot-gold);box-shadow:0 0 8px rgba(255,201,107,.6)';

export function renderList(container, { topics, groups, hotThreshold, onSelect }) {
  container.replaceChildren();

  for (const topic of topics) {
    const section = document.createElement('section');
    section.className = 'l-topic';

    const heading = document.createElement('h2');
    heading.textContent = topic.title;
    section.append(heading);

    // A star belonging to several topics is listed under each of them (§6).
    const mine = groups
      .filter((g) => g.topicIds.includes(topic.id))
      .sort((a, b) => b.score - a.score || a.ageDays - b.ageDays);

    const sub = document.createElement('div');
    sub.className = 'l-sub';
    if (topic.needs_setup) {
      sub.textContent = 'No definition yet — add one in Settings to start matching.';
    } else {
      sub.textContent = `${mine.length} signal${mine.length === 1 ? '' : 's'}`;
    }
    section.append(sub);

    if (!mine.length) {
      const none = document.createElement('div');
      none.className = 'l-none';
      // §6 requires this exact affordance: an empty topic says so rather than
      // vanishing, so a dry week is legible instead of looking broken.
      none.textContent = 'No signal today.';
      section.append(none);
      container.append(section);
      continue;
    }

    for (const g of mine) {
      const hot = g.sourceCount >= hotThreshold;
      const size = 6 + (g.score - 4) * 8;

      const row = document.createElement('button');
      row.className = 'l-item';
      row.type = 'button';

      const dot = document.createElement('span');
      dot.className = 'l-dot';
      dot.style.cssText =
        `width:${size}px;height:${size}px;opacity:${Math.max(0.35, 1 - g.ageDays * 0.105)};` +
        (hot ? HOT_GOLD : '');

      const title = document.createElement('span');
      title.className = 'l-title';
      title.textContent = g.title;

      // Shared stars are labelled, so seeing the same headline twice reads as
      // "this matters to two of your topics" rather than as a duplicate.
      if (g.topicIds.length > 1) {
        const badge = document.createElement('span');
        badge.className = 'l-badge';
        badge.textContent = `${g.topicIds.length} topics`;
        title.append(badge);
      }

      const meta = document.createElement('span');
      meta.className = 'l-meta';
      meta.textContent =
        `${g.score.toFixed(1)} · ${g.sourceCount} src · ${g.ageDays === 0 ? 'today' : g.ageDays + 'd'}`;

      row.append(dot, title, meta);
      row.addEventListener('click', () => onSelect(g));
      section.append(row);
    }

    container.append(section);
  }
}
