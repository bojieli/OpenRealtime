// Two ways into the same session.
//
// Both of these speak the identical protocol; what differs is only what the
// browser has to do for itself. That is the whole reason the surface offers
// both rather than picking one: over WebSocket this page implements resampling
// and playout scheduling and gets full-rate audio for it, and over WebRTC the
// browser's own stack does jitter, loss concealment, and echo cancellation and
// the synthesised voice arrives at telephone bandwidth. Neither is simply
// better, and hearing the difference back to back is the point.

const CHUNK_MAGIC = [0x4f, 0x52, 0x54, 0x43]; // "ORTC"
const CHUNK_VERSION = 1;
const CHUNK_HEADER_BYTES = 14;
const CHUNK_PAYLOAD_BYTES = 16 * 1024;

// A data channel message is one event, as text. Anything the peer cannot take
// whole crosses as a sequence of binary chunk frames instead — a screen frame
// is base64 and does not fit in one SCTP message on any browser. See
// docs/transports.md; this is the client half of that framing.
function splitChunks(identifier, bytes) {
  const count = Math.max(1, Math.ceil(bytes.length / CHUNK_PAYLOAD_BYTES));
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

  accept(buffer) {
    const bytes = new Uint8Array(buffer);
    if (bytes.length < CHUNK_HEADER_BYTES) return null;
    for (let index = 0; index < 4; index++) {
      if (bytes[index] !== CHUNK_MAGIC[index]) return null;
    }
    if (bytes[4] !== CHUNK_VERSION) return null;
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const identifier = view.getUint32(6);
    const index = view.getUint16(10);
    const count = view.getUint16(12);
    if (count === 0 || index >= count) return null;

    let message = this.#partial.get(identifier);
    if (!message) {
      message = { chunks: new Array(count), received: 0, bytes: 0 };
      this.#partial.set(identifier, message);
    }
    if (message.chunks.length !== count || message.chunks[index]) {
      this.#partial.delete(identifier);
      return null;
    }
    message.chunks[index] = bytes.slice(CHUNK_HEADER_BYTES);
    message.received++;
    message.bytes += bytes.length - CHUNK_HEADER_BYTES;
    if (message.received !== count) return null;

    this.#partial.delete(identifier);
    const complete = new Uint8Array(message.bytes);
    let offset = 0;
    for (const chunk of message.chunks) {
      complete.set(chunk, offset);
      offset += chunk.length;
    }
    return new TextDecoder().decode(complete);
  }
}

class Transport extends EventTarget {
  emit(name, detail) {
    this.dispatchEvent(new CustomEvent(name, { detail }));
  }

  deliver(text) {
    let event;
    try {
      event = JSON.parse(text);
    } catch {
      this.emit("malformed", text);
      return;
    }
    this.emit("event", event);
  }
}

// The WebSocket path goes through the surface's own origin, which is what lets
// the credential stay in that process and never reach this page. It is a
// byte-for-byte relay, so what this class speaks is the protocol exactly as a
// server-to-server client would.
export class WebSocketTransport extends Transport {
  #socket = null;
  name = "websocket";
  carriesMedia = false;

  async connect() {
    const url = new URL("/api/session", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    this.#socket = new WebSocket(url);
    this.#socket.addEventListener("message", (message) => this.deliver(message.data));
    this.#socket.addEventListener("close", () => this.emit("closed"));
    this.#socket.addEventListener("error", () => this.emit("failed", "the session socket failed"));
    await new Promise((resolve, reject) => {
      this.#socket.addEventListener("open", resolve, { once: true });
      this.#socket.addEventListener("error", () => reject(new Error("could not open the session")), { once: true });
    });
  }

  send(event) {
    if (this.#socket?.readyState !== WebSocket.OPEN) return false;
    this.#socket.send(JSON.stringify(event));
    return true;
  }

  close() {
    this.#socket?.close();
    this.#socket = null;
  }
}

// The WebRTC path terminates media in the browser and carries events on the
// data channel the Realtime API already names. Only the offer and answer go
// through the surface; ICE and the media itself are direct.
export class WebRTCTransport extends Transport {
  #connection = null;
  #channel = null;
  #microphone = null;
  #speaker = null;
  #assembler = new Reassembler();
  #outbound = 0;
  name = "webrtc";
  carriesMedia = true;

  async connect() {
    this.#microphone = await navigator.mediaDevices.getUserMedia({
      audio: {
        // The client's responsibility and the browser's speciality. The server
        // does neither and does not compensate for their absence.
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
    });

    this.#connection = new RTCPeerConnection();
    this.#connection.addTrack(this.#microphone.getAudioTracks()[0], this.#microphone);

    this.#speaker = new Audio();
    this.#speaker.autoplay = true;
    this.#connection.addEventListener("track", (event) => {
      this.#speaker.srcObject = event.streams[0];
      this.emit("media", { kind: "audio" });
    });

