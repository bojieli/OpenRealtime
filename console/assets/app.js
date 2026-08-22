import { createTransport } from "./transport.js";
import { Recorder, Player } from "./audio.js";
import { VideoSource } from "./video.js";
import { ToolBridge, declarations } from "./tools.js";
import * as ui from "./ui.js";

let config = { tools: [], webrtc: false, root: "", endpoint: "" };
let transport = null;
let recorder = null;
let player = null;
let tools = null;
const sources = new Map();

// The session's own view of what is happening, which is not the same as the
// UI's. Endpoint timing in particular has to be measured against the server's
// own report of when the turn ended, or the number measures this page's
// scheduling rather than the system's latency.
let endpointAt = null;
let firstAudioAt = null;
let negotiated = null;
let levelTimer = null;

const DEFAULT_INSTRUCTIONS =
  "You are a concise voice assistant helping a developer at their machine. " +
  "Answer briefly and expand when asked. When you use a tool, say what you are " +
  "doing in a few words rather than narrating each step.";

function defaultSession() {
  const session = {
    type: "realtime",
    instructions: DEFAULT_INSTRUCTIONS,
    audio: {
      input: { format: { type: "audio/pcm", rate: 24000 } },
      output: { format: { type: "audio/pcm", rate: 24000 } },
    },
    openrealtime: {
      version: 1,
      supports: ["video.input", "observations", "computer_use"],
    },
  };
  if (config.tools.length) session.tools = declarations(config.tools);
  return session;
}

// The WebRTC adapter owns the audio format, because it terminates media. A
// client that also stated one would be describing a connection it does not
// have, and the adapter drops the field for exactly that reason — so the
// console does not send it rather than sending something to be ignored.
function sessionForTransport(session, transportName) {
  if (transportName !== "webrtc") return session;
  const { audio, ...rest } = session;
  return rest;
}

function send(event) {
  if (!transport?.send(event)) return false;
  ui.recordEvent("out", event);
  return true;
}

// --- connection -------------------------------------------------------------

async function connect() {
  ui.elements.connect.disabled = true;
  ui.elements.transport.disabled = true;
  const name = ui.elements.transport.value;
  ui.clearLog();
  ui.resetStats();
  endpointAt = firstAudioAt = negotiated = null;

  try {
    if (config.tools.length) {
      tools = new ToolBridge(ui.askConfirmation);
      await tools.connect();
    }

    ui.setState(name === "webrtc" ? "negotiating" : "connecting", "working");
    transport = createTransport(name);
    transport.addEventListener("event", (message) => handle(message.detail));
    transport.addEventListener("closed", () => disconnect("the session closed"));
    transport.addEventListener("failed", (message) => ui.notice(message.detail));
    transport.addEventListener("peerstate", (message) => ui.setStat("peer connection", message.detail));
    await transport.connect();

    let session = defaultSession();
    try {
      session = JSON.parse(ui.elements.sessionJson.value);
    } catch {
      ui.notice("the session JSON did not parse, so the default was sent instead");
    }
    send({ type: "session.update", session: sessionForTransport(session, name) });

    if (!transport.carriesMedia) {
      player = new Player();
      player.addEventListener("first-audio", noteFirstAudio);
      recorder = new Recorder();
      recorder.addEventListener("frame", (message) => {
        send({ type: "input_audio_buffer.append", audio: message.detail });
      });
      await recorder.start();
      ui.setPressed(ui.elements.mic, true);
      levelTimer = setInterval(() => {
        ui.elements.level.style.width = `${(recorder?.level() ?? 0) * 100}%`;
      }, 60);
    } else {
      ui.setPressed(ui.elements.mic, true);
      ui.setMediaState("audio on the media track · mu-law, 8 kHz out");
      if (transport.maxMessageBytes) {
        const limit = transport.maxMessageBytes();
        ui.setStat("data channel max message", `${Math.round(limit / 1024)} KiB`);
      }
    }

    ui.setState("connected", "live");
    ui.elements.disconnect.disabled = false;
    ui.elements.mic.disabled = false;
    ui.elements.screen.disabled = false;
    ui.elements.camera.disabled = false;
  } catch (failure) {
    ui.setState(`could not connect: ${failure.message}`, "failed");
    ui.notice(failure.message);
    await disconnect();
  }
}

