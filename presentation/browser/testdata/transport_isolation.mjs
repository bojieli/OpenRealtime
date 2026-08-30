import { pathToFileURL } from "node:url";

const [websocketPath, webrtcPath] = process.argv.slice(2);
if (!websocketPath || !webrtcPath) throw new Error("transport module paths are required");
const websocketPlugin = (await import(pathToFileURL(websocketPath))).default;
const webrtcPlugin = (await import(pathToFileURL(webrtcPath))).default;

globalThis.location = new URL("http://127.0.0.1:8767/");

class EventTargetFixture {
  constructor() { this.listeners = new Map(); }
  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) ?? [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }
  removeEventListener(type, listener) {
    this.listeners.set(type, (this.listeners.get(type) ?? []).filter((candidate) => candidate !== listener));
  }
  emit(type, event = {}) {
    for (const listener of [...(this.listeners.get(type) ?? [])]) listener(event);
  }
}

class SocketFixture extends EventTargetFixture {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances = [];
  constructor(url) {
    super();
    this.url = url;
    this.readyState = SocketFixture.CONNECTING;
    SocketFixture.instances.push(this);
  }
  open() { this.readyState = SocketFixture.OPEN; this.emit("open"); }
  close() { this.readyState = SocketFixture.CLOSED; this.emit("close"); }
  send() {}
}
globalThis.WebSocket = SocketFixture;

let websocket;
const websocketDisposers = [];
await websocketPlugin.mount({
  manifest: { endpoints: [{ name: "realtime.websocket", method: "GET", path: "/client/v1/realtime" }] },
  permissions: { allows: (kind, resource, operation) =>
    kind === "network.connect" && resource === "host-realtime" && operation === "websocket" },
  publish(name, value) {
    if (name !== "presentation.client.connection") throw new Error(`unexpected WebSocket service ${name}`);
    websocket = value;
  },
  lifecycle: { defer(_name, dispose) { websocketDisposers.push(dispose); } },
});
const websocketStates = [];
const websocketMessages = [];
websocket.onState(() => { throw new Error("faulty WebSocket state consumer"); });
websocket.onState((state) => websocketStates.push(state));
websocket.subscribe(() => { throw new Error("faulty WebSocket message consumer"); });
websocket.subscribe((message) => websocketMessages.push(message));
const websocketConnect = websocket.connect();
SocketFixture.instances[0].open();
await websocketConnect;
SocketFixture.instances[0].emit("message", { data: "accepted" });
if (websocket.state() !== "connected" || websocketMessages[0] !== "accepted" ||
    !websocketStates.includes("connected")) {
  throw new Error("a faulty WebSocket subscriber affected the transport or a sibling");
}
for (const dispose of [...websocketDisposers].reverse()) await dispose();

class ChannelFixture extends EventTargetFixture {
  constructor() {
    super();
    this.readyState = "open";
    this.bufferedAmount = 0;
    this.sent = [];
  }
  send(value) { this.sent.push(value); }
  close() { this.readyState = "closed"; }
}

class PeerFixture extends EventTargetFixture {
  static instances = [];
  constructor() {
    super();
    this.connectionState = "new";
    this.iceConnectionState = "new";
    this.signalingState = "stable";
    this.iceGatheringState = "complete";
    this.localDescription = null;
    this.channel = new ChannelFixture();
    PeerFixture.instances.push(this);
  }
  addTrack() {}
  createDataChannel() { return this.channel; }
  async createOffer() { return { type: "offer", sdp: "fixture-offer" }; }
  async setLocalDescription(value) { this.localDescription = value; }
  async setRemoteDescription() { this.connectionState = "connected"; }
  async getStats() { return new Map(); }
  close() { this.connectionState = "closed"; }
}
globalThis.RTCPeerConnection = PeerFixture;
globalThis.fetch = async () => ({ ok: true, status: 200, text: async () => "fixture-answer" });

let releases = 0;
const media = {
  microphone: async () => ({ getAudioTracks: () => [] }),
  attachRemote() {},
  release() { releases++; },
  snapshot: () => ({ audio: { state: "ready" }, video: { active: [] } }),
};
let webrtc;
const webrtcDisposers = [];
await webrtcPlugin.mount({
  manifest: { endpoints: [{ name: "realtime.webrtc", method: "POST", path: "/client/v1/realtime/calls" }] },
  permissions: { allows: (kind, resource, operation) =>
    kind === "network.connect" && resource === "host-realtime" && operation === "webrtc" },
  services: { get: (name) => name === "presentation.client.media" ? media : undefined },
  publish(name, value) {
    if (name === "presentation.client.connection") webrtc = value;
  },
  lifecycle: { defer(_name, dispose) { webrtcDisposers.push(dispose); } },
});
const webrtcStates = [];
const webrtcMessages = [];
webrtc.onState(() => { throw new Error("faulty WebRTC state consumer"); });
webrtc.onState((state) => webrtcStates.push(state));
webrtc.subscribe(() => { throw new Error("faulty WebRTC message consumer"); });
webrtc.subscribe((message) => webrtcMessages.push(message));
await webrtc.connect();
PeerFixture.instances[0].channel.emit("message", { data: "accepted" });
if (webrtc.state() !== "connected" || webrtcMessages[0] !== "accepted" ||
    !webrtcStates.includes("connected")) {
  throw new Error("a faulty WebRTC subscriber affected the transport or a sibling");
}
for (const dispose of [...webrtcDisposers].reverse()) await dispose();
if (releases === 0) throw new Error("WebRTC disposal did not release its media provider");

console.log("transport subscriber isolation passed");
