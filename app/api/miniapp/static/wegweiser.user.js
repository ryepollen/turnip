// ==UserScript==
// @name         Wegweiser → turnip
// @namespace    org.welcherweg
// @version      1.2.0
// @description  One-click send of the current page to turnip: audio / notes / transcript / reader
// @match        https://www.youtube.com/*
// @match        https://youtube.com/*
// @match        https://m.youtube.com/*
// @match        https://youtu.be/*
// @match        https://podcasts.apple.com/*
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM.xmlHttpRequest
// @grant        GM.getValue
// @grant        GM.setValue
// @connect      welcherweg.org
// @updateURL    https://welcherweg.org/wegweiser/static/wegweiser.user.js
// @downloadURL  https://welcherweg.org/wegweiser/static/wegweiser.user.js
// ==/UserScript==

/* The browser side of #78: a small case-file bar in the corner. Buttons POST
   {url, action} to /wegweiser/api/submit with the standing token.
   Manager APIs differ (Tampermonkey = sync GM_*, Safari Userscripts = async
   GM.*), so every layer has fallbacks — storage down to localStorage,
   transport down to plain fetch (the server speaks CORS for that) — and any
   failure surfaces as a toast, never silence. */

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

  const store = {
    async get(k) {
      try { if (typeof GM_getValue === 'function') return GM_getValue(k, '') || ''; } catch (e) { /* next */ }
      try { if (typeof GM !== 'undefined' && GM.getValue) return (await GM.getValue(k, '')) || ''; } catch (e) { /* next */ }
      try { return localStorage.getItem('weg_' + k) || ''; } catch (e) { return ''; }
    },
    async set(k, v) {
      try { if (typeof GM_setValue === 'function') { GM_setValue(k, v); return; } } catch (e) { /* next */ }
      try { if (typeof GM !== 'undefined' && GM.setValue) { await GM.setValue(k, v); return; } } catch (e) { /* next */ }
      try { localStorage.setItem('weg_' + k, v); } catch (e) { /* give up */ }
    },
  };

  function request(opts) {
    try { if (typeof GM_xmlhttpRequest === 'function') { GM_xmlhttpRequest(opts); return; } } catch (e) { /* next */ }
    try { if (typeof GM !== 'undefined' && GM.xmlHttpRequest) { GM.xmlHttpRequest(opts); return; } } catch (e) { /* next */ }
    fetch(opts.url, { method: opts.method, headers: opts.headers, body: opts.data })
      .then(async (res) => opts.onload({ status: res.status, responseText: await res.text() }))
      .catch(() => { if (opts.onerror) opts.onerror(); });
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
    let tok = await store.get('token');
    if (!tok) {
      tok = (window.prompt('Wegweiser token (WEGWEISER_TOKEN):') || '').trim();
      if (!tok) { toast('no token', false); return; }
      await store.set('token', tok);
    }
    const link = cleanURL(location.href);
    request({
      method: 'POST',
      url: API,
      headers: { 'Content-Type': 'application/json', 'X-Wegweiser-Token': tok },
      data: JSON.stringify({ url: link, action: action }),
      onload: (r) => {
        if (r.status === 200) { toast('→ turnip: ' + action + ' ✓', true); return; }
        if (r.status === 401) { store.set('token', ''); toast('token rejected — will ask again', false); return; }
        let msg = 'HTTP ' + r.status;
        try { msg = JSON.parse(r.responseText).error || msg; } catch (e) { /* keep */ }
        toast('✗ ' + msg, false);
      },
      onerror: () => toast('✗ network', false),
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
      b.addEventListener('click', (ev) => {
        ev.preventDefault();
        ev.stopPropagation();
        send(action).catch((e) => toast('✗ ' + (e && e.message ? e.message : 'script error'), false));
      });
      return b;
    };
    bar.appendChild(mk('🎵', 'audio', 'audio → feed'));
    bar.appendChild(mk('📓', 'notes', 'notes → Notion'));
    bar.appendChild(mk('📄', 'md', 'transcript (.md)'));
    bar.appendChild(mk('📖', 'read', 'reader'));
    document.body.appendChild(bar);
  }

  buildBar();
  // YouTube is an SPA — keep the bar alive across its soft navigations
  setInterval(buildBar, 3000);
})();
