// hrn watch's bot. The sentences come from the server's bot stream (watch.js
// hands each one over as an hrn:say event); this script only decides which to
// speak or notify, by settings kept in this browser, and listens for a few
// commands that steer the bot itself. Nothing here acts on hrn.
'use strict';
(function () {
  const $ = (id) => document.getElementById(id);
  const store = {
    get(key, fallback) {
      try {
        const v = localStorage.getItem('hrn-bot:' + key);
        return v === null ? fallback : JSON.parse(v);
      } catch (e) {
        return fallback; // a private window, or storage turned off
      }
    },
    set(key, value) {
      try { localStorage.setItem('hrn-bot:' + key, JSON.stringify(value)); } catch (e) { /* kept for this visit only */ }
    },
  };
  const card = document.querySelector('.bot');
  const line = $('bot-line');
  const log = $('bot-log');
  const synth = window.speechSynthesis;

  // Which kinds are spoken and which notified: one checkbox each.
  const boxes = [...document.querySelectorAll('input[data-kind]')];
  for (const b of boxes) {
    const key = 'on:' + b.dataset.kind + ':' + b.dataset.how;
    b.checked = store.get(key, b.checked);
    b.addEventListener('change', () => store.set(key, b.checked));
  }
  const wants = (kind, how) => boxes.some((b) => b.dataset.kind === kind && b.dataset.how === how && b.checked);

  // Voices: those on this computer first, since an online one sends what it
  // says (prompts included) to its maker; then British ones; then a woman's,
  // as Ada is one. A browser does not say a voice's gender, so it goes by the
  // names Windows, macOS, Edge and Chrome give theirs. Choosing an online
  // voice is the person's call, in the settings.
  const voiceSel = $('bot-voice');
  const rate = $('bot-rate');
  rate.value = store.get('rate', 1);
  rate.addEventListener('change', () => store.set('rate', Number(rate.value)));
  const british = (v) => v.lang.toLowerCase().replace('_', '-') === 'en-gb';
  const woman = (v) => /\b(hazel|susan|sonia|libby|maisie|mia|abbi|bella|hollie|olivia|zira|aria|jenny|michelle|emma|amy|kate|serena|fiona|moira|karen|samantha|victoria|tessa|ava|allison|catherine|sara|natasha|linda|heather|eva|female)\b/i.test(v.name);
  const score = (v) => (v.localService ? 4 : 0) + (british(v) ? 2 : 0) + (woman(v) ? 1 : 0);
  const fillVoices = () => {
    const voices = synth.getVoices().filter((v) => v.lang.toLowerCase().startsWith('en')).sort((a, b) => score(b) - score(a));
    if (!voices.length) return;
    $('bot-no-uk').hidden = voices.some((v) => v.localService && british(v));
    voiceSel.textContent = '';
    for (const v of voices) {
      const o = document.createElement('option');
      o.value = v.voiceURI;
      o.textContent = v.name + (v.localService ? ' (on this computer)' : ' (online)');
      voiceSel.append(o);
    }
    const saved = store.get('voice', '');
    if (voices.some((v) => v.voiceURI === saved)) voiceSel.value = saved;
  };
  voiceSel.addEventListener('change', () => store.set('voice', voiceSel.value));

  // The face shows talking while any sentence is queued or spoken: the set
  // of utterances not yet ended, since speechSynthesis.speaking can still be
  // true when the last one ends. Holding them also keeps Chrome from dropping
  // a long one before its end event. Skip starts a new set, so the late end
  // events of what it cancelled cannot end the next sentence's talking.
  let lastSpoken = '';
  let inFlight = new Set();
  const speak = (text) => {
    if (!synth || !text) return;
    const u = new SpeechSynthesisUtterance(text);
    u.lang = 'en-GB';
    const v = synth.getVoices().find((x) => x.voiceURI === voiceSel.value);
    if (v) u.voice = v;
    u.rate = Number(rate.value) || 1;
    const mine = inFlight;
    u.onend = u.onerror = () => {
      mine.delete(u);
      card.classList.toggle('talking', inFlight.size > 0);
    };
    mine.add(u);
    card.classList.add('talking');
    synth.speak(u);
    lastSpoken = text;
  };
  const skip = () => {
    inFlight = new Set();
    card.classList.remove('talking');
    if (synth) synth.cancel();
  };

  // Snoozing: until a time, or until unmuted (-1). Muting: one run or batch.
  const snoozeSel = $('bot-snooze');
  const quiet = $('bot-quiet');
  const snoozed = () => { const u = store.get('snooze', 0); return u === -1 || u > Date.now(); };
  const showQuiet = () => {
    const u = store.get('snooze', 0);
    if (!snoozed()) {
      quiet.hidden = true;
      snoozeSel.value = '0';
      return;
    }
    quiet.hidden = false;
    quiet.textContent = u === -1 ? 'Quiet until you unmute me.' : 'Quiet until ' + new Date(u).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' }) + '.';
    snoozeSel.value = String(store.get('snoozeChoice', 0));
  };
  const snooze = (minutes) => {
    store.set('snooze', minutes === -1 ? -1 : minutes > 0 ? Date.now() + minutes * 60000 : 0);
    store.set('snoozeChoice', minutes);
    if (minutes) skip();
    showQuiet();
  };
  snoozeSel.addEventListener('change', () => snooze(Number(snoozeSel.value)));
  showQuiet();
  setInterval(showQuiet, 30000);

  const savedMuted = store.get('muted', []);
  let muted = new Set(Array.isArray(savedMuted) ? savedMuted.filter((id) => typeof id === 'string') : []);
  const markMuted = () => {
    for (const b of document.querySelectorAll('[data-mute]')) b.textContent = muted.has(b.dataset.mute) ? 'Unmute this' : 'Mute this';
  };
  document.addEventListener('click', (e) => {
    const b = e.target.closest('[data-mute]');
    if (!b) return;
    const id = b.dataset.mute;
    if (muted.has(id)) muted.delete(id); else muted.add(id);
    muted = new Set([...muted].slice(-200));
    store.set('muted', [...muted]);
    markMuted();
  });
  document.addEventListener('hrn:replaced', markMuted);
  markMuted();

  // Notifications: the browser allows them on this computer's own address
  // only (a secure context), after a yes.
  const ask = $('bot-notify-ask');
  const notifyState = $('bot-notify-state');
  const canNotify = 'Notification' in window && window.isSecureContext;
  const showNotify = () => {
    if (!canNotify) {
      notifyState.textContent = 'Notifications are not available here.';
      return;
    }
    ask.hidden = Notification.permission !== 'default';
    notifyState.textContent = { granted: 'Notifications are on.', denied: "Notifications are blocked for this page in the browser's settings.", default: '' }[Notification.permission] || '';
  };
  ask.addEventListener('click', () => Notification.requestPermission().then(showNotify));
  showNotify();
  if (!window.isSecureContext) $('bot-net').hidden = false;
  const safeURL = (u) => (/^\/(run|batch)\/[0-9a-f-]+$/.test(u || '') ? u : '');
  const notify = (m) => {
    if (!canNotify || Notification.permission !== 'granted') return;
    const n = new Notification('Ada', {
      body: m.text,
      tag: (m.ref || '') + ':' + m.kind,
      requireInteraction: m.kind === 'done' || m.kind === 'failed', // stays until dismissed
    });
    n.onclick = () => {
      window.focus();
      const url = safeURL(m.url);
      if (url) location.href = url;
      n.close();
    };
  };

  // The log of what was said, newest first.
  const show = (m) => {
    line.textContent = m.text;
    const empty = log.querySelector('.muted');
    if (empty) empty.remove();
    const li = document.createElement('li');
    li.className = m.kind;
    const t = document.createElement('time');
    t.textContent = new Date().toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' });
    li.append(t);
    const url = safeURL(m.url);
    const text = document.createElement(url ? 'a' : 'span');
    if (url) text.href = url;
    text.textContent = m.text;
    li.append(text);
    log.prepend(li);
    while (log.children.length > 100) log.lastElementChild.remove();
  };

  let lastStatus = 'Nothing is running.';
  document.addEventListener('hrn:say', (e) => {
    let m;
    try { m = JSON.parse(e.detail); } catch (err) { return; }
    if (!m || typeof m.text !== 'string' || typeof m.kind !== 'string') return;
    if (m.kind === 'status') {
      lastStatus = m.text;
      if (!wants('status', 'speak') && !wants('status', 'notify')) return;
    }
    show(m);
    if ((m.ref && muted.has(m.ref)) || snoozed()) return;
    if (wants(m.kind, 'speak')) speak(m.text);
    if (wants(m.kind, 'notify')) notify(m);
  });

  // A browser speaks only after the page has been clicked, so the bot waits
  // for one click before it says anything.
  const wake = $('bot-wake');
  const reply = (text) => {
    line.textContent = text;
    speak(text);
  };
  if (!synth) {
    wake.textContent = 'This browser cannot speak';
    wake.disabled = true;
  } else {
    fillVoices();
    synth.addEventListener('voiceschanged', fillVoices);
    wake.addEventListener('click', () => {
      wake.hidden = true;
      reply(lastStatus);
    });
  }
  $('bot-skip').addEventListener('click', skip);
  $('bot-test').addEventListener('click', () => reply("Hello, I'm Ada. This is how I sound. I'll tell you when something starts, and when it's done."));
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') skip(); });

  // Listening: one sentence each time the button is pressed, for commands
  // that steer the bot. Quiet is tried before unmute, so "quiet until I
  // unmute" keeps her quiet; a plain "unmute" is not heard as quiet, as
  // \bmute\b does not match inside it.
  const commands = [
    [/\b(status|what are you doing|what'?s (going on|happening|running)|anything running)\b/, () => reply(lastStatus)],
    [/\b(quiet|snooze|mute|silence|hush)\b/, (said) => {
      const minutes = /hour/.test(said) ? 60 : /until|unmute/.test(said) ? -1 : 15;
      snooze(minutes);
      reply(minutes === -1 ? "I'll keep quiet until you unmute me." : minutes === 60 ? 'Quiet for an hour.' : 'Quiet for 15 minutes.');
    }],
    [/\b(unmute|carry on|wake up|speak again|resume)\b/, () => { snooze(0); reply("Right, I'll tell you again."); }],
    [/\b(skip|stop|shush|enough)\b/, () => skip()],
    [/\b(repeat|again|what did you say)\b/, () => reply(lastSpoken || lastStatus)],
  ];
  const act = (alternatives) => {
    for (const said of alternatives) {
      for (const [re, run] of commands) {
        if (re.test(said)) {
          run(said);
          return;
        }
      }
    }
    reply("Sorry, I didn't catch that. You can say status, skip, quiet, unmute or repeat.");
  };
  // What the recogniser heard arrives as an event, so the commands can be
  // tried without a microphone.
  document.addEventListener('hrn:heard', (e) => {
    if (Array.isArray(e.detail)) act(e.detail.map((s) => String(s).toLowerCase()));
  });
  const Recogniser = window.SpeechRecognition || window.webkitSpeechRecognition;
  const listen = $('bot-listen');
  if (Recogniser && window.isSecureContext) {
    listen.hidden = false;
    listen.addEventListener('click', () => {
      const r = new Recogniser();
      r.lang = 'en-GB';
      r.interimResults = false;
      r.maxAlternatives = 3;
      r.onresult = (e) => document.dispatchEvent(new CustomEvent('hrn:heard', { detail: [...e.results[0]].map((a) => a.transcript) }));
      r.onerror = (e) => {
        line.textContent = e.error === 'not-allowed' ? 'The microphone is not allowed for this page.' : "I couldn't hear that (" + e.error + ').';
      };
      r.onend = () => {
        listen.textContent = 'Talk to me';
        listen.disabled = false;
      };
      skip(); // not talking over the person
      listen.textContent = 'Listening…';
      listen.disabled = true;
      r.start();
    });
  }
})();
