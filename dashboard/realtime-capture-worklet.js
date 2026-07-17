// AudioWorklet processor for dashboard voice capture. This file is copied as a
// standalone same-origin module by build.ts; it intentionally has no imports.
const TARGET_SAMPLE_RATE = 24_000;
const FRAME_SAMPLES = 480; // 20 ms at 24 kHz.

class AptevaPCMCapture extends AudioWorkletProcessor {
  constructor() {
    super();
    this.pending = [];
    this.cursor = 0;
  }

  process(inputs, outputs) {
    // Keep the connected output silent. The output exists only to keep Safari
    // pulling this node as part of the active audio graph.
    const output = outputs[0] && outputs[0][0];
    if (output) output.fill(0);

    const input = inputs[0] && inputs[0][0];
    if (!input || input.length === 0) return true;

    const ratio = sampleRate / TARGET_SAMPLE_RATE;
    let cursor = this.cursor;
    while (cursor < input.length) {
      const index = Math.min(input.length - 1, Math.floor(cursor));
      const sample = Math.max(-1, Math.min(1, input[index]));
      this.pending.push(sample < 0 ? sample * 32768 : sample * 32767);
      cursor += ratio;

      if (this.pending.length >= FRAME_SAMPLES) {
        const pcm = new Int16Array(this.pending.splice(0, FRAME_SAMPLES));
        this.port.postMessage(pcm.buffer, [pcm.buffer]);
      }
    }
    this.cursor = cursor - input.length;
    return true;
  }
}

registerProcessor("apteva-pcm-capture", AptevaPCMCapture);
