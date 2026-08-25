// Capture at the session's own rate, in the audio thread.
//
// The AudioContext is created at 24 kHz, so the browser resamples the
// microphone once, in native code, and what arrives here is already the rate
// the protocol wants. Doing it in JavaScript afterwards would be a second
// resample of worse quality, on the thread that also has to render the page.
//
// Frames are accumulated to a fixed size rather than forwarded at the render
// quantum, because 128 samples is 5.3 ms and a message per quantum is 188
// messages a second of mostly framing.
class CaptureProcessor extends AudioWorkletProcessor {
  static FRAME_SAMPLES = 480; // 20 ms at 24 kHz

  #buffer = new Int16Array(CaptureProcessor.FRAME_SAMPLES);
  #filled = 0;

  process(inputs) {
    const channel = inputs[0]?.[0];
    if (!channel) return true;
    for (let index = 0; index < channel.length; index++) {
      // Clamp before scaling: a sample past unity would wrap to the opposite
      // extreme, which is heard as a click rather than as clipping.
      const sample = Math.max(-1, Math.min(1, channel[index]));
      this.#buffer[this.#filled++] = sample < 0 ? sample * 0x8000 : sample * 0x7fff;
      if (this.#filled === this.#buffer.length) {
        this.port.postMessage(this.#buffer.slice());
        this.#filled = 0;
      }
    }
    return true;
  }
}

registerProcessor("capture-processor", CaptureProcessor);
