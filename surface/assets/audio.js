// Audio for the WebSocket transport.
//
// This is the plumbing a WebRTC client never writes, and having it here is the
// argument for the adapter made concretely: capture, framing, playout
// scheduling, and an AudioContext-clock estimate of playback progress. The
// browser still does echo cancellation and noise suppression — those are
// requested from getUserMedia and belong to the client on either transport,
// because the server does neither and does not compensate for their absence.

export const SESSION_RATE = 24000;

const FRAME_MS = 20;

function toBase64(bytes) {
  let binary = "";
  for (let index = 0; index < bytes.length; index++) binary += String.fromCharCode(bytes[index]);
  return btoa(binary);
}

function fromBase64(text) {
  const binary = atob(text);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

export class Recorder extends EventTarget {
  #context = null;
  #stream = null;
  #node = null;
  #silence = null;
  #analyser = null;
  #samples = null;
  #muted = false;
  #muteSettled = false;

  async start() {
    this.#stream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    });
    this.#context = new AudioContext({ sampleRate: SESSION_RATE });
    await this.#context.audioWorklet.addModule("pcm-worklet.js");

    const source = this.#context.createMediaStreamSource(this.#stream);
    this.#node = new AudioWorkletNode(this.#context, "capture-processor");
    this.#node.port.onmessage = (message) => {
      if (this.#muted && this.#muteSettled) return;
      const bytes = this.#muted
        ? new Uint8Array(message.data.byteLength)
        : new Uint8Array(message.data.buffer);
      this.dispatchEvent(new CustomEvent("frame", {
        detail: toBase64(bytes),
      }));
    };
    source.connect(this.#node);
    // A worklet with no downstream connection is not pulled in every browser,
    // and a zero-gain sink is the standard way to keep it running without
    // routing the microphone back to the speakers — which, with echo
    // cancellation active, would be a feedback path the canceller then has to
    // remove.
    const silence = this.#context.createGain();
    silence.gain.value = 0;
    this.#node.connect(silence).connect(this.#context.destination);

    // Keep the capture graph clocked while the microphone track is disabled.
    // Chromium may stop pulling a MediaStream source altogether when its only
    // track is disabled. In that case the worklet emits no zero-valued frames,
    // so a server-side VAD that was open before mute never observes the
    // silence that closes the turn. A zero-valued constant source contributes
    // no audio while making the worklet's 20 ms cadence independent of device
    // activity; the microphone track still controls whether captured samples
    // can enter the graph.
    this.#silence = new ConstantSourceNode(this.#context, { offset: 0 });
    this.#silence.connect(this.#node);
    this.#silence.start();

    this.#analyser = this.#context.createAnalyser();
    this.#analyser.fftSize = 512;
    source.connect(this.#analyser);
    this.#samples = new Uint8Array(this.#analyser.frequencyBinCount);
  }

  // Muting disables the track rather than tearing capture down, and the
  // difference is not cosmetic. A client that simply stops sending audio
  // leaves the server's voice activity gate wherever it was: mid-utterance,
  // the floor stays held and the agent will not speak, because as far as the
  // session knows the person is still talking and has just gone quiet on the
  // wire. Sending silence is what lets the turn end.
  setMuted(muted) {
    this.#muted = Boolean(muted);
    this.#muteSettled = false;
    for (const track of this.#stream?.getAudioTracks() ?? []) {
      track.enabled = !this.#muted;
    }
  }

  get muted() {
    return this.#muted;
  }

  // Once the server confirms its VAD closed, a muted WebSocket client no
  // longer needs to keep sending zero frames. Until that confirmation the
  // silence is protocol data: stopping it early can strand the floor open.
  settleMute() {
    if (this.#muted) this.#muteSettled = true;
  }

  // level is the peak of the most recent window, 0 to 1.
  level() {
    if (!this.#analyser) return 0;
    this.#analyser.getByteTimeDomainData(this.#samples);
    let peak = 0;
    for (const sample of this.#samples) peak = Math.max(peak, Math.abs(sample - 128));
    return Math.min(1, peak / 64);
  }

  stop() {
    this.#node?.port && (this.#node.port.onmessage = null);
    try {
      this.#silence?.stop();
    } catch {
      // An already stopped AudioScheduledSourceNode needs no further cleanup.
    }
    this.#silence?.disconnect();
    this.#node?.disconnect();
    this.#stream?.getTracks().forEach((track) => track.stop());
    this.#context?.close();
    this.#context = this.#stream = this.#node = this.#silence = this.#analyser = null;
    this.#muted = this.#muteSettled = false;
  }
}

// Player schedules PCM16 deltas and keeps an AudioContext-clock estimate of
// how much of each utterance has progressed past its scheduled start.
//
// That number is not bookkeeping. When a person interrupts, the server has
// generated more speech than reached anyone, and the difference is the whole
// of the problem: a trajectory that recorded the generated text as spoken
// would have the agent believing it said things nobody heard. So playback
// reports a boundary in milliseconds and the client sends it back, which is
// what conversation.item.truncate is for.
export class Player extends EventTarget {
  #context = null;
  #gain = null;
  #playhead = 0;
  #sources = new Set();
  #utterance = null;

  ensure() {
    if (!this.#context) {
      this.#context = new AudioContext({ sampleRate: SESSION_RATE });
      this.#gain = this.#context.createGain();
      this.#gain.connect(this.#context.destination);
    }
    if (this.#context.state === "suspended") this.#context.resume();
    return this.#context;
  }

  // begin names the utterance now playing, so a later interruption knows what
  // it is truncating.
  begin(itemId) {
    if (this.#utterance?.itemId !== itemId) {
      this.#utterance = { itemId, queuedMs: 0, startedAt: null };
    }
  }

  enqueue(itemId, base64) {
    const context = this.ensure();
    this.begin(itemId);
    const bytes = fromBase64(base64);
    const samples = new Int16Array(bytes.buffer, bytes.byteOffset, Math.floor(bytes.byteLength / 2));
    if (samples.length === 0) return;

    const buffer = context.createBuffer(1, samples.length, SESSION_RATE);
    const channel = buffer.getChannelData(0);
    for (let index = 0; index < samples.length; index++) channel[index] = samples[index] / 0x8000;

    const now = context.currentTime;
    // A playhead behind the clock means the queue drained; restarting it at
    // "now" plus a frame keeps the next delta from being scheduled in the
    // past, which browsers render as an immediate burst.
    if (this.#playhead < now) this.#playhead = now + FRAME_MS / 1000;
    const source = context.createBufferSource();
    source.buffer = buffer;
    source.connect(this.#gain);
    source.start(this.#playhead);
    if (this.#utterance.startedAt === null) {
      this.#utterance.startedAt = this.#playhead;
      // This observes the scheduling call. It is not a first-sample render or
      // hardware playout boundary.
      this.dispatchEvent(new CustomEvent("first-audio-scheduled", { detail: itemId }));
    }
    this.#playhead += buffer.duration;
    this.#utterance.queuedMs += buffer.duration * 1000;

    this.#sources.add(source);
    source.addEventListener("ended", () => this.#sources.delete(source));
  }

  // playedMs is the AudioContext-clock estimate used for protocol truncation
  // after an interruption. It is not hardware-render evidence.
  playedMs() {
    if (!this.#context || !this.#utterance || this.#utterance.startedAt === null) return 0;
    const elapsed = (this.#context.currentTime - this.#utterance.startedAt) * 1000;
    return Math.max(0, Math.round(Math.min(elapsed, this.#utterance.queuedMs)));
  }

  playingItem() {
    return this.#utterance?.itemId ?? null;
  }

  // speaking reports whether audio is still on its way to the speakers.
  //
  // It is not the same question as whether an utterance exists. The
  // identifier outlives the sound: it is cleared when the server says the
  // turn's audio is done, which arrives after the last sample has played and
  // may never arrive at all if the response was cancelled. Interrupting on
  // that asks the server to stop something that already stopped - which it
  // correctly refuses, and the refusal is then noise a person has to explain
  // to themselves.
  //
  // The playhead is the honest measure, because it is where the last
  // scheduled buffer ends: ahead of the clock means sound is still coming.
  speaking() {
    if (!this.#context || !this.#utterance || this.#utterance.startedAt === null) {
      return false;
    }
    return this.#playhead > this.#context.currentTime;
  }

  // stop drops everything queued but not yet heard.
  stop() {
    for (const source of this.#sources) {
      try {
        source.stop();
      } catch {
        // Already ended; nothing to stop.
      }
    }
    this.#sources.clear();
    this.#playhead = 0;
  }

  finish(itemId) {
    if (this.#utterance?.itemId === itemId) this.#utterance = null;
  }

  close() {
    this.stop();
    this.#context?.close();
    this.#context = null;
    this.#utterance = null;
  }
}
