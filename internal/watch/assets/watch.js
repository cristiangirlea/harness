// hrn watch: follow the page's stream. Fragments arrive as HTML the server
// rendered and escaped; this script only places them.
'use strict';
(function () {
  const state = document.getElementById('state');
  const setState = (text, cls) => {
    if (!state) return;
    state.textContent = text;
    state.className = 'state ' + (cls || '');
  };

  // Elapsed clocks, so the server's HTML stays the same while time passes.
  const span = (ms) => {
    const s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return s + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm' + String(s % 60).padStart(2, '0') + 's';
    return Math.floor(s / 3600) + 'h' + String(Math.floor(s / 60) % 60).padStart(2, '0') + 'm';
  };
  const tick = () => {
    const now = Date.now();
    for (const el of document.querySelectorAll('[data-since]')) {
      const t = Date.parse(el.dataset.since);
      if (!Number.isNaN(t)) el.textContent = span(now - t);
    }
    for (const el of document.querySelectorAll('[data-ago]')) {
      const t = Date.parse(el.dataset.ago);
      if (!Number.isNaN(t)) el.textContent = span(now - t);
    }
  };
  tick();
  setInterval(tick, 1000);

  const backends = document.getElementById('backends');
  const apply = document.getElementById('backends-apply');
  if (backends && apply) apply.addEventListener('click', async () => {
    const status = document.getElementById('backends-status');
    const enabled = [...backends.querySelectorAll('input[name="enabled"]:checked')].map((input) => input.value);
    const choice = backends.querySelector('input[name="default"]:checked');
    apply.disabled = true;
    status.textContent = 'Saving...';
    try {
      const response = await fetch('/backends', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Hrn-Watch-Key': backends.dataset.key },
        body: JSON.stringify({ enabled, default: choice ? choice.value : '' })
      });
      if (response.status !== 204) throw new Error((await response.text()).trim() || 'Backend preferences could not be saved');
      window.location.reload();
    } catch (error) {
      status.textContent = error.message;
      apply.disabled = false;
    }
  });

  const merges = document.getElementById('merges');
  if (merges) {
    const status = document.getElementById('merge-status');
    const confirm = document.getElementById('merge-confirm');
    const cancel = document.getElementById('merge-cancel');
    let grant = null;
    const reset = () => { grant = null; confirm.hidden = cancel.hidden = true; };
    merges.addEventListener('click', async (event) => {
      const task = event.target.dataset.mergeTask;
      if (!task) return;
      reset();
      status.textContent = 'Preparing confirmation...';
      try {
        const path = '/batch/' + encodeURIComponent(merges.dataset.batch) + '/merge/' + encodeURIComponent(task);
        const response = await fetch(path, { method: 'POST', headers: { 'X-Hrn-Watch-Key': merges.dataset.key } });
        if (!response.ok) throw new Error((await response.text()).trim());
        grant = await response.json();
        grant.path = path;
        status.textContent = 'Merge task ' + grant.task + ', branch ' + grant.branch + ' at ' + grant.head.slice(0, 12) + ' into ' + grant.target + '?';
        confirm.hidden = cancel.hidden = false;
      } catch (error) { status.textContent = error.message; }
    });
    cancel.addEventListener('click', () => { reset(); status.textContent = 'Cancelled'; });
    confirm.addEventListener('click', async () => {
      if (!grant) return;
      const current = grant;
      reset();
      status.textContent = 'Merging...';
      try {
        const response = await fetch(current.path + '/confirm', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Hrn-Watch-Key': merges.dataset.key }, body: JSON.stringify({ token: current.token }) });
        if (!response.ok) throw new Error((await response.text()).trim());
        const result = await response.json();
        status.textContent = 'Merged: ' + result.commit;
      } catch (error) { status.textContent = error.message; }
    });
  }

  const box = document.querySelector('[data-stream]');
  if (!box || !window.EventSource) return;

  // Which <details> a reader opened, so replacing the part keeps them open.
  const openKeys = () => new Set([...box.querySelectorAll('details[open][data-key]')].map((d) => d.dataset.key));
  const reopen = (keys) => {
    for (const d of box.querySelectorAll('details[data-key]')) if (keys.has(d.dataset.key)) d.open = true;
  };
  const nearBottom = () => window.innerHeight + window.scrollY >= document.body.scrollHeight - 80;

  const es = new EventSource(box.dataset.stream);
  es.onopen = () => setState('live', 'live');
  es.onerror = () => { if (es.readyState !== EventSource.CLOSED) setState('reconnecting…', 'lost'); };

  es.addEventListener('item', (m) => {
    const follow = nearBottom();
    box.insertAdjacentHTML('beforeend', m.data);
    const last = box.lastElementChild;
    const ago = document.querySelector('[data-ago]');
    if (ago && last && last.dataset.at) ago.dataset.ago = last.dataset.at;
    if (follow) window.scrollTo(0, document.body.scrollHeight);
  });
  // The usage chart, redrawn over every turn so far; a table a reader opened stays open.
  es.addEventListener('usage', (m) => {
    const usage = document.getElementById('usage');
    if (!usage) return;
    const open = new Set([...usage.querySelectorAll('details[open][data-key]')].map((d) => d.dataset.key));
    usage.innerHTML = m.data;
    for (const d of usage.querySelectorAll('details[data-key]')) if (open.has(d.dataset.key)) d.open = true;
  });
  es.addEventListener('result', (m) => {
    const result = document.getElementById('result');
    if (result) result.innerHTML = m.data;
    for (const el of document.querySelectorAll('.clock')) el.hidden = true;
  });
  es.addEventListener('replace', (m) => {
    const keys = openKeys();
    box.innerHTML = m.data;
    reopen(keys);
    tick();
    document.dispatchEvent(new Event('hrn:replaced'));
  });
  // What the bot's stream says is the bot page's own script's business.
  es.addEventListener('say', (m) => document.dispatchEvent(new CustomEvent('hrn:say', { detail: m.data })));
  es.addEventListener('problem', (m) => {
    box.insertAdjacentHTML('afterend', m.data);
    es.close();
    setState('stopped', 'lost');
  });
  es.addEventListener('end', (m) => {
    es.close();
    if (m.data === 'session expired') {
      setState('session expired', 'lost');
      location.reload();
      return;
    }
    setState(m.data === 'stopped' ? 'stopped' : 'finished');
  });
})();
