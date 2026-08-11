/* Wegweiser Mini App — vanilla JS, no build step.
   Reads Telegram.WebApp.initData and attaches it to every API call as the
   X-Telegram-Init-Data header; the server verifies the signature and the
   single-user gate. Dense rows, per-tab actions, ◀▶ pagination. */

'use strict';

const tg = window.Telegram && window.Telegram.WebApp ? window.Telegram.WebApp : null;
const INIT_DATA = tg ? tg.initData : '';
const API = '/wegweiser/api';
const PAGE = 15;

/* ------- tiny helpers ------- */

function authHeaders(extra) {
  const h = { 'X-Telegram-Init-Data': INIT_DATA };
  if (extra) Object.assign(h, extra);
  return h;
}

async function apiGet(path) {
  const res = await fetch(API + path, { headers: authHeaders() });
  if (!res.ok) throw new Error('HTTP ' + res.status);
  return res.json();
}

async function apiPost(path, body) {
  const res = await fetch(API + path, {
    method: 'POST',
    headers: authHeaders({ 'Content-Type': 'application/json' }),
    body: JSON.stringify(body || {}),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
  return data;
}

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function openLink(url) {
  if (!url) return;
  if (tg && tg.openLink) tg.openLink(url); else window.open(url, '_blank');
}

function confirmThen(msg, fn) {
  if (tg && tg.showConfirm) {
    tg.showConfirm(msg, (ok) => { if (ok) fn(); });
  } else if (window.confirm(msg)) {
    fn();
  }
}

let toastTimer = null;
function toast(text) {
  let el = document.getElementById('toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'toast';
    el.style.cssText = 'position:fixed;left:50%;bottom:24px;transform:translateX(-50%);' +
      'background:#1c1a17;color:#efe9dd;padding:8px 14px;border-radius:3px;font-size:12px;' +
      'z-index:99;max-width:90%;text-align:center';
    document.body.appendChild(el);
  }
  el.textContent = text;
  el.style.display = 'block';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.style.display = 'none'; }, 2200);
}

function haptic(kind) {
  if (tg && tg.HapticFeedback) {
    try { tg.HapticFeedback.notificationOccurred(kind); } catch (e) { /* ignore */ }
  }
}

async function download(path, filename) {
  try {
    const res = await fetch(API + path, { headers: authHeaders() });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = filename;
    document.body.appendChild(a); a.click(); a.remove();
    URL.revokeObjectURL(url);
  } catch (e) {
    toast('Download failed');
  }
}