    this.#channel = this.#connection.createDataChannel("oai-events");
    this.#channel.binaryType = "arraybuffer";
    this.#channel.addEventListener("message", (message) => {
      if (typeof message.data === "string") {
        this.deliver(message.data);
        return;
      }
      const complete = this.#assembler.accept(message.data);
      if (complete !== null) this.deliver(complete);
    });

    this.#connection.addEventListener("connectionstatechange", () => {
      const state = this.#connection.connectionState;
      this.emit("peerstate", state);
      if (["failed", "closed", "disconnected"].includes(state)) this.emit("closed");
    });

    const offer = await this.#connection.createOffer();
    await this.#connection.setLocalDescription(offer);
    await new Promise((resolve) => {
      if (this.#connection.iceGatheringState === "complete") return resolve();
      this.#connection.addEventListener("icegatheringstatechange", () => {
        if (this.#connection.iceGatheringState === "complete") resolve();
      });
    });

    const response = await fetch("/api/webrtc", {
      method: "POST",
      headers: { "Content-Type": "application/sdp" },
      body: this.#connection.localDescription.sdp,
    });
    if (!response.ok) {
      throw new Error(`the adapter refused the offer: ${response.status} ${await response.text()}`);
    }
    await this.#connection.setRemoteDescription({ type: "answer", sdp: await response.text() });

    await new Promise((resolve, reject) => {
      if (this.#channel.readyState === "open") return resolve();
      this.#channel.addEventListener("open", resolve, { once: true });
      setTimeout(() => reject(new Error("the data channel never opened")), 15000);
    });
  }

  // The negotiated maximum is what the peer will accept in one message. A
  // message that fits is sent whole, exactly as before chunking existed.
  #maxMessageBytes() {
    const negotiated = this.#connection?.sctp?.maxMessageSize ?? 0;
    return negotiated > 0 ? negotiated : 64 * 1024;
  }

  send(event) {
    if (this.#channel?.readyState !== "open") return false;
    const text = JSON.stringify(event);
    const bytes = new TextEncoder().encode(text);
    if (bytes.length <= this.#maxMessageBytes() - CHUNK_HEADER_BYTES) {
      this.#channel.send(text);
      return true;
    }
    this.#outbound = (this.#outbound + 1) >>> 0;
    for (const frame of splitChunks(this.#outbound, bytes)) {
      this.#channel.send(frame);
    }
    return true;
  }

  // Muting on this transport is the same idea as on the other one and for the
  // same reason: the track keeps flowing and carries silence, so the server's
  // voice activity gate hears a person who stopped talking rather than a
  // connection that stopped sending. Disabling the track is what a browser
  // does natively, so the silence is generated below the encoder.
  setMuted(muted) {
    for (const track of this.#microphone?.getAudioTracks() ?? []) {
      track.enabled = !muted;
    }
  }

  get muted() {
    const track = this.#microphone?.getAudioTracks()?.[0];
    return track ? !track.enabled : false;
  }

  // What one SCTP message may carry, as the two peers actually negotiated it.
  // Worth showing a developer: it is the number that decides whether a frame
  // crosses whole or in chunks, it differs by browser, and it is the first
  // thing to look at when video works in one and not another.
  maxMessageBytes() {
    return this.#maxMessageBytes();
  }

  // Buffered bytes are the honest measure of whether video is keeping up on
  // this transport: the data channel is reliable and ordered, so a source
  // producing faster than SCTP drains shows here as a growing queue rather
  // than as dropped frames.
  bufferedBytes() {
    return this.#channel?.bufferedAmount ?? 0;
  }

  close() {
    this.#channel?.close();
    this.#connection?.close();
    this.#microphone?.getTracks().forEach((track) => track.stop());
    if (this.#speaker) this.#speaker.srcObject = null;
    this.#channel = null;
    this.#connection = null;
    this.#microphone = null;
  }
}

export function createTransport(name) {
  return name === "webrtc" ? new WebRTCTransport() : new WebSocketTransport();
}
