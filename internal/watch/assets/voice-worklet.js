// The microphone's samples, as they are: the audio thread hands them to the
// recording page in blocks of 2048, rather than one message per 128.
'use strict';
class AdaCapture extends AudioWorkletProcessor {
  constructor() {
    super();
    this.block = new Float32Array(2048);
    this.used = 0;
  }

  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (ch) {
      let i = 0;
      while (i < ch.length) {
        const n = Math.min(ch.length - i, this.block.length - this.used);
        this.block.set(ch.subarray(i, i + n), this.used);
        this.used += n;
        i += n;
        if (this.used === this.block.length) {
          this.port.postMessage(this.block);
          this.block = new Float32Array(2048);
          this.used = 0;
        }
      }
    }
    return true;
  }
}
registerProcessor('ada-capture', AdaCapture);
