const CHUNK_MAGIC = [0x4f, 0x52, 0x54, 0x43];
const CHUNK_VERSION = 1;
const CHUNK_HEADER_BYTES = 14;
const CHUNK_PAYLOAD_BYTES = 16 * 1024;
const MAX_EVENT_BYTES = 16 << 20;

function splitChunks(identifier, bytes) {
  const count = Math.max(1, Math.ceil(bytes.length / CHUNK_PAYLOAD_BYTES));
  if (count > 0xffff) throw new Error("WebRTC event exceeds chunk count limit");
  const frames = [];
  for (let index = 0; index < count; index++) {
    const slice = bytes.subarray(index * CHUNK_PAYLOAD_BYTES, (index + 1) * CHUNK_PAYLOAD_BYTES);
    const frame = new Uint8Array(CHUNK_HEADER_BYTES + slice.length);
    frame.set(CHUNK_MAGIC, 0);
    frame[4] = CHUNK_VERSION;
    frame[5] = index === count - 1 ? 1 : 0;
    const view = new DataView(frame.buffer);
    view.setUint32(6, identifier);
    view.setUint16(10, index);
    view.setUint16(12, count);
    frame.set(slice, CHUNK_HEADER_BYTES);
    frames.push(frame);
  }
  return frames;
}

class Reassembler {
  #partial = new Map();
  #bytes = 0;

  clear() { this.#partial.clear(); this.#bytes = 0; }

  accept(buffer) {
    const bytes = new Uint8Array(buffer);
    if (bytes.length < CHUNK_HEADER_BYTES || bytes.length > CHUNK_HEADER_BYTES + CHUNK_PAYLOAD_BYTES) {
      throw new Error("invalid WebRTC chunk size");
    }
    for (let index = 0; index < CHUNK_MAGIC.length; index++) {
      if (bytes[index] !== CHUNK_MAGIC[index]) throw new Error("invalid WebRTC chunk magic");
    }
    if (bytes[4] !== CHUNK_VERSION || bytes[5] > 1) throw new Error("unsupported WebRTC chunk framing");
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const identifier = view.getUint32(6);
    const index = view.getUint16(10);
    const count = view.getUint16(12);
    if (count === 0 || index >= count || (bytes[5] === 1) !== (index === count - 1)) {
      throw new Error("invalid WebRTC chunk coordinates");
    }
    let message = this.#partial.get(identifier);
    if (!message) {
      if (this.#partial.size >= 64) throw new Error("too many partial WebRTC events");
      message = { chunks: new Array(count), received: 0, bytes: 0 };
      this.#partial.set(identifier, message);
    }
    if (message.chunks.length !== count || message.chunks[index]) {
      this.#bytes -= message.bytes;
      this.#partial.delete(identifier);
      throw new Error("inconsistent or duplicate WebRTC chunk");
    }
    const payload = bytes.slice(CHUNK_HEADER_BYTES);
    if (this.#bytes + payload.length > MAX_EVENT_BYTES) {
      this.clear();
      throw new Error("partial WebRTC events exceed memory limit");
    }
    message.chunks[index] = payload;
    message.received++;
    message.bytes += payload.length;
    this.#bytes += payload.length;
    if (message.received !== count) return null;

    this.#partial.delete(identifier);
    this.#bytes -= message.bytes;
    const complete = new Uint8Array(message.bytes);
    let offset = 0;
    for (const chunk of message.chunks) {
      complete.set(chunk, offset);
      offset += chunk.length;
    }
    return new TextDecoder("utf-8", { fatal: true }).decode(complete);
  }
}

function waitFor(target, success, failures, timeoutMS, label, signal) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => finish(new Error(`${label} timed out`)), timeoutMS);
    const finish = (error) => {
      clearTimeout(timer);
      target.removeEventListener(success, succeeded);
      for (const event of failures) target.removeEventListener(event, failed);
      signal?.removeEventListener("abort", aborted);
      error ? reject(error) : resolve();
    };
    const succeeded = () => finish();
    const failed = () => finish(new Error(`${label} failed`));
    const aborted = () => finish(new Error(`${label} was cancelled`));
    target.addEventListener(success, succeeded, { once: true });
    for (const event of failures) target.addEventListener(event, failed, { once: true });
    signal?.addEventListener("abort", aborted, { once: true });
    if (signal?.aborted) aborted();
  });
}

