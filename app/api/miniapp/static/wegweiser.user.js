// ==UserScript==
// @name         Wegweiser → turnip
// @namespace    org.welcherweg
// @version      1.0.0
// @description  One-click send of the current page to turnip: audio / notes / transcript / reader
// @match        https://www.youtube.com/*
// @match        https://youtube.com/*
// @match        https://m.youtube.com/*
// @match        https://youtu.be/*
// @match        https://podcasts.apple.com/*
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @connect      welcherweg.org
// @updateURL    https://welcherweg.org/wegweiser/static/wegweiser.user.js
// @downloadURL  https://welcherweg.org/wegweiser/static/wegweiser.user.js
// ==/UserScript==

/* The browser side of #78: a small case-file bar in the corner. Buttons POST
   {url, action} to /wegweiser/api/submit with the standing token (asked once,
   kept in the userscript manager's storage). The server strips tracking params
   too, but we clean before sending so the toast shows the canonical link. */

(function () {
  'use strict';

  const API = 'https://welcherweg.org/wegweiser/api/submit';
  const TRACKING = ['si', 'feature', 'pp', 'utm_source', 'utm_medium', 'utm_campaign',
    'utm_term', 'utm_content', 'utm_id', 'fbclid', 'gclid', 'igshid', 'ref', 'ref_src'];

  function cleanURL(raw) {
    try {
      const u = new URL(raw);
      TRACKING.forEach((k) => u.searchParams.delete(k));
      return u.toString();
    } catch (e) { return raw; }
  }

  async function token() {
    let t = await GM_getValue('weg_token', '');
    if (!t) {
      t = (window.prompt('Wegweiser token (WEGWEISER_TOKEN):') || '').trim();
      if (t) await GM_setValue('weg_token', t);
    }
    return t;
  }

  function toast(text, ok) {
    const el = document.createElement('div');
    el.textContent = text;
    el.style.cssText = 'position:fixed;right:14px;bottom:64px;z-index:999999;' +
      'background:' + (ok ? '#1c1a17' : '#7a2318') + ';color:#efe9dd;padding:8px 12px;' +
      'border-radius:4px;font:12px ui-monospace,monospace;max-width:60vw';
    document.body.appendChild(el);
    setTimeout(() => el.remove(), 2600);
  }

  async function send(action) {
    const tok = await token();
    if (!tok) { toast('нет токена', false); return; }
    const link = cleanURL(location.href);
    GM_xmlhttpRequest({
      method: 'POST',
      url: API,
      headers: { 'Content-Type': 'application/json', 'X-Wegweiser-Token': tok },
      data: JSON.stringify({ url: link, action: action }),
      onload: (r) => {
        if (r.status === 200) { toast('→ turnip: ' + action + ' ✓', true); return; }
        if (r.status === 401) { GM_setValue('weg_token', ''); toast('токен не подошёл — спрошу заново', false); return; }
        let msg = 'HTTP ' + r.status;
        try { msg = JSON.parse(r.responseText).error || msg; } catch (e) { /* keep */ }
        toast('✗ ' + msg, false);
      },
      onerror: () => toast('✗ сеть', false),
    });
  }

  function buildBar() {
    if (document.getElementById('weg-bar')) return;
    const bar = document.createElement('div');
    bar.id = 'weg-bar';
    bar.style.cssText = 'position:fixed;right:14px;bottom:14px;z-index:999999;display:flex;gap:6px;' +
      'background:#efe9dd;border:2px solid #1c1a17;border-radius:6px;padding:5px 7px;' +
      'font:12px ui-monospace,monospace;opacity:.88';
    const mk = (label, action, title) => {
      const b = document.createElement('button');
      b.textContent = label;
      b.title = title + ' → turnip';
      b.style.cssText = 'border:1px solid #8a8069;background:#e6dfce;color:#1c1a17;' +
        'border-radius:3px;padding:3px 7px;cursor:pointer;font:inherit';
      b.addEventListener('click', (ev) => { ev.preventDefault(); ev.stopPropagation(); send(action); });
      return b;
    };
    bar.appendChild(mk('🎵', 'audio', 'аудио в ленту'));
    bar.appendChild(mk('📓', 'notes', 'конспект в Notion'));
    bar.appendChild(mk('📄', 'md', 'транскрипт'));
    bar.appendChild(mk('📖', 'read', 'в читалку'));
    document.body.appendChild(bar);
  }

  buildBar();
  // YouTube is an SPA — keep the bar alive across its soft navigations
  new MutationObserver(() => buildBar()).observe(document.documentElement, { childList: true, subtree: false });
  setInterval(buildBar, 3000);
})();
