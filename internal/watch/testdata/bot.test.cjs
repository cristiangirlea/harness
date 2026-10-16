// Local unit tests of the bot page's script: node --test internal/watch/testdata/bot.test.cjs
// No browser, speech, microphone or network: the commands arrive as hrn:heard.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const vm = require('node:vm');

function page(stored = {}) {
  const elements = new Map();
  const handlers = {};
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, {
      id, hidden: false, disabled: false, textContent: '', value: '', children: [], dataset: {},
      classList: { add() {}, remove() {}, toggle() {} },
      addEventListener() {}, querySelector() { return null; }, prepend() {}, append() {},
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
  const sandbox = vm.createContext({ document, window: {}, localStorage, setInterval() {}, Date, JSON });
  vm.runInContext(readFileSync(join(__dirname, '../assets/bot.js'), 'utf8'), sandbox);
  return {
    hear: (...said) => handlers['hrn:heard']({ detail: said }),
    line: () => element('bot-line').textContent,
    snooze: () => JSON.parse(storage.get('hrn-bot:snooze') ?? '0'),
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

test('"quiet until I un mute" still keeps her quiet', () => {
  const p = page();
  p.hear('quiet until I un mute');
  assert.equal(p.snooze(), -1);
});
