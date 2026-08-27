import { createTransport } from "./transport.js";
import { Recorder, Player } from "./audio.js";
import { VideoSource, RemoteVideoSource } from "./video.js";
import { ToolBridge, declarations, channelOf } from "./tools.js";
import { ArtifactPanel, DownloadPanel } from "./artifacts.js";
import { Channels, CHANNELS } from "./channels.js";
import { DebugTimeline } from "./timeline.js";

const element = (id) => document.getElementById(id);
const elements = Object.fromEntries([
  "transport", "connect", "disconnect", "state", "dot", "meters",
  "observation-cards", "action-cards", "artifact-tabs", "artifact-host", "downloads",
  "events", "filter-media", "clear-events", "save-events",
  "timeline", "timeline-filter", "timeline-summary", "clear-timeline",
  "session-json", "apply-session", "reset-session", "negotiated",
  "instructions", "apply-instructions", "instructions-state",
  "stats", "tools-root", "tool-list",
  "mic", "level", "screen", "camera", "browser", "endturn",
  "compose", "typed", "media-state",
  "confirm", "confirm-requirement", "confirm-name", "confirm-arguments",
].map((id) => [id.replace(/-(.)/g, (_, character) => character.toUpperCase()), element(id)]));

let config = { tools: [], webrtc: false, root: "", endpoint: "", browser: { available: false } };
let transport = null;
let recorder = null;
let player = null;
let tools = null;
let channels = null;
let artifacts = null;
let downloads = null;
let timeline = null;
const sources = new Map();
const transcript = [];

// The session's own view of what is happening, which is not the same as the
// page's. Endpoint timing in particular has to be measured against the
// server's report of when the turn ended, or the number measures this page's
// scheduling rather than the system's latency.
let endpointAt = null;
let firstAudioAt = null;
let negotiated = null;
let manualTurns = false;
let responseOpen = false;
let levelTimer = null;

const DEFAULT_INSTRUCTIONS =
  "You are a realtime assistant being tested end to end. You can hear, see a shared screen, " +
  "see a camera pointed at the room, see and click a browser, run tools on this machine, and " +
  "render HTML artifacts. Answer briefly. When something is better looked at than listened to — " +
  "a table, a comparison, a chart, a form — call display_artifact and say one sentence about it " +
  "rather than reading it out. When you act on a screen, name what you are doing in a few words.";

// Which observation channel a video source's frames and observations belong
// to. The map is here rather than derived from the name, because a source name
// is the client's own vocabulary and the protocol says so.
const SOURCE_CHANNEL = { screen: "obs.screen", camera: "obs.camera", browser: "obs.browser" };

function defaultSession() {
  const session = {
    type: "realtime",
    instructions: elements.instructions.value || DEFAULT_INSTRUCTIONS,
    audio: {
      input: { format: { type: "audio/pcm", rate: 24000 } },
      output: { format: { type: "audio/pcm", rate: 24000 } },
    },
    openrealtime: {
      version: 1,
      supports: ["video.input", "observations", "computer_use"],
      observers: ["audio", "video"],
      debug: { enabled: true, include_payloads: false },
    },
  };
  if (config.tools.length) session.tools = declarations(config.tools);
  return session;
}

// The WebRTC adapter owns the audio format, because it terminates media. A
// client that also stated one would be describing a connection it does not
// have, and the adapter drops the field for exactly that reason — so this page
// does not send it rather than sending something to be ignored.
function sessionForTransport(session, transportName) {
  if (transportName !== "webrtc") return session;
  const { audio, ...rest } = session;
  return rest;
}

function send(event) {
  if (!transport?.send(event)) return false;
  recordEvent("out", event);
  return true;
}

// --- the inspector ----------------------------------------------------------

// Payloads are elided rather than truncated. A saved session is what a useful
// bug report contains, and a megabyte of base64 in the middle of it is what
// makes one unreadable.
function elide(event) {
  const copy = { ...event };
  for (const field of ["audio", "delta", "frame"]) {
    if (typeof copy[field] === "string" && copy[field].length > 64) {
      copy[field] = `‹${copy[field].length} chars›`;
    }
  }
  return copy;
}

