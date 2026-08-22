// Drive the console in a real browser and report what happened.
//
//   node browser.mjs <console url> <websocket|webrtc>
//
// This exists because the console is the only client that exercises the whole
// system at once - both transports, the extension negotiation, video, and
// client-executed tools - and none of that can be verified from Go. The Go
// suites prove the adapter forwards events and the gateway carries frames;
// only a browser proves they compose, and only a browser runs the client half
// of the chunk framing, the playout scheduling, and the capture path.
//
// Chromium is launched with fake devices, so getUserMedia yields a synthetic
// tone and getUserMedia for video a synthetic pattern. Everything else is real.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const CONSOLE_URL = process.argv[2];
const MODE = process.argv[3] ?? "websocket";
const PORT = Number(process.env.CDP_PORT ?? 19222);

if (!CONSOLE_URL) {
  console.error("usage: node browser.mjs <console url> <websocket|webrtc>");
  process.exit(2);
}

const profile = mkdtempSync(join(tmpdir(), "openrealtime-console-"));
// CONSOLE_FAKE_AUDIO plays a WAV file into the fake microphone instead of the
// synthetic tone. The tone is enough to prove audio reaches the protocol, and
// it is all the committed test needs; real speech is what a run against a real
// recogniser needs, because a tone transcribes to nothing.
const fakeAudio = process.env.CONSOLE_FAKE_AUDIO;
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new",
  `--remote-debugging-port=${PORT}`,
  "--no-sandbox",
  "--disable-gpu",
  "--use-fake-device-for-media-stream",
  "--use-fake-ui-for-media-stream",
  ...(fakeAudio ? [`--use-file-for-fake-audio-capture=${fakeAudio}%noloop`] : []),
  "--autoplay-policy=no-user-gesture-required",
  `--user-data-dir=${profile}`,
  "about:blank",
], { stdio: ["ignore", "pipe", "pipe"] });

let chromiumErrors = "";
chromium.stderr.on("data", (chunk) => { chromiumErrors += chunk.toString(); });

async function debuggerEndpoint() {
  for (let attempt = 0; attempt < 80; attempt++) {
    try {
      const response = await fetch(`http://127.0.0.1:${PORT}/json/version`);
      return (await response.json()).webSocketDebuggerUrl;
    } catch {
      await sleep(250);
    }
  }
  throw new Error(`chromium never opened a debugging port:\n${chromiumErrors}`);
}

class CDP {
  #socket; #next = 1; #pending = new Map(); #handlers = new Map();

