// Local browser-API unit tests: node --test internal/watch/testdata/voice.test.cjs
// No browser, microphone, network, or model calls.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const vm = require('node:vm');
const asset = (name) => readFileSync(join(__dirname, '../assets', name), 'utf8');
const tick = () => new Promise(setImmediate);
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
};

function page({ folder, permission, script = asset('voice.js') } = {}) {
  const elements = new Map();
  const downloads = [];
  const tracks = [];
  const contexts = [];
  let processor;
  const worklet = vm.createContext({
    AudioWorkletProcessor: class { constructor() { this.port = { postMessage: (data) => queueMicrotask(() => processor.node.port.onmessage({ data })) }; } },
    registerProcessor: (_, C) => { processor = new C(); },
  });
  vm.runInContext(asset('voice-worklet.js'), worklet);
  function element(id) {
    if (!elements.has(id)) elements.set(id, {
      hidden: false, disabled: false, textContent: '', handlers: {},
      addEventListener(type, fn) { this.handlers[type] = fn; },
      setAttribute() {}, focus() {}, after(el) { elements.set(el.id, el); },
      click() { if (this.download) downloads.push(this); else return this.handlers.click?.(); },
    });
    return elements.get(id);
  }
  class AudioContext {
    constructor() { this.sampleRate = 48000; this.audioWorklet = { addModule: async () => {} }; contexts.push(this); }
    async resume() { this.suspended = false; }
    async suspend() { this.suspended = true; }
    close() {}
    createGain() { return { gain: {}, connect() {} }; }
    createMediaStreamSource() { return { connect: (node) => node, disconnect() {} }; }
    createBuffer(_, n) { return { getChannelData: () => new Float32Array(n) }; }
    createBufferSource() {
      const source = { connect() {}, start() {} };
      (this.playbacks ||= []).push(source);
      return source;
    }
  }
  const document = {
    getElementById: element,
    querySelectorAll: () => [1, 2].map((n) => ({ dataset: { n }, textContent: 'Line ' + n })),
    createElement: () => element('created-' + elements.size),
    body: { classList: { toggle() {} } },
    addEventListener(type, fn) { this[type] = fn; },
  };
  const window = { addEventListener() {} };
  if (folder) window.showDirectoryPicker = async () => folder;
  const sandbox = vm.createContext({
    document, window, Blob, URL: { createObjectURL: (blob) => blob, revokeObjectURL() {} },
    setTimeout() {}, setInterval() {}, clearInterval() {},
    navigator: { mediaDevices: { getUserMedia: async () => {
      if (permission) await permission.promise;
      const track = { stopped: false, stop() { this.stopped = true; }, getSettings: () => ({}) };
      tracks.push(track);
      return { getTracks: () => [track], getAudioTracks: () => [track] };
    } } },
    AudioContext,
    AudioWorkletNode: class {
      constructor() { processor.node = this; this.port = { postMessage: (data) => queueMicrotask(() => processor.port.onmessage?.({ data })) }; }
      connect() { return { connect() {} }; }
      disconnect() {}
    },
  });
  vm.runInContext(script, sandbox);
  return { element, document, downloads, tracks, contexts, sandbox,
    click: async (id) => { await element(id).click(); await tick(); },
    audio: (n, value = 0.25) => processor.process([[new Float32Array(n).fill(value)]]),
  };
}

test('Stop flushes the last samples, excludes idle samples, and releases the microphone', async () => {
  const p = page();
  p.audio(128, 0.75);
  await p.click('voice-rec');
  p.audio(2048);
  p.audio(128);
  await p.click('voice-rec');
  assert.equal(p.tracks[0].stopped, true);
  assert.equal(p.contexts[0].suspended, true);
  await p.click('voice-keep');
  const wav = await p.downloads[0].href.arrayBuffer();
  assert.equal(wav.byteLength, 44 + (2048 + 128) * 2);
  assert.equal(new DataView(wav).getInt16(44, true), 8191);
  await p.click('voice-rec');
  assert.equal(p.tracks.length, 2);
  assert.equal(p.tracks[1].stopped, false);
});

test('playback resumes the context without reopening the microphone', async () => {
  const p = page(); await p.click('voice-rec'); p.audio(128); await p.click('voice-rec');
  await p.click('voice-play');
  assert.equal(p.tracks.length, 1);
  assert.equal(p.tracks[0].stopped, true);
  assert.equal(p.contexts[0].suspended, false);
  await p.click('voice-play');
  p.contexts[0].playbacks[0].onended();
  assert.equal(p.contexts[0].suspended, false);
  p.contexts[0].playbacks[1].onended();
  assert.equal(p.contexts[0].suspended, true);
});

test('download mode offers metadata for the takes kept in this session', async () => {
  const p = page();
  await p.click('voice-rec'); p.audio(2048); await p.click('voice-rec');
  await p.click('voice-keep');
  assert.equal(p.element('voice-metadata').textContent, 'Download metadata.csv');
  await p.click('voice-metadata');
  assert.equal(p.downloads[1].download, 'metadata.csv');
  assert.equal(await p.downloads[1].href.text(), '0001|Line 1\n');
});