async function disconnect(reason) {
  for (const source of sources.values()) source.stop();
  sources.clear();
  clearInterval(levelTimer);
  levelTimer = null;
  recorder?.stop();
  player?.close();
  tools?.close();
  transport?.close();
  recorder = player = tools = transport = null;

  ui.elements.connect.disabled = false;
  ui.elements.transport.disabled = false;
  ui.elements.disconnect.disabled = true;
  ui.elements.mic.disabled = true;
  ui.elements.screen.disabled = true;
  ui.elements.camera.disabled = true;
  ui.setPressed(ui.elements.mic, false);
  ui.setPressed(ui.elements.screen, false);
  ui.setPressed(ui.elements.camera, false);
  ui.elements.level.style.width = "0";
  ui.elements.thumbs.innerHTML = "";
  ui.setMediaState("");
  if (reason) ui.setState(reason, "");
  else ui.setState("not connected", "");
}

function noteFirstAudio() {
  if (firstAudioAt !== null || endpointAt === null) return;
  firstAudioAt = performance.now();
  ui.setStat("first audio after endpoint", `${Math.round(firstAudioAt - endpointAt)} ms`);
}

// --- protocol ---------------------------------------------------------------

function handle(event) {
  ui.recordEvent("in", event);

  switch (event.type) {
    case "session.created":
      ui.setStat("session", event.session?.id ?? "");
      break;

    case "session.updated":
      negotiated = event.session?.openrealtime ?? null;
      describeNegotiation(negotiated);
      for (const source of sources.values()) source.applyLimits(negotiated?.video);
      break;

    case "input_audio_buffer.speech_started":
      ui.setState("listening", "live");
      // The person started talking over the agent. What was generated past
      // this moment was never heard, and saying so is the client's job: the
      // server knows what it sent, only this page knows what came out of the
      // speakers.
      interrupt();
      break;

    case "input_audio_buffer.speech_stopped":
      endpointAt = performance.now();
      firstAudioAt = null;
      ui.setState("thinking", "working");
      break;

    case "conversation.item.input_audio_transcription.completed":
      ui.setTurn(event.item_id, "you", event.transcript);
      break;

    case "response.created":
      ui.setState("responding", "working");
      break;

    case "response.output_audio_transcript.delta":
      ui.appendTurn(event.item_id, "agent", event.delta, "agent");
      break;

    case "response.output_audio.delta":
      player?.enqueue(event.item_id, event.delta);
      break;

    case "response.output_audio.done":
      player?.finish(event.item_id);
      break;

    case "openrealtime.observation.added":
      // Rendered unlike speech on purpose: this is something the agent read,
      // not something anyone said.
      ui.setTurn(event.observation_id, `observed · ${event.observer}`, event.text, "observed");
      break;

    case "response.function_call_arguments.done":
      runTool(event);
      break;

    case "response.done":
      // A turn that produced nothing says why, and this is the one place a
      // person is looking for the answer that did not arrive. Rendering it
      // where the reply would have been is the difference between "the agent
      // ignored me" and "the output limit was reached"; the server's log
      // names the knob, and a client only gets the reason.
      //
      // Only when the person would not otherwise know. A cancelled turn is
      // barge-in - they interrupted, deliberately, and it is the most ordinary
      // thing that happens in a voice session. Telling them something went
      // wrong because they did the thing the system exists to support would
      // make the console cry wolf on every interruption.
      if (event.response?.status) {
        const status = event.response.status;
        const reason = event.response.status_details?.reason;
        ui.setStat("last turn", reason ? `${status} · ${reason}` : status);
        if (status === "incomplete" || status === "failed") {
          ui.notice(`the turn produced no output — ${status}: ${reason ?? "no reason given"}`);
        }
      }
      ui.setState("connected", "live");
      break;

    case "conversation.item.truncated":
      ui.setStat("last truncation", `${event.audio_end_ms} ms of ${event.item_id}`);
      break;

    case "error":
      ui.notice(event.error?.message ?? "the server reported an error");
      ui.setState("error", "failed");
      break;
  }
}

