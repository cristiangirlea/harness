// Local unit tests of the bot page's script: node --test internal/watch/testdata/bot.test.cjs
// No browser, speech, microphone or network: the commands arrive as hrn:heard.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const vm = require('node:vm');

function page(stored = {}, fetch = () => Promise.reject(new Error('offline')), opts = {}) {
  const elements = new Map();
  const handlers = {};
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, {
      id, hidden: false, disabled: false, textContent: '', value: '', children: [], dataset: {},
      classes: new Set(),
      classList: {
        add(c) { this.owner.classes.add(c); },
        remove(c) { this.owner.classes.delete(c); },
        toggle(c, on) { if (on) this.owner.classes.add(c); else this.owner.classes.delete(c); },
      },
      listeners: {},
      addEventListener(type, fn) { this.listeners[type] = fn; },
      querySelector() { return null; },
      prepend(child) { this.children.unshift(child); },
      append(child) { this.children.push(child); },
    });
    const e = elements.get(id);
    e.classList.owner = e;
    return e;
  };
  const storage = new Map(Object.entries(stored));
  const document = {
    getElementById: element,
    querySelector: () => element('card'),
    querySelectorAll: () => [],
    createElement: () => element('created-' + elements.size),
    addEventListener(type, fn) { handlers[type] = fn; },
  };
  const localStorage = {
    getItem: (k) => (storage.has(k) ? storage.get(k) : null),
    setItem: (k, v) => storage.set(k, String(v)),
  };
  if (opts.localVoice) element('card').dataset.localVoice = '';
  const spoken = [];
  const synth = opts.synth && {
    voices: opts.voices || [],
    getVoices() { return this.voices; },
    speak(u) { spoken.push(u); },
    cancel() { for (const u of spoken.splice(0)) if (u.onerror) u.onerror(); },
    addEventListener() {},
  };
  const audios = [];
  class Audio {
    constructor(src) { this.src = src; this.paused = true; audios.push(this); }
    play() { this.paused = false; return Promise.resolve(); }
    pause() { this.paused = true; }
  }
  const aborted = [];
  class AbortController {
    constructor() { this.signal = {}; }
    abort() { aborted.push(this.signal.url); }
  }
  const URL = { createObjectURL: (b) => 'blob:' + b.text, revokeObjectURL() {} };
  const window = synth ? { speechSynthesis: synth } : {};
  function SpeechSynthesisUtterance(text) { this.text = text; }
  const sandbox = vm.createContext({ document, window, localStorage, fetch, setInterval() {}, Date, JSON, Audio, AbortController, URL, SpeechSynthesisUtterance });
  vm.runInContext(readFileSync(join(__dirname, '../assets/bot.js'), 'utf8'), sandbox);
  return {
    hear: (...said) => handlers['hrn:heard']({ detail: said }),
    line: () => element('bot-line').textContent,
    snooze: () => JSON.parse(storage.get('hrn-bot:snooze') ?? '0'),
    element,
    spoken,
    audios,
    aborted,
    talking: () => element('card').classes.has('talking'),
    tryIt: () => element('bot-test').listeners.click(),
  };
}

test('"un mute" in two words unmutes rather than snoozing', () => {
  for (const said of ['un mute', 'Un mute please', 'un-mute']) {
    const p = page({ 'hrn-bot:snooze': '-1' });
    p.hear(said);
    assert.equal(p.snooze(), 0, said);
    assert.equal(p.line(), "Right, I'll tell you again.", said);
  }
});

test('spoken and typed questions reply and link the recorded flow', async () => {
  const calls = [];
  const p = page({}, async (url) => {
    calls.push(url);
    return { ok: true, json: async () => ({ text: 'abc failed at gate go vet (exited 1).', ref: 'abc', url: '/flow/abc' }) };
  });
  p.hear('why did it fail');
  await new Promise(setImmediate);
  assert.equal(calls[0], '/bot/answer?q=why%20did%20it%20fail');
  assert.match(p.line(), /abc failed/);
  const link = p.element('bot-log').children[0].children[1];
  assert.equal(link.href, '/flow/abc');
  const input = p.element('bot-question');
  input.value = 'what happens next';
  input.listeners.keydown({ key: 'Enter', preventDefault() {} });
  await new Promise(setImmediate);
  assert.equal(calls[1], '/bot/answer?q=what%20happens%20next');
  p.hear('what are you doing');
  assert.equal(calls.length, 2);
  assert.equal(p.line(), 'Nothing is running.');
  p.hear('why did quiet-hours fail');
  await new Promise(setImmediate);
  assert.equal(calls[2], '/bot/answer?q=why%20did%20quiet-hours%20fail');
  assert.equal(p.snooze(), 0, 'a question naming quiet must not snooze her');
});

