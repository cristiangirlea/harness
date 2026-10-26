// Local unit tests of the bot page's script: node --test internal/watch/testdata/bot.test.cjs
// No browser, speech, microphone or network: the commands arrive as hrn:heard.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const vm = require('node:vm');

function page(stored = {}, fetch = () => Promise.reject(new Error('offline'))) {
  const elements = new Map();
  const handlers = {};
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, {
      id, hidden: false, disabled: false, textContent: '', value: '', children: [], dataset: {},
      classList: { add() {}, remove() {}, toggle() {} },
      listeners: {},
      addEventListener(type, fn) { this.listeners[type] = fn; },
      querySelector() { return null; },
      prepend(child) { this.children.unshift(child); },
      append(child) { this.children.push(child); },
    });
    return elements.get(id);
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
  const sandbox = vm.createContext({ document, window: {}, localStorage, fetch, setInterval() {}, Date, JSON });
  vm.runInContext(readFileSync(join(__dirname, '../assets/bot.js'), 'utf8'), sandbox);
  return {
    hear: (...said) => handlers['hrn:heard']({ detail: said }),
    line: () => element('bot-line').textContent,
    snooze: () => JSON.parse(storage.get('hrn-bot:snooze') ?? '0'),
    element,
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