const MEDIA_EVENTS = new Set([
  "input_audio_buffer.append", "response.output_audio.delta",
  "openrealtime.input_video_frame.append",
]);

function recordEvent(direction, event) {
  transcript.push({ direction, at: new Date().toISOString(), event: elide(event) });
  if (elements.filterMedia.checked && MEDIA_EVENTS.has(event.type)) return;
  const line = document.createElement("div");
  line.className = direction;
  line.textContent = `${direction === "out" ? "▲" : "▼"} ${JSON.stringify(elide(event))}`;
  elements.events.append(line);
  while (elements.events.children.length > 500) elements.events.firstChild.remove();
  elements.events.scrollTop = elements.events.scrollHeight;
}

function setStat(name, value) {
  let entry = elements.stats.querySelector(`dd[data-stat="${CSS.escape(name)}"]`);
  if (!entry) {
    const term = document.createElement("dt");
    term.textContent = name;
    entry = document.createElement("dd");
    entry.dataset.stat = name;
    elements.stats.append(term, entry);
  }
  entry.textContent = value;
}

function setState(text, state = "") {
  elements.state.textContent = text;
  elements.dot.dataset.state = state;
}

function notice(message) {
  channels?.record("obs.tools", { title: "surface", body: message, kind: "mono" });
  setStat("last notice", message);
}

function setPressed(button, pressed) {
  button.setAttribute("aria-pressed", pressed ? "true" : "false");
}

// --- connection -------------------------------------------------------------