function describeNegotiation(extension) {
  if (!extension) {
    ui.elements.negotiated.textContent =
      "No extension in the reply. This is an ordinary Realtime session: voice only, no video, no observations.";
    ui.elements.screen.disabled = true;
    ui.elements.camera.disabled = true;
    return;
  }
  const enabled = extension.enabled ?? [];
  const video = extension.video;
  ui.elements.negotiated.textContent =
    `Enabled: ${enabled.join(", ") || "nothing"}.` +
    (video ? ` Video: ${video.format}, up to ${video.fps_cap}/s, ${video.max_dimension}px long edge,` +
             ` ${Math.round((video.max_frame_bytes ?? 0) / 1024)} KiB a frame.` : "");
  const canSendVideo = enabled.includes("video.input");
  ui.elements.screen.disabled = !canSendVideo;
  ui.elements.camera.disabled = !canSendVideo;
  if (!canSendVideo) {
    ui.setStat("video", "not enabled for this session");
  }
}

// interrupt tells the server where playback actually stopped.
//
// Both halves matter. output_audio_buffer.clear stops what is queued on the
// server, and conversation.item.truncate says how much of the utterance was
// heard — without the second, the trajectory records speech nobody received as
// something the agent said, and every later turn reasons from a conversation
// that did not happen.
function interrupt() {
  // Nothing to interrupt unless audio is actually still on its way. A person
  // starting to talk while the agent is silent is just a person talking.
  if (!player?.speaking()) return;
  const itemId = player.playingItem();
  const heard = player.playedMs();
  player.stop();
  if (!itemId) return;
  send({ type: "output_audio_buffer.clear" });
  send({ type: "conversation.item.truncate", item_id: itemId, content_index: 0, audio_end_ms: heard });
}

// --- tools ------------------------------------------------------------------

async function runTool(event) {
  const id = `call-${event.call_id}`;
  ui.setTurn(id, "tool call", `${event.name}(${event.arguments})`, "tool");

  if (!tools?.connected) {
    ui.setTurnOutcome(id, "failed");
    send({
      type: "conversation.item.create",
      item: {
        type: "function_call_output", call_id: event.call_id,
        output: JSON.stringify({ error: "this console has no tool host attached" }),
      },
    });
    return;
  }

  const started = performance.now();
  const result = await tools.call(event.call_id, event.name, event.arguments);
  const took = Math.round(performance.now() - started);
  ui.setStat("last tool round trip", `${took} ms`);

  if (result.error) {
    ui.setTurnOutcome(id, result.error.includes("declined") ? "declined" : "failed");
    ui.appendTurn(id, "tool call", `\n\n${result.error}`, "tool");
  } else {
    ui.setTurnOutcome(id, "done");
    const preview = result.output.length > 400 ? `${result.output.slice(0, 400)}…` : result.output;
    ui.appendTurn(id, "tool call", `\n\n${preview}`, "tool");
  }

  // Whatever happened, the session hears about it. A call left unanswered is
  // the one outcome that leaves the model waiting on something that is never
  // coming — and the server now fails it on a deadline for exactly that
  // reason, so silence here would only turn a fast error into a slow one.
  send({
    type: "conversation.item.create",
    item: {
      type: "function_call_output", call_id: event.call_id,
      output: result.error ? JSON.stringify({ error: result.error }) : result.output,
    },
  });
}

// --- video ------------------------------------------------------------------