function fmtDate(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const p = (n) => (n < 10 ? '0' + n : '' + n);
  return p(d.getDate()) + '.' + p(d.getMonth() + 1) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

/* ------- tab definitions ------- */

const TABS = [
  { key: 'feed', label: 'Feed', paged: true },
  { key: 'history', label: 'History', paged: true },
  { key: 'notes', label: 'Notes', paged: true },
  { key: 'read', label: 'Reader', paged: true },
  { key: 'queue', label: 'Queue', paged: false },
  { key: 'refs', label: 'References', paged: false },
  { key: 'feeds', label: 'Editions', paged: false },
];

const state = { tab: 'feed', offset: 0, refType: null };

/* ------- rendering ------- */

const $list = () => document.getElementById('list');
const $pager = () => document.getElementById('pager');

function row(inner) { return '<div class="row">' + inner + '</div>'; }

function badge(cls, text) { return '<span class="badge ' + cls + '">' + esc(text) + '</span>'; }

function metaLine(parts) {
  const kept = parts.filter(Boolean);
  return kept.join('<span class="sep">·</span>');
}

function renderFeed(data) {
  if (!data.items.length) return '<div class="empty">Feed is empty</div>';
  return data.items.map((it) => {
    const meta = metaLine([
      esc(fmtDate(it.added)),
      it.duration ? esc(it.duration) : '',
      it.has_notion ? badge('notion', '📓 Notion') : '',
    ]);
    const acts =
      (it.media_url ? '<button class="btn" data-act="open" data-url="' + esc(it.media_url) + '">⬇ Audio</button>' : '') +
      '<button class="btn primary" data-act="notes" data-url="' + esc(it.url) + '">📓 Notes</button>' +
      '<button class="btn danger" data-act="feed-del" data-id="' + esc(it.video_id) + '" data-t="' + esc(it.title) + '">🗑</button>';
    return row(
      '<div class="head"><div class="title">' + esc(it.title) + '</div></div>' +
      '<div class="meta">' + meta + '</div>' +
      '<div class="actions">' + acts + '</div>'
    );
  }).join('');
}

function renderHistory(data) {
  if (!data.items.length) return '<div class="empty">History is empty</div>';
  return data.items.map((it) => {
    const meta = metaLine([
      esc(fmtDate(it.ts)),
      esc(it.action),
      it.duration ? esc(it.duration) : '',
      it.deleted ? badge('deleted', '✗ deleted ' + fmtDate(it.deleted_at)) : '',
    ]);
    return row(
      '<div class="head"><div class="title' + (it.deleted ? ' struck' : '') + '">' + esc(it.title || it.url) + '</div></div>' +
      '<div class="meta">' + meta + '</div>'
    );
  }).join('');
}

function renderNotes(data) {
  if (!data.items.length) return '<div class="empty">No transcripts</div>';
  return data.items.map((it) => {
    const meta = metaLine([
      esc(it.date),
      it.duration_min ? esc(it.duration_min + 'm') : '',
      (it.tags && it.tags.length) ? esc(it.tags.join(', ')) : '',
      it.has_notion ? badge('notion', '📓') : '',
    ]);
    const acts =
      '<button class="btn" data-act="note-dl" data-id="' + esc(it.source_id) + '">⬇ .md</button>' +
      (it.url ? '<button class="btn primary" data-act="notes" data-url="' + esc(it.url) + '">📓 Notion</button>' : '') +
      '<button class="btn danger" data-act="note-del" data-id="' + esc(it.source_id) + '" data-t="' + esc(it.title) + '">🗑</button>';
    return row(
      '<div class="head"><div class="title">' + esc(it.title) + '</div></div>' +
      '<div class="meta">' + meta + '</div>' +
      '<div class="actions">' + acts + '</div>'
    );
  }).join('');
}

function renderRead(data) {
  if (!data.items.length) return '<div class="empty">No articles</div>';
  return data.items.map((it) => {
    const meta = metaLine([
      esc(it.date),
      it.reading_min ? esc(it.reading_min + 'm read') : '',
      it.site ? esc(it.site) : '',
      (it.tags && it.tags.length) ? esc(it.tags.join(', ')) : '',
    ]);
    const acts =
      '<button class="btn" data-act="read-dl" data-id="' + esc(it.source_id) + '">⬇ .md</button>' +
      (it.source_url ? '<button class="btn" data-act="open" data-url="' + esc(it.source_url) + '">🔗 Source</button>' : '') +
      '<button class="btn danger" data-act="read-del" data-id="' + esc(it.source_id) + '" data-t="' + esc(it.title) + '">🗑</button>';
    return row(
      '<div class="head"><div class="title">' + esc(it.title) + '</div></div>' +
      '<div class="meta">' + meta + '</div>' +
      '<div class="actions">' + acts + '</div>'
    );
  }).join('');
}

function renderQueue(data) {
  const pauseBtn = data.paused
    ? '<button class="btn primary wide" data-act="resume">▶ Resume</button>'
    : '<button class="btn wide" data-act="pause">⏸ Pause</button>';
  let html = row(
    '<div class="meta">' +
    '<span class="chip">queued: <b>' + data.queued + '</b></span><span class="sep">·</span>' +
    '<span class="chip">processing: <b>' + data.processing + '</b></span>' +
    (data.paused ? '<span class="sep">·</span><span class="paused">paused</span>' : '') +
    '</div><div class="actions">' + pauseBtn + '</div>'
  );
  if (data.recent && data.recent.length) {
    html += data.recent.map((j) => {
      const mark = j.status === 'failed' ? '❌' : (j.status === 'done' ? '✅' : (j.status === 'processing' ? '⏳' : '•'));
      const meta = metaLine([esc(j.level), esc(j.status), esc(fmtDate(j.updated)), j.error ? esc(j.error) : '']);
      return row(
        '<div class="head"><div class="title">' + mark + ' ' + esc(j.url) + '</div></div>' +
        '<div class="meta">' + meta + '</div>'
      );
    }).join('');
  } else {
    html += '<div class="empty">Queue is empty</div>';
  }
  return html;
}

function renderRefsSummary(data) {
  if (!data.types || !data.types.length) return '<div class="empty">No references yet</div>';
  return data.types.map((t) => row(
    '<div class="head"><div class="title" data-act="ref-type" data-type="' + esc(t.type) + '">' +
    esc(t.type) + '</div><div class="meta"><b>' + t.count + '</b></div></div>'
  )).join('');
}

function renderRefsGroups(data) {
  const back = '<div class="row"><button class="btn" data-act="ref-back">◀ All types</button></div>';
  if (!data.groups || !data.groups.length) return back + '<div class="empty">Empty</div>';
  const body = data.groups.map((g) => {
    const mentions = g.mentions.map((m) => {
      const label = esc(m.episode_title || m.episode_url || '—') + (m.timecode ? ' <span class="badge">' + esc(m.timecode) + '</span>' : '');
      const inner = m.notion_url
        ? '<span data-act="open" data-url="' + esc(m.notion_url) + '" class="link">' + label + '</span>'
        : label;
      const quote = m.quote ? '<div class="meta">«' + esc(m.quote) + '»</div>' : '';
      return '<div class="meta">' + inner + '</div>' + quote;
    }).join('');
    return row('<div class="head"><div class="title">' + esc(g.name) + '</div></div>' + mentions);
  }).join('');
  return back + body;
}

function renderFeeds(data) {
  if (!data.feeds || !data.feeds.length) return '<div class="empty">No editions</div>';
  return data.feeds.map((f) => row(
    '<div class="head"><div class="title">' + esc(f.category) + '</div></div>' +
    '<div class="meta">' + f.episodes + ' ep.</div>' +
    '<div class="actions"><button class="btn" data-act="open" data-url="' + esc(f.feed_url) + '">🔗 Feed link</button></div>'
  )).join('');
}

/* ------- pager ------- */

function renderPager(total) {
  const p = $pager();
  if (total == null || total <= PAGE) { p.innerHTML = ''; return; }
  const page = Math.floor(state.offset / PAGE) + 1;
  const pages = Math.ceil(total / PAGE);
  const prev = state.offset > 0
    ? '<button class="btn" data-act="prev">◀</button>' : '<button class="btn" disabled>◀</button>';
  const next = state.offset + PAGE < total
    ? '<button class="btn" data-act="next">▶</button>' : '<button class="btn" disabled>▶</button>';
  p.innerHTML = prev + '<span>p. ' + page + '/' + pages + '</span>' + next;
}

/* ------- loaders ------- */

async function loadTab() {
  const l = $list();
  l.innerHTML = '<div class="loading">…</div>';
  $pager().innerHTML = '';
  try {
    switch (state.tab) {
      case 'feed': {
        const d = await apiGet('/feed?offset=' + state.offset + '&limit=' + PAGE);
        l.innerHTML = renderFeed(d); renderPager(d.total); break;
      }
      case 'history': {
        const d = await apiGet('/history?offset=' + state.offset + '&limit=' + PAGE);
        l.innerHTML = renderHistory(d); renderPager(d.total); break;
      }
      case 'notes': {
        const d = await apiGet('/notes?offset=' + state.offset + '&limit=' + PAGE);
        l.innerHTML = renderNotes(d); renderPager(d.total); break;
      }
      case 'read': {
        const d = await apiGet('/read?offset=' + state.offset + '&limit=' + PAGE);
        l.innerHTML = renderRead(d); renderPager(d.total); break;
      }
      case 'queue': {
        const d = await apiGet('/status'); l.innerHTML = renderQueue(d); break;
      }
      case 'refs': {
        if (state.refType) {
          const d = await apiGet('/refs?type=' + encodeURIComponent(state.refType));
          l.innerHTML = renderRefsGroups(d);
        } else {
          const d = await apiGet('/refs'); l.innerHTML = renderRefsSummary(d);
        }
        break;
      }
      case 'feeds': {
        const d = await apiGet('/feeds'); l.innerHTML = renderFeeds(d); break;
      }
    }
  } catch (e) {
    l.innerHTML = '<div class="err">Error: ' + esc(e.message) + '</div>';
  }
}

async function loadPulse() {
  try {
    const s = await apiGet('/summary');
    const p = document.getElementById('pulse');
    p.innerHTML =
      '<span class="chip">' + s.feed + ' in feed</span><span class="sep">·</span>' +
      '<span class="chip">' + s.queued + ' queued</span><span class="sep">·</span>' +
      '<span class="chip">' + s.notes + ' notes</span><span class="sep">·</span>' +
      '<span class="chip">' + s.refs + ' refs</span>' +
      (s.paused ? '<span class="sep">·</span><span class="paused">paused</span>' : '');
  } catch (e) {
    document.getElementById('pulse').textContent = '';
  }
}

/* ------- actions ------- */

async function handleAction(act, el) {
  switch (act) {
    case 'open':
      openLink(el.getAttribute('data-url')); break;
    case 'notes':
      try { await apiPost('/notes/enqueue', { url: el.getAttribute('data-url'), level: 'notes' }); toast('📓 Queued'); haptic('success'); loadPulse(); }
      catch (e) { toast(e.message); haptic('error'); }
      break;
    case 'feed-del':
      confirmThen('Delete "' + (el.getAttribute('data-t') || '') + '" from feed?', async () => {
        try { await apiPost('/feed/delete', { video_id: el.getAttribute('data-id') }); toast('Deleted'); haptic('success'); loadTab(); loadPulse(); }
        catch (e) { toast(e.message); haptic('error'); }
      });
      break;
    case 'note-dl':
      download('/notes/file?id=' + encodeURIComponent(el.getAttribute('data-id')), el.getAttribute('data-id') + '.md'); break;
    case 'note-del':
      confirmThen('Delete transcript?', async () => {
        try { await apiPost('/notes/delete', { source_id: el.getAttribute('data-id') }); toast('Deleted'); haptic('success'); loadTab(); loadPulse(); }
        catch (e) { toast(e.message); haptic('error'); }
      });
      break;
    case 'read-dl':
      download('/read/file?id=' + encodeURIComponent(el.getAttribute('data-id')), el.getAttribute('data-id') + '.md'); break;
    case 'read-del':
      confirmThen('Delete article?', async () => {
        try { await apiPost('/read/delete', { source_id: el.getAttribute('data-id') }); toast('Deleted'); haptic('success'); loadTab(); loadPulse(); }
        catch (e) { toast(e.message); haptic('error'); }
      });
      break;
    case 'pause':
    case 'resume':
      try { await apiPost('/queue/pause', { paused: act === 'pause' }); haptic('success'); loadTab(); loadPulse(); }
      catch (e) { toast(e.message); haptic('error'); }
      break;
    case 'ref-type':
      state.refType = el.getAttribute('data-type'); loadTab(); break;
    case 'ref-back':
      state.refType = null; loadTab(); break;
    case 'prev':
      state.offset = Math.max(0, state.offset - PAGE); loadTab(); break;
    case 'next':
      state.offset += PAGE; loadTab(); break;
  }
}

/* ------- wiring ------- */

function buildTabs() {
  const nav = document.getElementById('tabs');
  nav.innerHTML = TABS.map((t) =>
    '<div class="tab' + (t.key === state.tab ? ' active' : '') + '" data-tab="' + t.key + '">' + t.label + '</div>'
  ).join('');
}

function selectTab(key) {
  state.tab = key;
  state.offset = 0;
  state.refType = null;
  buildTabs();
  loadTab();
}

document.addEventListener('click', (ev) => {
  const tabEl = ev.target.closest('.tab');
  if (tabEl) { selectTab(tabEl.getAttribute('data-tab')); return; }
  const actEl = ev.target.closest('[data-act]');
  if (actEl) { handleAction(actEl.getAttribute('data-act'), actEl); }
});

function init() {
  if (tg) {
    tg.ready();
    tg.expand();
  }
  if (!INIT_DATA) {
    document.getElementById('list').innerHTML =
      '<div class="err">Open via the bot button in Telegram — no access signature here.</div>';
    return;
  }
  buildTabs();
  loadPulse();
  loadTab();
}

init();