async function connect() {
  elements.connect.disabled = true;
  elements.transport.disabled = true;
  const name = elements.transport.value;
  channels.reset();
  artifacts.clear();
  downloads.clear();
  elements.artifactHost.querySelector(".placeholder")?.removeAttribute("hidden");
  transcript.length = 0;
  endpointAt = firstAudioAt = negotiated = null;

  try {
    if (config.tools.length) {
      tools = new ToolBridge(askConfirmation);
      await tools.connect();
    }

    setState(name === "webrtc" ? "negotiating" : "connecting", "working");
    transport = createTransport(name);
    transport.addEventListener("event", (message) => handle(message.detail));
    transport.addEventListener("closed", () => disconnect("the session closed"));
    transport.addEventListener("failed", (message) => notice(message.detail));
    transport.addEventListener("peerstate", (message) => setStat("peer connection", message.detail));
    await transport.connect();

    let session = defaultSession();
    try {
      session = JSON.parse(elements.sessionJson.value);
    } catch {
      notice("the session JSON did not parse, so the default was sent instead");
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
      setPressed(elements.mic, true);
      channels.setState("obs.audio", "live");
      levelTimer = setInterval(() => {
        elements.level.style.width = `${(recorder?.level() ?? 0) * 100}%`;
      }, 60);
    } else {
      setPressed(elements.mic, true);
      channels.setState("obs.audio", "live");
      elements.mediaState.textContent = "audio on the media track · mu-law, 8 kHz out";
      if (transport.maxMessageBytes) {
        const limit = transport.maxMessageBytes();
        setStat("data channel max message", `${Math.round(limit / 1024)} KiB`);
      }
    }

    setState("connected", "live");
    elements.disconnect.disabled = false;
    for (const control of [elements.mic, elements.screen, elements.camera]) control.disabled = false;
    elements.browser.disabled = !config.browser.available;
  } catch (failure) {
    setState(`could not connect: ${failure.message}`, "failed");
    notice(failure.message);
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

  elements.connect.disabled = false;
  elements.transport.disabled = false;
  elements.disconnect.disabled = true;
  for (const control of [elements.mic, elements.screen, elements.camera,
                         elements.browser, elements.endturn]) {
    control.disabled = true;
  }
  manualTurns = false;
  for (const toggle of [elements.mic, elements.screen, elements.camera, elements.browser]) {
    setPressed(toggle, false);
  }
  elements.level.style.width = "0";
  elements.mediaState.textContent = "";
  for (const definition of CHANNELS) channels.setState(definition.id, "");
  setState(reason ?? "not connected", "");
  if (reason) reportCoverage();
}

// reportCoverage says which channels carried something, and it is the answer
// to the question this page exists to ask. A run where the camera never sent a
// frame and the artifact channel never fired looks, in a transcript, exactly
// like a run where they did.
function reportCoverage() {
  const summary = channels.summary();
  const carried = summary.filter((channel) => channel.count > 0);
  setStat("channels exercised", `${carried.length} of ${summary.length}`);
  const silent = summary.filter((channel) => channel.count === 0).map((channel) => channel.label);
  setStat("silent channels", silent.length ? silent.join(", ") : "none");
}

function noteFirstAudio() {
  if (firstAudioAt !== null || endpointAt === null) return;
  firstAudioAt = performance.now();
  setStat("first audio after endpoint", `${Math.round(firstAudioAt - endpointAt)} ms`);
}

// --- protocol ---------------------------------------------------------------

function handle(event) {
  recordEvent("in", event);

  switch (event.type) {
    case "session.created":
      setStat("session", event.session?.id ?? "");
      break;

    case "session.updated": {
      // A session that took the floor is one the server will not endpoint for.
      // It is read from what the server reports rather than from what was
      // asked, because the two differ on a binding that refuses to hand the
      // floor over.
      const detection = event.session?.audio?.input?.turn_detection;
      manualTurns = detection === null;
      elements.endturn.disabled = !manualTurns;
      elements.mediaState.textContent = manualTurns
        ? "this session declares its own turns — press End turn when you have finished speaking"
        : elements.mediaState.textContent;
      // Whether the prompt on screen is the prompt the agent has. A surface
      // that only said "sent" would leave the most useful question about a
      // mid-session edit unanswered: a server that ignored it and a server
      // that applied it look identical from the client's side of the send.
      if (typeof event.session?.instructions === "string") {
        elements.instructionsState.textContent =
          event.session.instructions.trim() === elements.instructions.value.trim()
            ? "in force"
            : "the server is running different instructions from the ones shown";
      }
      negotiated = event.session?.openrealtime ?? null;
      describeNegotiation(negotiated);
      for (const source of sources.values()) source.applyLimits(negotiated?.video);
      break;
    }

    case "input_audio_buffer.speech_started":
      setState("listening", "live");
      channels.pulse("obs.audio");
      // The person started talking over the agent. What was generated past
      // this moment was never heard, and saying so is the client's job: the
      // server knows what it sent, only this page knows what came out of the
      // speakers.
      interrupt();
      break;

    case "input_audio_buffer.speech_stopped":
      endpointAt = performance.now();
      firstAudioAt = null;
      setState("thinking", "working");
      break;

    case "conversation.item.input_audio_transcription.completed":
      channels.record("obs.audio", { title: "you · speech", body: event.transcript,
                                     key: `heard-${event.item_id}` });
      break;

    case "response.created":
      responseOpen = true;
      setState("responding", "working");
      break;

    case "response.output_audio_transcript.delta":
      channels.setState("act.speech", "live");
      channels.append("act.speech", `said-${event.item_id}`, event.delta, "spoken");
      break;

    // Text is not the transcript of speech, and a session that asked for text
    // gets only this. There was nothing to notice while every session spoke;
    // there is now, and rendering them on one channel would hide which of the
    // two a binding actually produced.
    case "response.output_text.delta":
      channels.setState("act.text", "live");
      channels.append("act.text", `wrote-${event.item_id}`, event.delta, "written");
      break;

    case "response.output_audio.delta":
      player?.enqueue(event.item_id, event.delta);
      channels.pulse("act.speech");
      break;

    case "response.output_audio.done":
      player?.finish(event.item_id);
      break;

    case "openrealtime.observation.added":
      observed(event);
      break;

    case "openrealtime.debug.event":
      timeline.add(event);
      setStat("last debug event", `${event.category} · ${event.name}`);
      break;

    case "response.function_call_arguments.done":
      runTool(event);
      break;

    case "response.done":
      responseOpen = false;
      channels.setState("act.speech", "");
      channels.setState("act.text", "");
      if (event.response?.status) {
        const status = event.response.status;
        const reason = event.response.status_details?.reason;
        setStat("last turn", reason ? `${status} · ${reason}` : status);
        // A cancelled turn is barge-in — the most ordinary thing that happens
        // in a voice session — so it is not reported as a problem. The other
        // two are the answer to "why did nothing arrive", and this is where a
        // person is looking for it.
        if (status === "incomplete" || status === "failed") {
          notice(`the turn produced no output — ${status}: ${reason ?? "no reason given"}`);
        }
      }
      setState("connected", "live");
      break;

    case "conversation.item.truncated":
      setStat("last truncation", `${event.audio_end_ms} ms of ${event.item_id}`);
      break;

    case "error":
      notice(event.error?.message ?? "the server reported an error");
      setState("error", "failed");
      break;
  }
}

// observed renders what the agent perceived, on the channel it perceived it
// through.
//
// Rendered unlike speech on purpose, and the protocol is explicit about why:
// narrated screen text is something the agent read, not something anyone said,
// and a page that displayed the two the same way would be teaching the person
// the confusion the authority rules exist to prevent.
function observed(event) {
  const channel = SOURCE_CHANNEL[event.source]
    ?? (event.observer === "audio" ? "obs.audio" : null);
  const title = `observed · ${event.observer}${event.source ? ` · ${event.source}` : ""}`;
  if (!channel) {
    // An observer this page did not declare a card for still happened, and
    // silently dropping it would make the surface lie about what the agent
    // perceived.
    channels.record("obs.tools", { title, body: event.text, kind: "observed" });
    return;
  }
  channels.record(channel, {
    title, body: event.text, kind: "observed", key: `obs-${event.observation_id}`,
  });
  if (event.authority && event.authority !== "observer") {
    setStat("observation authority", event.authority);
  }
}

function describeNegotiation(extension) {
  if (!extension) {
    elements.negotiated.textContent =
      "No extension in the reply. This is an ordinary Realtime session: voice and text only, " +
      "no video, no observations — the three video channels stay dark.";
    for (const control of [elements.screen, elements.camera, elements.browser]) control.disabled = true;
    return;
  }
  const enabled = extension.enabled ?? [];
  const video = extension.video;
  elements.negotiated.textContent =
    `Enabled: ${enabled.join(", ") || "nothing"}. ` +
    `Observers: ${(extension.observers ?? []).join(", ") || "none"}` +
    (extension.available_observers ? ` of ${extension.available_observers.join(", ")}` : "") + "." +
    (video ? ` Video: ${video.format}, up to ${video.fps_cap}/s, ${video.max_dimension}px long edge,` +
             ` ${Math.round((video.max_frame_bytes ?? 0) / 1024)} KiB a frame.` : "");
  const canSendVideo = enabled.includes("video.input");
  elements.screen.disabled = !canSendVideo;
  elements.camera.disabled = !canSendVideo;
  elements.browser.disabled = !canSendVideo || !config.browser.available;
  setStat("computer use", enabled.includes("computer_use") ? "negotiated" : "not enabled");
  if (!canSendVideo) setStat("video", "not enabled for this session");
}

// interrupt tells the server where playback actually stopped.
//
// Both halves matter. output_audio_buffer.clear stops what is queued on the
// server, and conversation.item.truncate says how much of the utterance was
// heard — without the second, the trajectory records speech nobody received as
// something the agent said, and every later turn reasons from a conversation
// that did not happen.
function interrupt() {
  if (!player?.speaking()) return;
  const itemId = player.playingItem();
  const heard = player.playedMs();
  player.stop();
  if (!itemId) return;
  if (responseOpen) send({ type: "output_audio_buffer.clear" });
  send({ type: "conversation.item.truncate", item_id: itemId, content_index: 0, audio_end_ms: heard });
}

// --- the action side --------------------------------------------------------

async function runTool(event) {
  const channel = channelOf(config.tools, event.name);
  const key = `call-${event.call_id}`;
  const card = {
    computer: "act.computer", artifact: "act.artifact", download: "act.download",
  }[channel] ?? "act.tools";
  channels.setState(card, "live");
  channels.record(card, {
    title: event.name, body: summarise(channel, event.arguments), kind: "mono", key,
  });
  if (channel === "computer") markAction(event.name, event.arguments);

  if (!tools?.connected) {
    channels.outcome(card, key, "failed");
    answer(event.call_id, JSON.stringify({ error: "this surface has no tool host attached" }));
    return;
  }

  const started = performance.now();
  const result = await tools.call(event.call_id, event.name, event.arguments);
  setStat("last tool round trip", `${Math.round(performance.now() - started)} ms`);

  if (result.error) {
    channels.outcome(card, key, result.error.includes("declined") ? "declined" : "failed");
    channels.record("obs.tools", { title: `${event.name} · refused`, body: result.error, kind: "mono" });
    channels.setState(card, "failed");
  } else {
    channels.outcome(card, key, "done");
    if (result.artifact) {
      artifacts.show(result.artifact);
      elements.artifactHost.querySelector(".placeholder")?.setAttribute("hidden", "");
      channels.record("obs.tools", {
        title: `${event.name} · displayed`,
        body: `${result.artifact.title} · version ${result.artifact.version}`,
      });
    } else if (result.download) {
      downloads.show(result.download);
      channels.record("obs.tools", {
        title: `${event.name} · available`,
        body: `${result.download.filename} · ${result.download.bytes} bytes`,
      });
    } else {
      const output = result.output ?? "";
      channels.record("obs.tools", {
        title: `${event.name} · returned`, kind: "mono",
        body: output.length > 400 ? `${output.slice(0, 400)}…` : output,
      });
    }
    channels.setState(card, "");
  }

  // Whatever happened, the session hears about it. A call left unanswered is
  // the one outcome that leaves the model waiting on something that is never
  // coming — and the server fails it on a deadline for exactly that reason, so
  // silence here would only turn a fast error into a slow one.
  answer(event.call_id, result.error ? JSON.stringify({ error: result.error }) : result.output);
}

function answer(callId, output) {
  send({
    type: "conversation.item.create",
    item: { type: "function_call_output", call_id: callId, output: output ?? "" },
  });
}

// summarise turns a call's arguments into the one line worth reading on a
// channel card. The whole thing is in the event log; what belongs here is what
// tells you at a glance whether the agent did the right thing.
function summarise(channel, args) {
  let parsed;
  try {
    parsed = typeof args === "string" ? JSON.parse(args) : args ?? {};
  } catch {
    return String(args);
  }
  if (channel === "artifact") {
    return `${parsed.title ?? parsed.artifact_id ?? "artifact"} · ${(parsed.html ?? "").length} bytes`;
  }
  if (channel === "download") {
    return `${parsed.filename ?? parsed.artifact_id ?? "download"} · ` +
      `${(parsed.text ?? parsed.base64 ?? "").length} encoded chars`;
  }
  if (channel === "computer") {
    const where = parsed.source ? `${parsed.source} ` : "";
    if (parsed.text !== undefined) return `${where}type ${JSON.stringify(parsed.text)}`;
    if (parsed.keys !== undefined) return `${where}keys ${parsed.keys.join("+")}`;
    if (parsed.from_x !== undefined) {
      return `${where}${parsed.from_x},${parsed.from_y} → ${parsed.to_x},${parsed.to_y}`;
    }
    if (parsed.delta_y !== undefined || parsed.delta_x !== undefined) {
      return `${where}scroll ${parsed.delta_x ?? 0},${parsed.delta_y ?? 0} at ${parsed.x},${parsed.y}`;
    }
    if (parsed.x !== undefined) return `${where}${parsed.x},${parsed.y}`;
    if (parsed.duration_ms !== undefined) return `wait ${parsed.duration_ms} ms`;
    return where.trim() || "—";
  }
  const shown = JSON.stringify(parsed);
  return shown.length > 200 ? `${shown.slice(0, 200)}…` : shown;
}

// markAction draws where the action landed, on the source it named.
function markAction(name, args) {
  let parsed;
  try {
    parsed = typeof args === "string" ? JSON.parse(args) : args ?? {};
  } catch {
    return;
  }
  if (parsed.x === undefined || !parsed.source) return;
  const label = name.replace("computer.", "");
  if (!channels.mark(parsed.source, parsed.x, parsed.y, label)) {
    // An action on a source that is not on screen is worth saying out loud.
    // It is the shape of the most confusing computer-use failure there is: the
    // model acting confidently in a coordinate space nobody is sending.
    notice(`${name} named source "${parsed.source}", which is not currently being sent`);
  }
}

async function askConfirmation(request) {
  elements.confirmName.textContent = request.name;
  elements.confirmRequirement.textContent = request.confirm;
  elements.confirmArguments.textContent = JSON.stringify(request.arguments, null, 2);
  elements.confirm.showModal();
  return await new Promise((resolve) => {
    elements.confirm.addEventListener("close", () => {
      resolve(elements.confirm.returnValue === "allow");
    }, { once: true });
  });
}

// --- video ------------------------------------------------------------------

async function toggleSource(name, button) {
  const existing = sources.get(name);
  if (existing) {
    existing.stop();
    return;
  }
  const channel = SOURCE_CHANNEL[name];
  const source = name === "browser" ? new RemoteVideoSource(name) : new VideoSource(name);

  source.addEventListener("source", (message) => send(message.detail));
  source.addEventListener("frame", (message) => {
    if (!send(message.detail)) return;
    channels.pulse(channel);
    if (transport?.bufferedBytes) {
      const buffered = transport.bufferedBytes();
      setStat("data channel queue", `${Math.round(buffered / 1024)} KiB`);
      // Whether a frame crossed whole or in pieces is the difference between
      // video working and video silently not working on a peer with a small
      // limit, so the surface says which happened rather than leaving it to be
      // inferred from a capture.
      const size = message.detail.frame.length;
      setStat("last frame",
        `${Math.round(size / 1024)} KiB, sent ${size <= transport.maxMessageBytes() ? "whole" : "in chunks"}`);
    }
  });
  source.addEventListener("throttled", (message) => {
    const { size, limit, quality } = message.detail;
    setStat(`${name} encoding`,
      `${Math.round(size / 1024)} KiB exceeded ${Math.round(limit / 1024)} KiB, retrying at q${quality}`);
  });
  source.addEventListener("located", (message) => {
    if (message.detail) channels.caption(channel, message.detail);
  });
  source.addEventListener("failed", (message) => {
    channels.setState(channel, "failed");
    notice(`${name}: ${message.detail}`);
  });
  source.addEventListener("ended", () => {
    sources.delete(name);
    setPressed(button, false);
    channels.detach(channel);
  });

  try {
    await source.start(negotiated?.video);
    sources.set(name, source);
    setPressed(button, true);
    channels.attach(channel, source.thumbnail);
    if (name === "browser" && config.browser.width) {
      channels.caption(channel, `${config.browser.width}×${config.browser.height} · ${config.browser.target}`);
    }
  } catch (failure) {
    notice(`could not start ${name}: ${failure.message}`);
    channels.setState(channel, "failed");
  }
}

// --- wiring -----------------------------------------------------------------

function renderTools(tools) {
  elements.toolsRoot.textContent = config.root
    ? `Filesystem tools resolve inside ${config.root}, after symlinks.`
    : "This surface declares no filesystem tools.";
  elements.toolList.innerHTML = "";
  for (const tool of tools) {
    const item = document.createElement("li");
    const name = document.createElement("span");
    name.className = "name";
    name.textContent = tool.name;
    const channel = document.createElement("span");
    channel.className = "badge";
    channel.textContent = tool.channel;
    const confirm = document.createElement("span");
    confirm.className = "badge";
    confirm.textContent = `confirm: ${tool.confirm}`;
    item.append(name, channel, confirm);
    elements.toolList.append(item);
  }
}

function submitText(text, origin) {
  if (!text || !transport) return;
  // A client that types is the same participant as a client that speaks; the
  // difference is the transport rather than the provenance. An artifact the
  // person clicked reaches the session the same way, and that is the whole
  // reason generative UI needs no channel of its own coming back.
  send({
    type: "conversation.item.create",
    item: { type: "message", role: "user", content: [{ type: "input_text", text }] },
  });
  send({ type: "response.create" });
  channels.record("obs.text", { title: origin, body: text, key: `typed-${Date.now()}` });
  channels.setState("obs.text", "live");
}

async function load() {
  const response = await fetch("/api/config");
  config = await response.json();

  channels = new Channels(elements.observationCards, elements.actionCards, elements.meters);
  timeline = new DebugTimeline(elements.timeline, elements.timelineSummary, elements.timelineFilter);
  artifacts = new ArtifactPanel(elements.artifactTabs, elements.artifactHost);
  downloads = new DownloadPanel(elements.downloads);
  artifacts.addEventListener("interaction", (message) => {
    submitText(message.detail.text, `artifact · ${message.detail.artifactId}`);
  });

  elements.instructions.value = DEFAULT_INSTRUCTIONS;
  elements.sessionJson.value = JSON.stringify(defaultSession(), null, 2);
  renderTools(config.tools);

  if (!config.webrtc) {
    const option = elements.transport.querySelector('option[value="webrtc"]');
    option.disabled = true;
    option.textContent = "WebRTC (no adapter configured)";
  }
  if (!config.browser.available) {
    elements.browser.textContent = "Browser (none attached)";
    channels.caption("obs.browser", "no browser context — start the surface with -browser-devtools-url");
  }
  setStat("endpoint", config.endpoint);
  setStat("credential", config.authenticated ? "held by the surface" : "none");
  setStat("channels", `${CHANNELS.length} declared`);
}

elements.connect.addEventListener("click", connect);
elements.disconnect.addEventListener("click", () => disconnect());
elements.screen.addEventListener("click", () => toggleSource("screen", elements.screen));
elements.camera.addEventListener("click", () => toggleSource("camera", elements.camera));
elements.browser.addEventListener("click", () => toggleSource("browser", elements.browser));
elements.clearEvents.addEventListener("click", () => { elements.events.innerHTML = ""; });
elements.clearTimeline.addEventListener("click", () => timeline.clear());
elements.saveEvents.addEventListener("click", () => {
  const blob = new Blob([JSON.stringify({ transcript, coverage: channels.summary() }, null, 2)],
                        { type: "application/json" });
  const link = document.createElement("a");
  link.href = URL.createObjectURL(blob);
  link.download = `openrealtime-surface-${Date.now()}.json`;
  link.click();
  URL.revokeObjectURL(link.href);
});

elements.mic.addEventListener("click", () => {
  // Whichever side owns capture owns muting: the recorder on the WebSocket
  // path, the peer connection's own track on the WebRTC one.
  const microphone = recorder ?? transport;
  if (!microphone?.setMuted) return;
  const muted = !microphone.muted;
  microphone.setMuted(muted);
  setPressed(elements.mic, !muted);
  elements.mic.textContent = muted ? "Microphone (muted)" : "Microphone";
  channels.setState("obs.audio", muted ? "" : "live");
  if (muted) elements.level.style.width = "0";
});

elements.endturn.addEventListener("click", () => {
  if (!manualTurns) return;
  // Both halves. The commit says where the turn ended; without the request the
  // server has been told a turn finished and not that anything should come of
  // it, which is the whole point of taking the floor.
  send({ type: "input_audio_buffer.commit" });
  send({ type: "response.create" });
});

elements.compose.addEventListener("submit", (submission) => {
  submission.preventDefault();
  submitText(elements.typed.value.trim(), "you · typed");
  elements.typed.value = "";
});

elements.applySession.addEventListener("click", () => {
  let session;
  try {
    session = JSON.parse(elements.sessionJson.value);
  } catch (failure) {
    notice(`the session JSON did not parse: ${failure.message}`);
    return;
  }
  if (!send({ type: "session.update", session: sessionForTransport(session, transport?.name) })) {
    notice("not connected, so nothing was sent");
  }
});

elements.resetSession.addEventListener("click", () => {
  elements.sessionJson.value = JSON.stringify(defaultSession(), null, 2);
});

// The prompt, changed mid-session. It is a session.update carrying only
// instructions, which leaves everything else negotiated exactly as it was —
// re-sending the whole session to change one field would re-negotiate the
// extension as a side effect of editing a sentence.
elements.applyInstructions.addEventListener("click", () => {
  const instructions = elements.instructions.value.trim();
  if (!instructions) {
    notice("an empty prompt was not sent");
    return;
  }
  if (send({ type: "session.update", session: { type: "realtime", instructions } })) {
    elements.instructionsState.textContent = "sent — it applies from the next turn";
  } else {
    notice("not connected, so the prompt was not sent");
  }
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

load().catch((failure) => {
  setState(`could not load the surface configuration: ${failure.message}`, "failed");
});