test('folder loading blocks Space and preserves unrecognised metadata rows', async () => {
  const loading = deferred();
  const writes = new Map();
  const folder = { name: 'test', getFileHandle: async (name, options) => options?.create ? {
    createWritable: async () => ({ write: async (blob) => writes.set(name, await blob.text()), close: async () => {} }),
  } : { getFile: async () => { await loading.promise; return { text: async () => 'id|text\n# note\n0001|Line 1\n' }; } } };
  const p = page({ folder });
  const picking = p.click('voice-folder');
  await tick();
  let prevented = false;
  p.document.keydown({ key: ' ', target: {}, preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(p.tracks.length, 0);
  loading.resolve(); await picking;
  assert.equal(p.element('voice-line').textContent, 'Line 2');
  await p.click('voice-rec'); p.audio(2048); await p.click('voice-rec'); await p.click('voice-keep');
  assert.match(writes.get('metadata.csv'), /id\|text\n# note\n/);
  assert.match(writes.get('metadata.csv'), /0002\|Line 2\n/);
});

for (const mode of ['read', 'talk']) test('switching mode cancels an opening ' + mode + ' recording', async () => {
  const permission = deferred();
  const p = page({ permission });
  if (mode === 'talk') await p.click('voice-mode-talk');
  const opening = p.click(mode === 'read' ? 'voice-rec' : 'voice-talk-rec');
  await tick();
  await p.click(mode === 'read' ? 'voice-mode-talk' : 'voice-mode-read');
  permission.resolve(); await opening;
  assert.equal(p.tracks[0].stopped, true);
  assert.equal(p.element('voice-rec').textContent, 'Record (Space)');
  assert.notEqual(p.element('voice-talk-rec').textContent, 'Stop');
});

test('Keep and Retake are disabled while a retake opens the microphone', async () => {
  const permission = deferred(); permission.resolve();
  const p = page({ permission });
  await p.click('voice-rec'); p.audio(2048); await p.click('voice-rec');
  const waking = deferred();
  permission.promise = waking.promise;
  const retaking = p.element('voice-retake').click();
  try {
    assert.equal(p.element('voice-keep').disabled, true);
    assert.equal(p.element('voice-retake').disabled, true);
  } finally {
    waking.resolve(); await retaking;
  }
});

test('a saved talk retry clears its warning and releases the microphone', async () => {
  let fail = true;
  const folder = { name: 'test', getFileHandle: async (_, options) => {
    if (!options) throw Object.assign(new Error('missing'), { name: 'NotFoundError' });
    if (fail) throw new Error('disk');
    return { createWritable: async () => ({ write: async () => {}, close: async () => {} }) };
  } };
  const p = page({ folder }); await p.click('voice-folder'); await p.click('voice-mode-talk');
  await p.click('voice-talk-rec'); p.audio(2048); await tick(); await p.click('voice-talk-rec');
  assert.equal(p.element('voice-warn').hidden, false);
  fail = false; await p.click('voice-talk-rec');
  assert.equal(p.element('voice-warn').hidden, true);
  assert.match(p.element('voice-talk-note').textContent, /^Saved /);
  assert.equal(p.tracks[0].stopped, true);
});

test('a rejected talk queue does not prevent the next part from saving', async () => {
  // Inject a prior queue rejection without an unhandled rejection in the test runner.
  const source = asset('voice.js').replace('let flushing = Promise.resolve();',
    'let flushing = Promise.reject(new Error("previous part")); flushing.catch(() => {});');
  const q = page({ script: source });
  await q.click('voice-mode-talk'); await q.click('voice-talk-rec'); q.audio(2048);
  await q.click('voice-talk-rec');
  assert.equal(q.downloads.length, 1);
});

test('leaving a running talk flushes its partial block and releases the microphone', async () => {
  const p = page(); await p.click('voice-mode-talk'); await p.click('voice-talk-rec');
  p.audio(128); await p.click('voice-mode-read');
  assert.equal(p.tracks[0].stopped, true);
  assert.equal(p.contexts[0].suspended, true);
  assert.equal(p.element('voice-talk').hidden, true);
  assert.equal((await p.downloads[0].href.arrayBuffer()).byteLength, 44 + 128 * 2);
});

test('unreadable metadata is reported and never overwritten', async () => {
  let writes = 0;
  const folder = { getFileHandle: async (_, options) => {
    if (options?.create) writes++;
    throw new Error('cannot read');
  } };
  const p = page({ folder }); await p.click('voice-folder');
  assert.match(p.element('voice-warn').textContent, /metadata could not be read/);
  await p.click('voice-rec'); p.audio(2048); await p.click('voice-rec'); await p.click('voice-keep');
  assert.equal(writes, 0);
  assert.equal(p.downloads[0].download, '0001.wav');
});