  static async connect(url) {
    const client = new CDP();
    client.#socket = new WebSocket(url);
    await new Promise((resolve, reject) => {
      client.#socket.addEventListener("open", resolve, { once: true });
      client.#socket.addEventListener("error", reject, { once: true });
    });
    client.#socket.addEventListener("message", (message) => {
      const frame = JSON.parse(message.data);
      if (frame.id && client.#pending.has(frame.id)) {
        const { resolve, reject } = client.#pending.get(frame.id);
        client.#pending.delete(frame.id);
        frame.error ? reject(new Error(JSON.stringify(frame.error))) : resolve(frame.result);
      } else if (frame.method) {
        (client.#handlers.get(frame.method) ?? []).forEach((handler) => handler(frame.params));
      }
    });
    return client;
  }

  on(method, handler) {
    if (!this.#handlers.has(method)) this.#handlers.set(method, []);
    this.#handlers.get(method).push(handler);
  }

  send(method, params = {}, sessionId) {
    const id = this.#next++;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject });
      this.#socket.send(JSON.stringify({ id, method, params, sessionId }));
    });
  }
}

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok, detail });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? `  — ${detail}` : ""}`);
}

try {
  const browser = await CDP.connect(await debuggerEndpoint());
  const { targetId } = await browser.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await browser.send("Target.attachToTarget", { targetId, flatten: true });

  const pageErrors = [];
  const requestFailures = [];
  browser.on("Runtime.exceptionThrown", (params) =>
    pageErrors.push(params.exceptionDetails.exception?.description ?? params.exceptionDetails.text));
  browser.on("Network.loadingFailed", (params) => requestFailures.push(params.errorText));

  const call = (method, params) => browser.send(method, params, sessionId);
  await call("Runtime.enable");
  await call("Page.enable");
  await call("Network.enable");
  await call("Page.navigate", { url: CONSOLE_URL });
  await sleep(2000);

  const evaluate = async (expression) => {
    const { result, exceptionDetails } = await call("Runtime.evaluate", {
      expression, awaitPromise: true, returnByValue: true,
    });
    if (exceptionDetails) {
      throw new Error(exceptionDetails.exception?.description ?? exceptionDetails.text);
    }
    return result.value;
  };

  const stats = () => evaluate(`
    Object.fromEntries([...document.querySelectorAll('#stats dt')].map(
      (dt, i) => [dt.textContent, document.querySelectorAll('#stats dd')[i].textContent]))`);
  const turns = () => evaluate(`
    [...document.querySelectorAll('.turn')].map(t => ({
      kind: t.className, who: t.querySelector('.who').textContent,
      text: t.querySelector('.text').textContent.slice(0, 200),
      outcome: t.dataset.outcome ?? null,
    }))`);
  const eventNames = (direction) => evaluate(`
    [...document.querySelectorAll('.event')]
      ${direction ? `.filter(e => e.dataset.direction === '${direction}')` : ""}
      .map(e => e.querySelector('.name').textContent)`);
  const notices = () => evaluate(`
    [...document.querySelectorAll('.turn.notice')].map(t => t.querySelector('.text').textContent)`);

  // Waiting on a condition rather than on a duration. Fixed sleeps are the
  // reason browser tests are flaky: they encode how fast the machine was on
  // the day they were written, and this one shares a machine with everything
  // else the gate runs.
  const waitFor = async (what, predicate, timeoutMs = 30000) => {
    const deadline = Date.now() + timeoutMs;
    let last;
    while (Date.now() < deadline) {
      last = await predicate();
      if (last) return last;
      await sleep(250);
    }
    console.log(`  (timed out waiting for ${what})`);
    return last;
  };

  // --- the page ------------------------------------------------------------
  check("the page loads", await evaluate("document.title") === "OpenRealtime console");
  check("every module loaded", requestFailures.length === 0, requestFailures.join("; "));
  check("no uncaught exception on load", pageErrors.length === 0, pageErrors.join("; "));

  const declared = JSON.parse(await evaluate("document.getElementById('session-json').value"));
  check("the session declares the extension",
    declared.openrealtime?.supports?.includes("video.input"));
  check("tool declarations carry their confirmation requirement",
    declared.tools?.find((tool) => tool.name === "write_file")?.openrealtime?.confirm === "always",
    JSON.stringify(declared.tools?.map((t) => [t.name, t.openrealtime?.confirm])));

  // --- connect -------------------------------------------------------------
  // The event log hides frames and audio deltas by default, so it has to be
  // turned on before it can be used as evidence.
  await evaluate("document.getElementById('filter-audio').checked = false");
  await evaluate(`
    document.getElementById('transport').value = ${JSON.stringify(MODE)};
    document.getElementById('connect').click();`);

  const live = ["connected", "listening", "thinking", "responding"];
  await waitFor("the session to open", async () =>
    live.includes(await evaluate("document.getElementById('state').textContent")));

  const state = await evaluate("document.getElementById('state').textContent");
  check(`the session connects over ${MODE}`,
    ["connected", "listening", "thinking", "responding"].includes(state), `state=${state}`);

  await waitFor("session.updated to arrive", async () =>
    (await evaluate("document.getElementById('negotiated').textContent")).length > 0);
  const negotiated = await evaluate("document.getElementById('negotiated').textContent");
  check("the extension negotiated", negotiated.includes("video.input"), negotiated);

  const sent = await eventNames("out");
  check("the page sent session.update first", sent[0]?.includes("session.update"),
    JSON.stringify(sent.slice(0, 3)));

  if (MODE === "websocket") {
    await waitFor("captured audio to reach the protocol", async () =>
      (await eventNames("out")).some((name) => name.includes("input_audio_buffer.append")));
    check("microphone audio reaches the protocol",
      (await eventNames("out")).some((name) => name.includes("input_audio_buffer.append")));
  } else {
    // On this transport the adapter owns the media and the audio format, so a
    // client that also sent audio events would be describing a connection it
    // does not have.
    check("the page sends no audio events when the adapter owns media",
      !(await eventNames("out")).some((name) => name.includes("input_audio_buffer.append")));
    check("the peer connection reports connected",
      (await stats())["peer connection"] === "connected");
  }

  // --- a turn --------------------------------------------------------------
  //
  // The fake microphone emits a continuous tone, which the server hears as a
  // person talking. That is the right behaviour on both sides and it means the
  // agent will not speak: it is not going to talk over someone. So the turn
  // here is: let the gate hear speech, mute to give it the silence that ends a
  // turn, then type. A client that types is the same participant as one that
  // speaks; only the transport differs.
  await waitFor("the gate to hear the fake microphone", async () =>
    (await eventNames("in")).some((name) => name.includes("speech_started")));
  check("the server's voice activity gate hears the microphone",
    (await eventNames("in")).some((name) => name.includes("speech_started")));

  await evaluate("document.getElementById('mic').click()");
  check("the microphone can be muted on this transport",
    (await evaluate("document.getElementById('mic').textContent")).includes("muted"));

  // Silence, not absence: muting keeps the track flowing, which is what lets
  // the gate decide the turn ended. A client that simply stopped sending would
  // leave the floor held.
  await waitFor("the turn to end after muting", async () =>
    (await eventNames("in")).some((name) => name.includes("speech_stopped")));
  check("muting ends the turn rather than leaving the floor held",
    (await eventNames("in")).some((name) => name.includes("speech_stopped")));

  await evaluate(`
    document.getElementById('typed').value = 'please read the notes file';
    document.getElementById('compose').dispatchEvent(new Event('submit', {cancelable: true}));`);

  await waitFor("the turn to complete", async () => {
    const so_far = await turns();
    return so_far.some((turn) => turn.kind.includes("tool") && turn.outcome !== null)
      && so_far.some((turn) => turn.who === "agent");
  });

  const conversation = await turns();
  check("what the user said is rendered",
    conversation.some((turn) => turn.who === "you" && turn.text.includes("notes file")),
    JSON.stringify(conversation.map((turn) => turn.who)));
  check("the agent's speech is rendered",
    conversation.some((turn) => turn.who === "agent" && turn.text.length > 0));
  check("a tool call ran on this machine",
    conversation.some((turn) => turn.kind.includes("tool") && turn.outcome === "done"),
    JSON.stringify(conversation.filter((t) => t.kind.includes("tool")).map((t) => t.outcome)));
  check("the tool returned the file's real contents",
    conversation.some((turn) => turn.text.includes("deadline moved to Friday")));

  const afterTurn = await eventNames("out");
  check("the tool result went back as a conversation item",
    afterTurn.filter((name) => name.includes("conversation.item.create")).length >= 2);

  if (MODE === "websocket") {
    check("synthesised audio arrived and was scheduled",
      (await eventNames("in")).filter((n) => n.includes("response.output_audio.delta")).length > 0);
    check("first-audio latency was measured", "first audio after endpoint" in await stats());
  }
  check("the tool round trip was measured", "last tool round trip" in await stats());

  // --- video ---------------------------------------------------------------
  await evaluate("document.getElementById('camera').click()");
  await waitFor("an observation of the camera", async () =>
    (await evaluate(`
      [...document.querySelectorAll('.turn.observed')].map(t => t.querySelector('.text').textContent)`))
      .some((text) => text.includes("editor")));

  const video = (await eventNames("out")).filter((name) => name.includes("input_video"));
  check("a source was declared before any frame",
    video[0]?.includes("input_video_source.update"), JSON.stringify(video.slice(0, 2)));
  check("frames reach the protocol",
    video.filter((name) => name.includes("frame.append")).length > 0,
    `${video.filter((name) => name.includes("frame.append")).length} frames`);
  check("an observation came back and renders as observed content",
    (await evaluate(`
      [...document.querySelectorAll('.turn.observed')].map(t => t.querySelector('.text').textContent)`))
      .some((text) => text.includes("editor")));

  if (MODE === "webrtc") {
    const measured = await stats();
    // Whether a frame crosses whole or in chunks is decided by what the two
    // peers negotiated, and it differs by browser: Chromium reports 256 KiB
    // here even though pion advertises far more. A real screen frame is often
    // larger than that once base64 encoded, which is the case chunking exists
    // for.
    check("the console reports the negotiated message limit",
      "data channel max message" in measured, JSON.stringify(measured["data channel max message"]));
    check("the console reports how each frame crossed",
      /whole|in chunks/.test(measured["last frame"] ?? ""), measured["last frame"]);
  }

  check("no errors were surfaced to the user", (await notices()).length === 0,
    JSON.stringify(await notices()));
  check("no uncaught exception during the session", pageErrors.length === 0, pageErrors.join("; "));
} catch (failure) {
  check("the run completed", false, failure.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (${MODE})`);
process.exit(failed.length === 0 ? 0 : 1);