test('an unavailable answer is reported without an unhandled rejection', async () => {
  const p = page();
  p.hear('why did it fail');
  await new Promise(setImmediate);
  assert.match(p.line(), /couldn.t read/);
});

test('"quiet until I un mute" still keeps her quiet', () => {
  const p = page();
  p.hear('quiet until I un mute');
  assert.equal(p.snooze(), -1);
});

const settle = () => new Promise(setImmediate);
const sound = (calls) => async (url, init) => {
  calls.push(url);
  if (init && init.signal) init.signal.url = url;
  return { ok: true, blob: async () => ({ text: decodeURIComponent(url.split('q=')[1]) }) };
};

test('the local voice is offered first and speaks sentences in order', async () => {
  const calls = [];
  const p = page({}, sound(calls), { localVoice: true, synth: true, voices: [{ voiceURI: 'v1', name: 'David', lang: 'en-US', localService: true }] });
  const voice = p.element('bot-voice');
  assert.equal(voice.children[0].textContent, 'Local voice (on this computer)');
  assert.equal(voice.value, voice.children[0].value);
  assert.equal(p.element('bot-no-uk').hidden, true, 'no British-voice notice while the local voice is chosen');
  p.element('bot-rate').value = '1.3';
  p.tryIt();
  p.hear('status');
  await settle();
  assert.equal(calls.length, 2);
  assert.match(calls[0], /^\/bot\/voice\?q=Hello%2C%20I'm%20Ada/);
  assert.equal(calls[1], '/bot/voice?q=Nothing%20is%20running.');
  assert.equal(p.audios.length, 1, 'one sentence at a time');
  assert.match(p.audios[0].src, /^blob:Hello, I'm Ada/);
  assert.equal(p.audios[0].playbackRate, 1.3);
  assert.ok(p.talking());
  p.audios[0].onended();
  await settle();
  assert.equal(p.audios[1].src, 'blob:Nothing is running.');
  assert.ok(p.talking());
  p.audios[1].onended();
  assert.ok(!p.talking());
  assert.equal(p.spoken.length, 0);
});

test('a saved browser voice stays chosen over the local voice', () => {
  const p = page({ 'hrn-bot:voice': '"v1"' }, undefined, { localVoice: true, synth: true, voices: [{ voiceURI: 'v1', name: 'David', lang: 'en-US', localService: true }] });
  assert.equal(p.element('bot-voice').value, 'v1');
  assert.equal(p.element('bot-no-uk').hidden, false);
});

test('a sentence the local voice cannot say is spoken by the browser', async () => {
  const calls = [];
  const p = page({}, async (url) => { calls.push(url); return { ok: false, status: 502 }; }, { localVoice: true, synth: true });
  p.hear('status');
  await settle();
  assert.deepEqual(calls, ['/bot/voice?q=Nothing%20is%20running.']);
  assert.equal(p.audios.length, 0);
  assert.equal(p.spoken.length, 1);
  assert.equal(p.spoken[0].text, 'Nothing is running.');
  assert.ok(p.talking());
  p.spoken[0].onend();
  assert.ok(!p.talking());
});

test('skip stops the local voice and clears what waits', async () => {
  const calls = [];
  const p = page({}, sound(calls), { localVoice: true, synth: true });
  p.tryIt();
  p.hear('status');
  await settle();
  const first = p.audios[0];
  p.element('bot-skip').listeners.click();
  assert.ok(first.paused);
  assert.ok(!p.talking());
  assert.deepEqual(p.aborted, calls);
  first.onended();
  await settle();
  assert.equal(p.audios.length, 1, 'a late end of what was skipped plays nothing');
  assert.ok(!p.talking());
  p.hear('repeat');
  await settle();
  assert.equal(p.audios.length, 2, 'the next sentence plays at once');
  assert.equal(p.audios[1].src, 'blob:Nothing is running.');
  first.onended();
  assert.ok(p.talking(), "a late end does not end the next sentence's talking");
});

test('the local voice asks with the header only Ada\'s page can send', async () => {
  const inits = [];
  const p = page({}, async (url, init) => { inits.push(init); return { ok: true, blob: async () => ({ text: 'x' }) }; }, { localVoice: true, synth: true });
  p.hear('status');
  await settle();
  assert.equal(inits.length, 1);
  assert.equal(inits[0].headers['X-Hrn-Voice'], '1');
});

test('the local voice fetches at most one sentence ahead and plays in order', async () => {
  const open = [];
  let most = 0;
  const fetch = (url) => new Promise((resolve) => {
    const entry = {
      url,
      done() {
        open.splice(open.indexOf(entry), 1);
        resolve({ ok: true, blob: async () => ({ text: decodeURIComponent(url.split('q=')[1]) }) });
      },
    };
    open.push(entry);
    most = Math.max(most, open.length);
  });
  const p = page({}, fetch, { localVoice: true, synth: true });
  p.tryIt();
  p.hear('status');
  p.hear('unmute');
  await settle();
  assert.equal(open.length, 1, 'only the head is fetched');
  assert.match(open[0].url, /Hello/);
  for (let i = 0; i < 3; i++) {
    if (open.length) open[0].done();
    await settle();
    if (open.length) open[0].done();
    await settle();
    assert.equal(p.audios.length, i + 1);
    p.audios[i].onended();
    await settle();
  }
  assert.ok(most <= 2, most + ' fetches open at once');
  assert.deepEqual(p.audios.map((a) => a.src), ["blob:Hello, I'm Ada. This is how I sound. I'll tell you when something starts, and when it's done.", 'blob:Nothing is running.', "blob:Right, I'll tell you again."]);
  assert.ok(!p.talking());
  assert.equal(p.spoken.length, 0);
});

test('a saved local voice without the page offering it speaks in the browser', async () => {
  const calls = [];
  const p = page({ 'hrn-bot:voice': '"hrn:local"' }, sound(calls), { synth: true, voices: [{ voiceURI: 'v1', name: 'David', lang: 'en-US', localService: true }] });
  assert.ok(p.element('bot-voice').children.every((o) => o.value !== 'hrn:local'));
  p.hear('status');
  await settle();
  assert.equal(calls.length, 0);
  assert.equal(p.spoken.length, 1);
  assert.equal(p.spoken[0].text, 'Nothing is running.');
});

test('without speechSynthesis the local voice still wakes and a failed sentence moves on', async () => {
  const calls = [];
  const p = page({}, async (url) => {
    calls.push(url);
    if (calls.length === 1) throw new Error('offline');
    return { ok: true, blob: async () => ({ text: decodeURIComponent(url.split('q=')[1]) }) };
  }, { localVoice: true });
  const wake = p.element('bot-wake');
  assert.equal(wake.disabled, false);
  wake.listeners.click();
  p.hear('unmute');
  await settle();
  assert.equal(calls.length, 2);
  assert.equal(p.audios.length, 1);
  assert.equal(p.audios[0].src, "blob:Right, I'll tell you again.");
});

test('changing the voice stops the local voice', async () => {
  const p = page({}, sound([]), { localVoice: true, synth: true, voices: [{ voiceURI: 'v1', name: 'David', lang: 'en-US', localService: true }] });
  p.hear('status');
  await settle();
  const playing = p.audios[0];
  assert.ok(!playing.paused);
  const voice = p.element('bot-voice');
  voice.value = 'v1';
  voice.listeners.change();
  assert.ok(playing.paused);
  assert.ok(!p.talking());
  p.hear('repeat');
  assert.equal(p.spoken.length, 1, 'the browser voice speaks alone');
});

test('without a local voice nothing asks the server to speak', async () => {
  const calls = [];
  const p = page({}, sound(calls), { synth: true, voices: [{ voiceURI: 'v1', name: 'David', lang: 'en-US', localService: true }] });
  assert.ok(p.element('bot-voice').children.every((o) => o.textContent !== 'Local voice (on this computer)'));
  p.hear('status');
  await settle();
  assert.equal(calls.length, 0);
  assert.equal(p.spoken.length, 1);
  assert.equal(p.audios.length, 0);
});