async function toggleSource(name, button) {
  const existing = sources.get(name);
  if (existing) {
    existing.stop();
    return;
  }
  const source = new VideoSource(name);
  source.addEventListener("source", (message) => send(message.detail));
  source.addEventListener("frame", (message) => {
    if (!send(message.detail)) return;
    if (transport?.bufferedBytes) {
      const buffered = transport.bufferedBytes();
      ui.setStat("data channel queue", `${Math.round(buffered / 1024)} KiB`);
      // Whether a frame crossed whole or in pieces is the difference between
      // video working and video silently not working on a peer with a small
      // limit, so the console says which happened rather than leaving it to be
      // inferred from a capture.
      const size = message.detail.frame.length;
      ui.setStat("last frame",
        `${Math.round(size / 1024)} KiB, sent ${size <= transport.maxMessageBytes() ? "whole" : "in chunks"}`);
    }
  });
  source.addEventListener("throttled", (message) => {
    const { size, limit, quality } = message.detail;
    ui.setStat(`${name} encoding`,
      `${Math.round(size / 1024)} KiB exceeded ${Math.round(limit / 1024)} KiB, retrying at q${quality}`);
  });
  source.addEventListener("ended", () => {
    sources.delete(name);
    ui.setPressed(button, false);
    ui.hideThumbnail(name);
  });

  try {
    await source.start(negotiated?.video);
    sources.set(name, source);
    ui.setPressed(button, true);
    ui.showThumbnail(name, source.thumbnail);
  } catch (failure) {
    ui.notice(`could not start ${name}: ${failure.message}`);
  }
}

// --- wiring -----------------------------------------------------------------

async function load() {
  const response = await fetch("/api/config");
  config = await response.json();
  ui.renderTools(config.root, config.tools);
  ui.elements.sessionJson.value = JSON.stringify(defaultSession(), null, 2);
  if (!config.webrtc) {
    const option = ui.elements.transport.querySelector('option[value="webrtc"]');
    option.disabled = true;
    option.textContent = "WebRTC (no adapter configured)";
  }
  ui.setStat("endpoint", config.endpoint);
  ui.setStat("credential", config.authenticated ? "held by the console" : "none");
}

ui.elements.connect.addEventListener("click", connect);
ui.elements.disconnect.addEventListener("click", () => disconnect());
ui.elements.screen.addEventListener("click", () => toggleSource("screen", ui.elements.screen));
ui.elements.camera.addEventListener("click", () => toggleSource("camera", ui.elements.camera));
ui.elements.clearEvents.addEventListener("click", ui.clearEvents);
ui.elements.saveEvents.addEventListener("click", ui.saveTranscript);

ui.elements.mic.addEventListener("click", () => {
  // Whichever side owns capture owns muting: the recorder on the WebSocket
  // path, the peer connection's own track on the WebRTC one.
  const microphone = recorder ?? transport;
  if (!microphone?.setMuted) return;
  const muted = !microphone.muted;
  microphone.setMuted(muted);
  ui.setPressed(ui.elements.mic, !muted);
  ui.elements.mic.textContent = muted ? "Microphone (muted)" : "Microphone";
  if (muted) ui.elements.level.style.width = "0";
});

ui.elements.compose.addEventListener("submit", (submission) => {
  submission.preventDefault();
  const text = ui.elements.typed.value.trim();
  if (!text || !transport) return;
  // A client that types is the same participant as a client that speaks; the
  // difference is the transport rather than the provenance.
  send({
    type: "conversation.item.create",
    item: { type: "message", role: "user", content: [{ type: "input_text", text }] },
  });
  send({ type: "response.create" });
  ui.setTurn(`typed-${Date.now()}`, "you", text, "user");
  ui.elements.typed.value = "";
});

ui.elements.applySession.addEventListener("click", () => {
  let session;
  try {
    session = JSON.parse(ui.elements.sessionJson.value);
  } catch (failure) {
    ui.notice(`the session JSON did not parse: ${failure.message}`);
    return;
  }
  if (!send({ type: "session.update", session: sessionForTransport(session, transport?.name) })) {
    ui.notice("not connected, so nothing was sent");
  }
});

ui.elements.resetSession.addEventListener("click", () => {
  ui.elements.sessionJson.value = JSON.stringify(defaultSession(), null, 2);
});

for (const tab of document.querySelectorAll(".tabs button")) {
  tab.addEventListener("click", () => {
    document.querySelectorAll(".tabs button").forEach((other) => other.classList.remove("active"));
    document.querySelectorAll(".panel").forEach((panel) => panel.classList.remove("active"));
    tab.classList.add("active");
    document.getElementById(`tab-${tab.dataset.tab}`).classList.add("active");
  });
}

window.addEventListener("beforeunload", () => transport?.close());

load().catch((failure) => ui.notice(`could not load the console configuration: ${failure.message}`));