export default {
  name: "openrealtime.presentation.client.webrtc",
  revision: 1,
  async mount(context) {
    if (!context.permissions.allows("network.connect", "host-realtime", "webrtc")) {
      throw new Error("WebRTC transport lacks its deployment grant");
    }
    const endpoint = context.manifest.endpoints?.find((candidate) => candidate.name === "realtime.webrtc");
    if (!endpoint || endpoint.method !== "POST") throw new Error("WebRTC endpoint is not declared");
    const media = context.services.get("presentation.client.media");
    if (!media) throw new Error("browser media service is unavailable");

    let peer;
    let channel;
    let connecting;
    let attempt;
    let state = "idle";
    let stateDetail = "";
    let outbound = 0;
    const assembler = new Reassembler();
    const listeners = new Set();
    const states = new Set();
    const notifyState = () => {
      for (const listener of states) {
        try { listener(state, stateDetail); } catch {}
      }
    };
    const notifyMessage = (message) => {
      for (const listener of listeners) {
        try { listener(message); } catch {}
      }
    };
    const setState = (next, detail = "") => { state = next; stateDetail = detail; notifyState(); };
    const release = () => {
      attempt?.abort();
      attempt = undefined;
      const oldChannel = channel;
      const oldPeer = peer;
      channel = undefined;
      peer = undefined;
      assembler.clear();
      try { oldChannel?.close(); } catch {}
      try { oldPeer?.close(); } catch {}
      media.release();
    };

    const connection = Object.freeze({
      kind: "webrtc",
      state: () => state,
      onState(listener) {
        if (typeof listener !== "function") throw new Error("transport state listener must be a function");
        states.add(listener);
        try { listener(state, stateDetail); } catch {}
        return () => states.delete(listener);
      },
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("transport message listener must be a function");
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
      async connect() {
        if (channel?.readyState === "open" && peer?.connectionState === "connected") return;
        if (connecting) return connecting;
        connecting = (async () => {
          setState("connecting");
          const controller = new AbortController();
          attempt = controller;
          const candidatePeer = new RTCPeerConnection();
          peer = candidatePeer;
          try {
            // Reserve an audio sender so text-only users can enable their
            // microphone later without renegotiating this one-shot session.
            const audio = candidatePeer.addTransceiver("audio", { direction: "sendrecv" });
            media.bindMicrophoneSender(audio.sender);
            try {
              const stream = await media.microphone();
              if (peer !== candidatePeer) throw new Error("WebRTC connection was superseded");
              await audio.sender.replaceTrack(stream.getAudioTracks()[0]);
            } catch (error) {
              if (!["NotAllowedError", "NotFoundError"].includes(error.name)) throw error;
              await media.setMicrophoneMuted(true);
            }
            candidatePeer.addEventListener("track", (event) => media.attachRemote(event.streams[0]));
            const candidateChannel = candidatePeer.createDataChannel("oai-events");
            candidateChannel.binaryType = "arraybuffer";
            channel = candidateChannel;
            candidateChannel.addEventListener("message", (message) => {
              try {
                if (typeof message.data === "string") {
                  if (new TextEncoder().encode(message.data).length > MAX_EVENT_BYTES) {
                    throw new Error("WebRTC event exceeds limit");
                  }
                  notifyMessage(message.data);
                  return;
                }
                const complete = assembler.accept(message.data);
                if (complete !== null) notifyMessage(complete);
              } catch (error) {
                release(); setState("failed", error?.message ?? "invalid WebRTC event");
              }
            });
            candidatePeer.addEventListener("connectionstatechange", () => {
              if (peer !== candidatePeer) return;
              const physical = candidatePeer.connectionState;
              if (physical === "connected") setState("connected");
              if (["failed", "disconnected", "closed"].includes(physical)) {
                const detail = `WebRTC ${physical}; ICE ${candidatePeer.iceConnectionState}; signaling ${candidatePeer.signalingState}`;
                release(); setState(physical === "failed" ? "failed" : "closed", detail);
              }
            });
            const offer = await candidatePeer.createOffer();
            await candidatePeer.setLocalDescription(offer);
            if (candidatePeer.iceGatheringState !== "complete") {
              await new Promise((resolve, reject) => {
                const timer = setTimeout(() => finish(new Error("ICE gathering timed out")), 15000);
                const finish = (error) => {
                  clearTimeout(timer);
                  candidatePeer.removeEventListener("icegatheringstatechange", changed);
                  controller.signal.removeEventListener("abort", aborted);
                  error ? reject(error) : resolve();
                };
                const changed = () => {
                  if (candidatePeer.iceGatheringState === "complete") finish();
                };
                const aborted = () => finish(new Error("ICE gathering was cancelled"));
                candidatePeer.addEventListener("icegatheringstatechange", changed);
                controller.signal.addEventListener("abort", aborted, { once: true });
                changed();
              });
            }
            const response = await fetch(endpoint.path, {
              method: "POST", headers: { "Content-Type": "application/sdp" },
              body: candidatePeer.localDescription.sdp, signal: controller.signal,
            });
            const answer = await response.text();
            if (!response.ok || answer.length === 0 || answer.length > (1 << 20)) {
              throw new Error(`WebRTC offer was refused with ${response.status}`);
            }
            await candidatePeer.setRemoteDescription({ type: "answer", sdp: answer });
            if (candidateChannel.readyState !== "open") {
              await waitFor(candidateChannel, "open", ["close", "error"], 15000,
                "WebRTC data channel", controller.signal);
            }
            if (peer !== candidatePeer || channel !== candidateChannel) {
              throw new Error("WebRTC connection was superseded");
            }
            setState("connected");
          } catch (error) {
            if (peer === candidatePeer) {
              release(); setState("failed", error?.message ?? "WebRTC connection failed");
            }
            throw error;
          } finally {
            if (attempt === controller) attempt = undefined;
          }
        })();
        const current = connecting;
        try { await current; } finally { if (connecting === current) connecting = undefined; }
      },
      send(event) {
        if (!channel || channel.readyState !== "open") throw new Error("WebRTC data channel is not connected");
        const text = typeof event === "string" ? event : JSON.stringify(event);
        const bytes = new TextEncoder().encode(text);
        if (bytes.length > MAX_EVENT_BYTES) throw new Error("WebRTC event exceeds limit");
        if (bytes.length <= CHUNK_PAYLOAD_BYTES) channel.send(text);
        else for (const frame of splitChunks(++outbound >>> 0, bytes)) channel.send(frame);
      },
      close() { release(); setState("closed"); },
      mediaSnapshot: () => media.snapshot(),
    });
    context.publish("presentation.client.connection", connection);
    const diagnostics = Object.freeze({
      async snapshot() {
        const result = {
          state, detail: stateDetail,
          data_channel_buffered_bytes: channel?.bufferedAmount ?? 0,
          audio_bytes_received: 0, audio_packets_received: 0, audio_packets_lost: 0,
          video_bytes_received: 0, round_trip_time_ms: 0,
          media: media.snapshot(),
        };
        const current = peer;
        if (!current || current.connectionState === "closed") return Object.freeze(result);
        const reports = await current.getStats();
        reports.forEach((report) => {
          if (report.type === "inbound-rtp" && report.isRemote !== true) {
            const kind = report.kind ?? report.mediaType;
            if (kind === "audio") {
              result.audio_bytes_received += Number(report.bytesReceived ?? 0);
              result.audio_packets_received += Number(report.packetsReceived ?? 0);
              result.audio_packets_lost += Number(report.packetsLost ?? 0);
            } else if (kind === "video") {
              result.video_bytes_received += Number(report.bytesReceived ?? 0);
            }
          }
          if (report.type === "candidate-pair" && report.state === "succeeded" &&
              Number.isFinite(report.currentRoundTripTime)) {
            result.round_trip_time_ms = Math.max(result.round_trip_time_ms,
              Math.round(report.currentRoundTripTime * 10000) / 10);
          }
        });
        return Object.freeze(result);
      },
    });
    context.publish("presentation.client.transport_diagnostics", diagnostics);
    context.lifecycle.defer("webrtc", () => connection.close());
  },
};
