// Screen and camera, as protocol events.
//
// The client sends frames and makes no decision about which ones matter. That
// is not laziness and it is not a performance trade: selective perception is
// what the server is for, and a gate pushed into the client would mean every
// client reimplemented it differently or not at all, and the observations a
// session produced would stop being a property of the server.
//
// So this file does exactly three things the server cannot do for itself:
// declare the geometry, respect the negotiated limits, and stop sending when
// nobody asked for video.

const DEFAULT_LIMITS = { format: "jpeg", fps_cap: 3, max_dimension: 1280, max_frame_bytes: 4 << 20 };

// Quality is stepped down rather than fixed, because "1280 on the long edge"
// and "under N bytes" are two different constraints and a busy screen can
// satisfy the first while failing the second.
const QUALITY_STEPS = [0.72, 0.6, 0.5, 0.4, 0.3];

export class VideoSource extends EventTarget {
  #stream = null;
  #video = null;
  #canvas = document.createElement("canvas");
  #timer = null;
  #quality = 0;
  #limits = DEFAULT_LIMITS;
  #declared = null;
  #sending = false;

  constructor(name) {
    super();
    this.name = name;
  }

  get active() {
    return this.#stream !== null;
  }

  get thumbnail() {
    return this.#canvas;
  }

  applyLimits(limits) {
    this.#limits = { ...DEFAULT_LIMITS, ...(limits ?? {}) };
    if (this.#stream) this.#restartTimer();
  }

  async start(limits) {
    this.applyLimits(limits);
    this.#stream = this.name === "screen"
      ? await navigator.mediaDevices.getDisplayMedia({ video: { frameRate: 30 }, audio: false })
      : await navigator.mediaDevices.getUserMedia({ video: { facingMode: "user" }, audio: false });

    // A person ending the share from the browser's own control is a source
    // closing, and the session has to be told or it keeps a geometry that no
    // longer describes anything.
    this.#stream.getVideoTracks()[0].addEventListener("ended", () => this.stop());

    this.#video = document.createElement("video");
    this.#video.srcObject = this.#stream;
    this.#video.muted = true;
    this.#video.playsInline = true;
    await this.#video.play();
    await new Promise((resolve) => {
      if (this.#video.videoWidth > 0) return resolve();
      this.#video.addEventListener("loadedmetadata", resolve, { once: true });
    });

    this.#restartTimer();
  }

  #restartTimer() {
    clearInterval(this.#timer);
    const fps = Math.max(1, Math.min(this.#limits.fps_cap ?? 3, 10));
    this.#timer = setInterval(() => this.#capture(), 1000 / fps);
  }

  #geometry() {
    const width = this.#video.videoWidth;
    const height = this.#video.videoHeight;
    const longest = Math.max(width, height);
    const cap = this.#limits.max_dimension ?? 1280;
    const scale = longest > cap ? cap / longest : 1;
    return { width: Math.round(width * scale), height: Math.round(height * scale) };
  }

  async #capture() {
    if (!this.#video || this.#sending) return;
    const { width, height } = this.#geometry();
    if (width === 0 || height === 0) return;

    // Geometry is declared before frames and on every change, because a
    // computer-use coordinate is meaningless without the space the model saw.
    if (!this.#declared || this.#declared.width !== width || this.#declared.height !== height) {
      this.#declared = { width, height };
      this.dispatchEvent(new CustomEvent("source", {
        detail: { type: "openrealtime.input_video_source.update", source: this.name,
                  state: "active", width, height },
      }));
    }

    this.#canvas.width = width;
    this.#canvas.height = height;
    this.#canvas.getContext("2d").drawImage(this.#video, 0, 0, width, height);

    this.#sending = true;
    try {
      const frame = await this.#encode();
      if (!frame) return;
      this.dispatchEvent(new CustomEvent("frame", {
        detail: {
          type: "openrealtime.input_video_frame.append",
          source: this.name, frame, timestamp_ms: Date.now(),
        },
      }));
    } finally {
      this.#sending = false;
    }
  }

  async #encode() {
    const limit = this.#limits.max_frame_bytes ?? DEFAULT_LIMITS.max_frame_bytes;
    for (let step = this.#quality; step < QUALITY_STEPS.length; step++) {
      const blob = await new Promise((resolve) =>
        this.#canvas.toBlob(resolve, "image/jpeg", QUALITY_STEPS[step]));
      if (!blob) return null;
      if (blob.size <= limit) {
        // Remember what worked. A screen that needed stepping down once will
        // need it again, and re-encoding every frame twice to rediscover that
        // is work nobody sees the benefit of.
        this.#quality = step;
        return await this.#base64(blob);
      }
      this.dispatchEvent(new CustomEvent("throttled", {
        detail: { size: blob.size, limit, quality: QUALITY_STEPS[step] },
      }));
    }
    return null;
  }

  async #base64(blob) {
    const buffer = new Uint8Array(await blob.arrayBuffer());
    let binary = "";
    const block = 0x8000;
    for (let offset = 0; offset < buffer.length; offset += block) {
      binary += String.fromCharCode.apply(null, buffer.subarray(offset, offset + block));
    }
    return btoa(binary);
  }

  stop() {
    clearInterval(this.#timer);
    this.#timer = null;
    this.#stream?.getTracks().forEach((track) => track.stop());
    this.#video?.remove();
    this.#stream = null;
    this.#video = null;
    if (this.#declared) {
      this.#declared = null;
      this.dispatchEvent(new CustomEvent("source", {
        detail: { type: "openrealtime.input_video_source.update", source: this.name, state: "closed" },
      }));
    }
    this.dispatchEvent(new CustomEvent("ended"));
  }
}
