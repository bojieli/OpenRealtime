const VIDEO_DEFAULTS = Object.freeze({
  format: "jpeg", fps_cap: 3, max_dimension: 1280, max_frame_bytes: 4 << 20,
});
const VIDEO_QUALITY = Object.freeze([0.72, 0.60, 0.50, 0.40, 0.30]);

function videoLimits(value) {
  const limits = { ...VIDEO_DEFAULTS, ...(value ?? {}) };
  if (limits.format !== "jpeg" || !Number.isInteger(limits.fps_cap) ||
      limits.fps_cap < 1 || limits.fps_cap > 10 ||
      !Number.isInteger(limits.max_dimension) || limits.max_dimension < 64 || limits.max_dimension > 4096 ||
      !Number.isInteger(limits.max_frame_bytes) || limits.max_frame_bytes < 1024 ||
      limits.max_frame_bytes > (4 << 20)) {
    throw new Error("negotiated video limits are invalid or exceed the browser ceiling");
  }
  return Object.freeze(limits);
}

async function base64(blob) {
  const bytes = new Uint8Array(await blob.arrayBuffer());
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode.apply(null, bytes.subarray(offset, offset + 0x8000));
  }
  return btoa(binary);
}

export default {
  name: "openrealtime.presentation.client.webrtc-media",
  revision: 1,
  async mount(context) {
    for (const operation of ["microphone", "playout"]) {
      if (!context.permissions.allows("device.media", "browser-audio", operation)) {
        throw new Error(`browser media lacks its ${operation} deployment grant`);
      }
    }
    for (const operation of ["camera", "screen"]) {
      if (!context.permissions.allows("device.media", "browser-video", operation)) {
        throw new Error(`browser media lacks its ${operation} deployment grant`);
      }
    }
    let microphone;
    let mediaGeneration = 0;
    let microphoneMuted = false;
    let speakerMuted = false;
    let speaker;
    let microphoneSender;
    let captureStartedAt = 0;
    let playoutAttachedAt = 0;
    let captureSerial = 0;
    const captures = new Map();

    const stopVideo = (source) => {
      const capture = captures.get(source);
      if (!capture) return;
      captures.delete(source);
      capture.stopped = true;
      clearTimeout(capture.timer);
      capture.stream?.getTracks().forEach((track) => track.stop());
      capture.video?.remove();
      capture.video.srcObject = null;
      if (capture.opened) {
        capture.observer(Object.freeze({ kind: "closed", source, generation: capture.generation }));
      }
    };

    const encodeFrame = async (capture) => {
      for (let quality = capture.quality; quality < VIDEO_QUALITY.length; quality++) {
        const blob = await new Promise((resolve) =>
          capture.canvas.toBlob(resolve, "image/jpeg", VIDEO_QUALITY[quality]));
        if (!blob) throw new Error("browser did not encode the captured video frame");
        if (blob.size <= capture.limits.max_frame_bytes) {
          capture.quality = quality;
          return blob;
        }
      }
      return null;
    };

    const captureFrame = async (capture) => {
      if (capture.stopped || captures.get(capture.source) !== capture) return;
      const started = performance.now();
      try {
        const width = capture.video.videoWidth;
        const height = capture.video.videoHeight;
        const longest = Math.max(width, height);
        if (width > 0 && height > 0 && longest > 0) {
          const scale = longest > capture.limits.max_dimension
            ? capture.limits.max_dimension / longest : 1;
          const geometry = {
            width: Math.max(1, Math.round(width * scale)),
            height: Math.max(1, Math.round(height * scale)),
          };
          if (!capture.geometry || capture.geometry.width !== geometry.width ||
              capture.geometry.height !== geometry.height) {
            capture.geometry = geometry;
            capture.opened = true;
            capture.observer(Object.freeze({
              kind: "opened", source: capture.source, generation: capture.generation, ...geometry,
            }));
          }
          capture.canvas.width = geometry.width;
          capture.canvas.height = geometry.height;
          capture.canvas.getContext("2d", { alpha: false }).drawImage(
            capture.video, 0, 0, geometry.width, geometry.height);
          const blob = await encodeFrame(capture);
          if (blob && !capture.stopped && captures.get(capture.source) === capture) {
            const frame = await base64(blob);
            if (!capture.stopped && captures.get(capture.source) === capture) {
              capture.frames++;
              capture.lastFrameBytes = blob.size;
              capture.lastEncodeMS = performance.now() - started;
              capture.observer(Object.freeze({
                kind: "frame", source: capture.source, generation: capture.generation,
                width: geometry.width, height: geometry.height, frame,
                captured_at_ms: Date.now(), bytes: blob.size,
              }));
            }
          } else if (!blob) {
            capture.drops++;
            capture.observer(Object.freeze({
              kind: "dropped", source: capture.source, generation: capture.generation,
              reason: "encoded frame exceeded negotiated byte limit",
            }));
          }
        }
      } catch (error) {
        capture.failures++;
        capture.observer(Object.freeze({
          kind: "failed", source: capture.source, generation: capture.generation,
          error: error?.message ?? String(error),
        }));
        stopVideo(capture.source);
        return;
      }
      if (!capture.stopped && captures.get(capture.source) === capture) {
        const interval = 1000 / capture.limits.fps_cap;
        capture.timer = setTimeout(() => captureFrame(capture),
          Math.max(0, interval - (performance.now() - started)));
      }
    };

    const release = () => {
      mediaGeneration++;
      microphoneSender = undefined;
      for (const source of [...captures.keys()]) stopVideo(source);
      microphone?.getTracks().forEach((track) => track.stop());
      microphone = undefined;
      if (speaker) {
        speaker.pause();
        speaker.srcObject = null;
        speaker.remove();
        speaker = undefined;
      }
    };
    const media = Object.freeze({
      async microphone() {
        if (microphone?.active) return microphone;
        const generation = mediaGeneration;
        const acquired = await navigator.mediaDevices.getUserMedia({
          audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
          video: false,
        });
        if (generation !== mediaGeneration) { acquired.getTracks().forEach((track) => track.stop()); throw new Error("Microphone capture was cancelled."); }
        microphone = acquired;
        microphone.getAudioTracks().forEach((track) => { track.enabled = !microphoneMuted; });
        captureStartedAt = performance.now();
        return microphone;
      },
      attachRemote(stream) {
        if (!(stream instanceof MediaStream)) throw new Error("remote media is not a MediaStream");
        if (!speaker) {
          speaker = document.createElement("audio");
          speaker.autoplay = true;
          speaker.hidden = true;
          speaker.dataset.openrealtimeMedia = "remote-audio";
        }
        speaker.muted = speakerMuted;
        speaker.srcObject = stream;
        playoutAttachedAt = performance.now();
        speaker.play().catch(() => {});
      },
      async startVideo(source, negotiated, observer) {
        if (!["camera", "screen"].includes(source) || typeof observer !== "function") {
          throw new Error("video capture requires a camera/screen source and observer");
        }
        stopVideo(source);
        const limits = videoLimits(negotiated);
        const generation = ++captureSerial;
        const capture = {
          source, generation, limits, observer, stopped: false, opened: false,
          stream: null, video: document.createElement("video"),
          canvas: document.createElement("canvas"), timer: undefined,
          geometry: null, quality: 0, frames: 0, drops: 0, failures: 0,
          lastFrameBytes: 0, lastEncodeMS: 0,
        };
        captures.set(source, capture);
        try {
          capture.stream = source === "screen"
            ? await navigator.mediaDevices.getDisplayMedia({ video: { frameRate: 30 }, audio: false })
            : await navigator.mediaDevices.getUserMedia({
                video: { facingMode: "user", frameRate: { ideal: 30 } }, audio: false,
              });
          if (capture.stopped || captures.get(source) !== capture) {
            capture.stream.getTracks().forEach((track) => track.stop());
            throw new Error("video capture was superseded");
          }
          capture.video.srcObject = capture.stream;
          capture.video.muted = true;
          capture.video.playsInline = true;
          await capture.video.play();
          const track = capture.stream.getVideoTracks()[0];
          if (!track) throw new Error("video capture returned no video track");
          track.addEventListener("ended", () => stopVideo(source), { once: true });
          captureFrame(capture);
          return Object.freeze({ source, generation, stop: () => stopVideo(source) });
        } catch (error) {
          if (captures.get(source) === capture) stopVideo(source);
          throw error;
        }
      },
      bindMicrophoneSender(sender) { microphoneSender = sender; },
      async setMicrophoneMuted(value) {
        const muted = Boolean(value);
        if (!muted && !microphone?.active) {
          const stream = await media.microphone();
          if (microphoneSender) await microphoneSender.replaceTrack(stream.getAudioTracks()[0]);
        }
        microphoneMuted = muted;
        microphone?.getAudioTracks().forEach((track) => { track.enabled = !microphoneMuted; });
      },
      setSpeakerMuted(value) {
        speakerMuted = Boolean(value);
        if (speaker) speaker.muted = speakerMuted;
      },
      streams() {
        return { microphone, remote: speaker?.srcObject,
          camera: captures.get("camera")?.stream, screen: captures.get("screen")?.stream };
      },
      stopVideo,
      release,
      snapshot() {
        const video = {};
        for (const [source, capture] of captures) {
          video[source] = Object.freeze({
            generation: capture.generation,
            tracks: capture.stream?.getVideoTracks().filter((track) => track.readyState === "live").length ?? 0,
            frames: capture.frames, drops: capture.drops, failures: capture.failures,
            last_frame_bytes: capture.lastFrameBytes,
            last_encode_ms: Math.round(capture.lastEncodeMS * 10) / 10,
            geometry: capture.geometry ? Object.freeze({ ...capture.geometry }) : null,
          });
        }
        return Object.freeze({
          microphone_muted: microphoneMuted, speaker_muted: speakerMuted,
          microphone_tracks: microphone?.getAudioTracks().filter((track) => track.readyState === "live").length ?? 0,
          remote_tracks: speaker?.srcObject?.getAudioTracks().filter((track) => track.readyState === "live").length ?? 0,
          capture_started_at_ms: Math.floor(captureStartedAt),
          playout_attached_at_ms: Math.floor(playoutAttachedAt),
          video: Object.freeze(video),
        });
      },
    });
    context.publish("presentation.client.media", media);
    context.lifecycle.defer("browser-media", release);
  },
};
