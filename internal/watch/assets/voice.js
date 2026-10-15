// hrn watch's voice recorder (#91): records the lines for Ada's voice in the
// browser and saves them into a folder the person picks. Nothing is sent to
// hrn watch or anywhere else; the server only serves this page.
'use strict';
(function () {
  const $ = (id) => document.getElementById(id);
  const lines = [...document.querySelectorAll('#voice-lines li')].map((li) => ({ n: Number(li.dataset.n), text: li.textContent }));
  if (!lines.length) return;
  const pad = (n) => String(n).padStart(4, '0');
  const warn = (text) => {
    const w = $('voice-warn');
    w.hidden = !text;
    w.textContent = text || '';
  };

  // ---- Saving: into a folder she picks (File System Access API, in Edge and
  // Chrome), with metadata.csv as id|text; else each take downloads, named by
  // its line number, which is fixed for each line.
  let dir = null;
  let loading = false;
  let unknownRows = '';
  const kept = new Map(); // line number -> text
  const canPick = typeof window.showDirectoryPicker === 'function';
  const folderName = $('voice-folder-name');
  if (!canPick) {
    $('voice-folder').hidden = true;
    folderName.textContent = 'This browser cannot write into a folder, so each take downloads; the number in its name is its line. Edge or Chrome can save into a folder.';
  }
  const loadKept = async () => {
    kept.clear();
    unknownRows = '';
    try {
      const file = await (await dir.getFileHandle('metadata.csv')).getFile();
      for (const raw of (await file.text()).match(/[^\n]*(?:\n|$)/g)) {
        if (!raw) continue;
        const row = raw.replace(/\r?\n$/, '');
        const bar = row.indexOf('|');
        const n = Number(row.slice(0, bar));
        if (bar > 0 && Number.isInteger(n) && n > 0) kept.set(n, row.slice(bar + 1));
        else unknownRows += raw;
      }
    } catch (e) {
      // Only a missing file means a new folder; never overwrite an unread one.
      if (e.name !== 'NotFoundError') throw e;
    }
  };
  const save = async (name, blob) => {
    if (dir) {
      const handle = await dir.getFileHandle(name, { create: true });
      const w = await handle.createWritable();
      await w.write(blob);
      await w.close();
      return;
    }
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 60000);
  };
  const saveKept = () => save('metadata.csv', new Blob(
    [unknownRows, unknownRows && !unknownRows.endsWith('\n') ? '\n' : '',
      [...kept].sort((a, b) => a[0] - b[0]).map(([n, t]) => pad(n) + '|' + t + '\n').join('')],
    { type: 'text/csv' }));
  const metadata = document.createElement('button');
  metadata.id = 'voice-metadata';
  metadata.textContent = 'Download metadata.csv';
  metadata.addEventListener('click', saveKept);
  folderName.after(metadata);
  $('voice-folder').addEventListener('click', async () => {
    if (saving || loading || starting || stopping || switching || talking) return;
    loading = true;
    show();
    try {
      dir = await window.showDirectoryPicker({ id: 'ada-voice', mode: 'readwrite' });
    } catch (e) {
      loading = false;
      show();
      return; // cancelled
    }
    // The page moves to another line now: a take held for this one must not
    // be kept under that one.
    if (recording) await stopTake();
    take = null;
    $('voice-note').textContent = '';
    try {
      await loadKept();
    } catch (e) {
      dir = null;
      folderName.textContent = 'Each take downloads; the number in its name is its line.';
      warn('The folder metadata could not be read: ' + e.message);
      return;
    } finally {
      loading = false;
      show();
    }
    folderName.textContent = 'Saving into "' + dir.name + '": ' + kept.size + ' of ' + lines.length + ' lines kept so far.';
    // A take finds its text by its number: if the lines have changed since,
    // say so rather than mix old takes with new words.
    const changed = [...kept].filter(([n, t]) => !lines[n - 1] || lines[n - 1].text !== t).length;
    warn(changed ? changed + ' kept line(s) in this folder no longer match the text shown for their number: record them again, or keep the old folder for the old text.' : '');
    at = firstOpen(0);
    show();
    // Off the folder button, whose Space would open the picker again.
    $('voice-rec').focus();
  });

  // ---- The microphone, as it is: the browser's echo cancellation, noise
  // suppression and automatic gain off, one channel, through an AudioWorklet.
  let ctx = null;
  let playing = 0;
  let stream = null;
  let source = null;
  let node = null;
  let onBlock = null; // where the samples go while something records
  let stopped = null;
  let stopping = false;
  let switching = false;
  let generation = 0; // changing mode invalidates an asynchronous start
  const release = async () => {
    if (stream) stream.getTracks().forEach((t) => t.stop());
    stream = null;
    if (source) source.disconnect();
    if (node) node.disconnect();
    source = node = null;
    if (ctx) await ctx.suspend();
  };
  const stopAudio = async () => {
    if (node && onBlock) {
      let timer;
      try {
        await Promise.race([
          new Promise((resolve) => {
            stopped = resolve;
            node.port.postMessage('stop');
          }),
          // An interrupted audio thread may never acknowledge Stop.
          new Promise((resolve) => { timer = setTimeout(resolve, 400); }),
        ]);
      } finally {
        clearTimeout(timer);
        stopped = null;
      }
    }
    onBlock = null;
    await release();
  };
  const open = async () => {
    stream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 1 },
    });
    const s = stream.getAudioTracks()[0].getSettings();
    if (s.echoCancellation || s.noiseSuppression || s.autoGainControl) {
      warn('The browser is still filtering the microphone, perhaps for another tab using it (a ChatGPT tab, say): recordings may sound processed. Close that tab, or talk to ChatGPT in its app or on a phone instead.');
    }
    // Kept only once the whole graph stands: a half-built one would make
    // every later take silently empty. On failure the microphone is let go.
    const c = ctx || new AudioContext();
    try {
      if (!ctx) await c.audioWorklet.addModule('/static/voice-worklet.js');
      await c.resume();
      node = new AudioWorkletNode(c, 'ada-capture');
      const silent = c.createGain();
      silent.gain.value = 0; // the graph must reach the speakers to run; nothing is heard
      source = c.createMediaStreamSource(stream);
      source.connect(node).connect(silent).connect(c.destination);
      node.port.onmessage = (e) => {
        if (e.data === null) {
          if (stopped) { const done = stopped; stopped = null; done(); }
        } else if (onBlock) onBlock(e.data);
      };
    } catch (e) {
      stream.getTracks().forEach((t) => t.stop());
      stream = null;
      c.close();
      ctx = null;
      throw e;
    }
    ctx = c;
  };
  // Starting a recording, once however fast Record is pressed: a second press
  // while the first is still opening the microphone would build a second
  // graph, and every take after would hold both streams' blocks.
  let starting = false;
  const ready = async () => {
    if (starting || stopping || loading || switching) return false;
    starting = true;
    const requested = generation;
    show();
    try {
      await open();
      if (requested !== generation) {
        await release();
        return false;
      }
      return true;
    } catch (e) {
      warn('The microphone could not be opened: ' + e.message);
      return false;
    } finally {
      starting = false;
      show();
    }
  };
  const levelOf = (block) => {
    let peak = 0;
    let sum = 0;
    for (const s of block) {
      const a = Math.abs(s);
      if (a > peak) peak = a;
      sum += s * s;
    }
    return { peak, rms: Math.sqrt(sum / block.length) };
  };
  const meter = (el, rms) => { el.value = Math.max(0, Math.min(1, (20 * Math.log10(rms + 1e-9) + 60) / 60)); };
  const samples = (chunks) => chunks.reduce((s, c) => s + c.length, 0);
  // A 16-bit PCM WAV of one channel.
  const wav = (chunks, rate) => {
    const n = samples(chunks);
    const buf = new ArrayBuffer(44 + n * 2);
    const v = new DataView(buf);
    const str = (o, s) => { for (let i = 0; i < s.length; i++) v.setUint8(o + i, s.charCodeAt(i)); };
    str(0, 'RIFF');
    v.setUint32(4, 36 + n * 2, true);
    str(8, 'WAVE');
    str(12, 'fmt ');
    v.setUint32(16, 16, true);
    v.setUint16(20, 1, true); // PCM
    v.setUint16(22, 1, true); // one channel
    v.setUint32(24, rate, true);
    v.setUint32(28, rate * 2, true);
    v.setUint16(32, 2, true);
    v.setUint16(34, 16, true);
    str(36, 'data');
    v.setUint32(40, n * 2, true);
    let o = 44;
    for (const c of chunks) {
      for (let i = 0; i < c.length; i++, o += 2) {
        const s = Math.max(-1, Math.min(1, c[i]));
        v.setInt16(o, s < 0 ? s * 0x8000 : s * 0x7fff, true);
      }
    }
    return new Blob([buf], { type: 'audio/wav' });
  };

  // ---- Read the lines.
  let at = 0; // index into lines
  let take = null; // { chunks, rate, peak }
  let recording = false;
  // A take being written: until it is, the line, the take and the folder stay
  // as they are, or it would be filed under another line or split between
  // two folders.
  let saving = false;
  const firstOpen = (from) => {
    for (let k = 0; k < lines.length; k++) {
      const i = (from + k) % lines.length;
      if (!kept.has(lines[i].n)) return i;
    }
    return Math.min(from, lines.length - 1);
  };
  const show = () => {
    const l = lines[at];
    $('voice-line').textContent = l.text;
    $('voice-count').textContent = 'Line ' + l.n + ' of ' + lines.length + ' · ' + kept.size + ' kept' +
      (kept.has(l.n) ? ' · this one is kept already: keeping a new take replaces it' : '');
    $('voice-rec').textContent = recording ? 'Stop (Space)' : 'Record (Space)';
    const busy = starting || stopping || loading || switching;
    $('voice-rec').disabled = busy || saving;
    $('voice-play').disabled = !take || recording || busy;
    $('voice-keep').disabled = !take || recording || saving || busy;
    $('voice-retake').disabled = !take || recording || saving || busy;
    $('voice-folder').disabled = busy || saving || !!talking;
    $('voice-talk-rec').disabled = busy;
    metadata.hidden = !!dir;
    metadata.disabled = saving || loading;
    document.body.classList.toggle('recording', recording || !!talking);
  };
  const judge = (t) => {
    const seconds = samples(t.chunks) / t.rate;
    const peak = 20 * Math.log10(t.peak + 1e-9);
    if (seconds < 0.6) return 'Too short: press Record, read the line, then Stop.';
    if (t.peak >= 0.99) return 'It clipped: too loud. Move back a little, or turn the microphone down, and retake.';
    if (peak < -30) return 'Very quiet: move closer, or turn the microphone up in the sound settings.';
    return 'Sounds good: Keep (Enter), or Play back (P) first.';
  };
  const startTake = async () => {
    if (saving || recording || $('voice-read').hidden || !(await ready())) return;
    take = { chunks: [], rate: ctx.sampleRate, peak: 0 };
    const t = take;
    onBlock = (b) => {
      t.chunks.push(b);
      const m = levelOf(b);
      t.peak = Math.max(t.peak, m.peak);
      meter($('voice-meter'), m.rms);
    };
    recording = true;
    node.port.postMessage('start');
    $('voice-note').textContent = 'Recording…';
    show();
  };
  const stopTake = async () => {
    if (stopping) return;
    stopping = true;
    show();
    try {
      await stopAudio();
    } finally {
      recording = false;
      stopping = false;
      $('voice-meter').value = 0;
      $('voice-note').textContent = take ? judge(take) : '';
      show();
    }
  };
  const keep = async () => {
    if (!take || recording || saving || starting || stopping || loading || switching) return;
    const l = lines[at];
    saving = true;
    show();
    try {
      await save(pad(l.n) + '.wav', wav(take.chunks, take.rate));
      kept.set(l.n, l.text);
      if (dir) await saveKept();
    } catch (e) {
      warn('The take could not be saved: ' + e.message);
      return;
    } finally {
      saving = false;
      show();
    }
    take = null;
    $('voice-note').textContent = 'Kept line ' + l.n + '.';
    at = firstOpen(at + 1);
    show();
  };
  const play = () => {
    if (!take || recording || !ctx || starting || stopping || loading || switching) return;
    ctx.resume();
    const buf = ctx.createBuffer(1, samples(take.chunks), take.rate);
    const data = buf.getChannelData(0);
    let o = 0;
    for (const c of take.chunks) {
      data.set(c, o);
      o += c.length;
    }
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.connect(ctx.destination);
    playing++;
    src.onended = () => { if (--playing === 0 && !onBlock && !starting) ctx.suspend(); };
    src.start();
  };
  const move = async (d) => {
    if (saving || starting || stopping || loading || switching) return;
    if (recording) await stopTake();
    take = null;
    $('voice-note').textContent = '';
    at = (at + d + lines.length) % lines.length;
    show();
  };
  $('voice-rec').addEventListener('click', () => (recording ? stopTake() : startTake()));
  $('voice-keep').addEventListener('click', keep);
  $('voice-retake').addEventListener('click', startTake);
  $('voice-play').addEventListener('click', play);
  $('voice-skip').addEventListener('click', () => move(1));
  $('voice-prev').addEventListener('click', () => move(-1));

  // ---- Just record me: one long recording, saved in parts of five minutes.
  let talking = null; // { chunks, part, stamp, started }
  let clock = null;
  const stamp = () => new Date().toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15);
  const savePart = async () => {
    if (!talking || !talking.chunks.length) return;
    const chunks = talking.chunks;
    talking.chunks = [];
    const name = 'talk-' + talking.stamp + '-' + String(talking.part++).padStart(2, '0') + '.wav';
    try {
      await save(name, wav(chunks, ctx.sampleRate));
      warn('');
      $('voice-talk-note').textContent = 'Saved ' + name + '.';
    } catch (e) {
      // Put the audio back: the next part, or Save again, carries it.
      if (talking) {
        talking.chunks = chunks.concat(talking.chunks);
        talking.part -= 1;
      }
      warn('A part could not be saved (' + e.message + '); its audio is kept, and saving it is tried again.');
    }
  };
  // One part after another: a part that fails puts its audio and its number
  // back before the next is named, and Stop waits for every part before it.
  let flushing = Promise.resolve();
  const flush = () => (flushing = flushing.then(savePart, savePart));
  const talkStart = async () => {
    if ($('voice-talk').hidden || talking || !(await ready())) return;
    talking = { chunks: [], part: 1, stamp: stamp(), started: Date.now() };
    const limit = ctx.sampleRate * 300;
    let held = 0;
    onBlock = (b) => {
      talking.chunks.push(b);
      held += b.length;
      meter($('voice-talk-meter'), levelOf(b).rms);
      if (held >= limit) {
        held = 0;
        flush();
      }
    };
    node.port.postMessage('start');
    $('voice-talk-rec').textContent = 'Stop';
    clock = setInterval(() => {
      const s = Math.floor((Date.now() - talking.started) / 1000);
      $('voice-talk-time').textContent = Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
    }, 1000);
    show();
  };
  const talkStop = async () => {
    if (stopping) return;
    stopping = true;
    show();
    await stopAudio();
    clearInterval(clock);
    $('voice-talk-meter').value = 0;
    try {
      await flush();
    } finally {
      stopping = false;
      show();
    }
    if (talking && talking.chunks.length) {
      $('voice-talk-rec').textContent = 'Save again'; // the last part is still only in memory
      return;
    }
    talking = null;
    $('voice-talk-rec').textContent = 'Start';
    show();
  };
  $('voice-talk-rec').addEventListener('click', () => (talking ? talkStop() : talkStart()));

  // ---- Modes and keys.
  const setMode = async (read) => {
    if (switching || stopping || loading) return;
    generation++;
    switching = true;
    show();
    try {
      if (recording) await stopTake();
      if (talking) await talkStop();
    } finally {
      switching = false;
    }
    $('voice-read').hidden = !read;
    $('voice-talk').hidden = read;
    $('voice-mode-read').setAttribute('aria-pressed', String(read));
    $('voice-mode-talk').setAttribute('aria-pressed', String(!read));
    show();
  };
  $('voice-mode-read').addEventListener('click', () => setMode(true));
  $('voice-mode-talk').addEventListener('click', () => setMode(false));
  document.addEventListener('keydown', (e) => {
    if (loading || switching || stopping) {
      if ([' ', 'Enter', 'r', 'R', 'p', 'P', 's', 'S', 'ArrowRight', 'ArrowLeft'].includes(e.key)) e.preventDefault();
      return;
    }
    // A held key repeats: it must not start and stop a take over and over.
    if ($('voice-read').hidden || e.ctrlKey || e.metaKey || e.altKey || e.repeat) return;
    // A control outside the reading card (the folder, the modes, the help)
    // keeps its own Space and Enter.
    if (e.target.closest && e.target.closest('button, summary, a, input, select, textarea') && !e.target.closest('#voice-read')) return;
    const k = e.key.toLowerCase();
    const act = { ' ': () => (recording ? stopTake() : startTake()), enter: keep, r: startTake, p: play, s: () => move(1), arrowright: () => move(1), arrowleft: () => move(-1) }[k];
    if (act) {
      e.preventDefault(); // Space and Enter would also press the focused button
      act();
    }
  });
  window.addEventListener('beforeunload', (e) => {
    if (recording || talking || take) e.preventDefault();
  });
  show();
})();
