// Drive the test surface in a real browser and report what every channel did.
//
//   node surface.mjs <surface url> <websocket|webrtc>
//
// The surface exists because twelve channels running at once fail in ways that
// are invisible one at a time, and this file is the same argument applied to
// testing it: what it checks is not that each piece works but that they all
// carry something in one session, against one server, over one transport.
//
// Three of the checks could not be made any other way. An artifact is a
// sandboxed cross-origin frame, so proving it rendered means clicking a button
// inside it with a real input event and watching what comes back out. A
// computer-use action is only real if the page it named actually changed, so
// the target browser's own location is what gets asserted rather than the fact
// that a command was sent. And a coordinate mark is drawn from the geometry
// the model was shown, which exists only in a browser that laid the page out.
//
// Chromium is launched with fake devices, so the camera is a synthetic pattern
// and the microphone a tone this file replaces with something that sustains.
// Everything else is real.

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const SURFACE_URL = process.argv[2];
const MODE = process.argv[3] ?? "websocket";
const PORT = Number(process.env.CDP_PORT ?? 19223);

if (!SURFACE_URL) {
  console.error("usage: node surface.mjs <surface url> <websocket|webrtc>");
  process.exit(2);
}

const profile = mkdtempSync(join(tmpdir(), "openrealtime-surface-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new",
  `--remote-debugging-port=${PORT}`,
  "--no-sandbox",
  "--disable-gpu",
  "--use-fake-device-for-media-stream",
  "--use-fake-ui-for-media-stream",
  "--autoplay-policy=no-user-gesture-required",
  "--window-size=1600,1000",
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
  await call("Page.navigate", { url: SURFACE_URL });
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

  // --- reading the page ----------------------------------------------------

  const channel = (id) => evaluate(`(() => {
    // .channel scopes this to the card: the meter strip carries the same
    // data-channel attribute and comes first in the document.
    const card = document.querySelector('.channel[data-channel="${id}"]');
    if (!card) return null;
    return {
      count: Number(card.querySelector('.count').textContent),
      state: card.dataset.state ?? "",
      caption: card.querySelector('.stage-caption')?.textContent ?? "",
      marks: card.querySelectorAll('.mark').length,
      entries: [...card.querySelectorAll('.entries li')].map(entry => ({
        title: entry.querySelector('.entry-title').textContent,
        body: entry.querySelector('.entry-body').textContent.slice(0, 240),
        outcome: entry.dataset.outcome ?? null,
      })),
    };
  })()`);

  const events = (direction) => evaluate(`
    [...document.querySelectorAll('#events div')]
      ${direction ? `.filter(line => line.className === '${direction}')` : ""}
      .map(line => line.textContent)`);

  const sawEvent = async (direction, type) =>
    (await events(direction)).some((line) => line.includes(`"type":"${type}"`));

  const stats = () => evaluate(`
    Object.fromEntries([...document.querySelectorAll('#stats dt')].map(
      (dt, i) => [dt.textContent, document.querySelectorAll('#stats dd')[i].textContent]))`);

  // Waiting on a condition rather than on a duration. Fixed sleeps encode how
  // fast the machine was on the day they were written, and this shares a
  // machine with everything else the gate runs.
  const waitFor = async (what, predicate, timeoutMs = 45000) => {
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

  // Chromium's fake microphone is a beep, not a voice: about forty
  // milliseconds at full amplitude and then a second of quiet. That is the
  // shape of a click, and a gate that admitted clicks would turn room tone
  // into turns nobody took. So it is replaced with something that sustains.
  await evaluate(`(() => {
    const context = new AudioContext();
    const oscillator = context.createOscillator();
    const gain = context.createGain();
    const destination = context.createMediaStreamDestination();
    oscillator.frequency.value = 220;
    gain.gain.value = 0.4;
    oscillator.connect(gain).connect(destination);
    oscillator.start();
    const real = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
    navigator.mediaDevices.getUserMedia = async (constraints) => {
      if (!constraints || !constraints.audio) return real(constraints);
      const stream = await real({ ...constraints, audio: false }).catch(() => new MediaStream());
      const voice = new MediaStream([destination.stream.getAudioTracks()[0]]);
      stream.getVideoTracks().forEach((track) => voice.addTrack(track));
      return voice;
    };

    // Headless Chromium has no operating-system display picker, but display
    // capture itself is still a browser media path. A changing canvas track
    // gives getDisplayMedia a real MediaStreamTrack, which exercises source
    // declaration, frame encoding, negotiated limits, transport, server
    // observation, thumbnail rendering, and source shutdown without granting
    // this test runner an ambient desktop.
    const display = document.createElement('canvas');
    display.width = 960;
    display.height = 540;
    const paint = display.getContext('2d');
    let frame = 0;
    const draw = () => {
      paint.fillStyle = frame++ % 2 ? '#17365d' : '#244f7d';
      paint.fillRect(0, 0, display.width, display.height);
      paint.fillStyle = 'white';
      paint.font = 'bold 48px system-ui';
      paint.fillText('Shared development screen', 90, 220);
      paint.font = '28px system-ui';
      paint.fillText('frame ' + frame, 90, 280);
    };
    draw();
    setInterval(draw, 250);
    const displayStream = display.captureStream(4);
    Object.defineProperty(navigator.mediaDevices, 'getDisplayMedia', {
      configurable: true,
      value: async () => new MediaStream(displayStream.getVideoTracks()),
    });
    return "installed";
  })()`);

  // --- the page ------------------------------------------------------------

  check("the page loads", await evaluate("document.title") === "OpenRealtime surface");
  check("every module loaded", requestFailures.length === 0, requestFailures.join("; "));
  check("no uncaught exception on load", pageErrors.length === 0, pageErrors.join("; "));

  const declaredChannels = await evaluate(
    "document.querySelectorAll('.channel').length");
  check("every channel has a card", declaredChannels === 12, `${declaredChannels} cards`);
  const meters = await evaluate("document.querySelectorAll('.meter').length");
  check("every channel has a meter", meters === 12, `${meters} meters`);

  const declared = JSON.parse(await evaluate("document.getElementById('session-json').value"));
  check("the session declares the extension",
    declared.openrealtime?.supports?.includes("video.input"));
  check("the session asks for both observers",
    JSON.stringify(declared.openrealtime?.observers) === '["audio","video"]',
    JSON.stringify(declared.openrealtime?.observers));
  check("generative UI is declared as an ordinary function tool",
    declared.tools?.some((tool) => tool.name === "display_artifact" && tool.type === "function"),
    JSON.stringify(declared.tools?.map((tool) => tool.name)));
  check("downloadable files are declared as an ordinary function tool",
    declared.tools?.some((tool) => tool.name === "publish_download" && tool.type === "function"));
  check("the developer debug stream is explicitly requested",
    declared.openrealtime?.debug?.enabled === true);
  check("the computer-use vocabulary is declared",
    declared.tools?.filter((tool) => tool.name.startsWith("computer.")).length === 10,
    `${declared.tools?.filter((tool) => tool.name.startsWith("computer.")).length} actions`);
  check("client-hosted tools delegate server confirmation explicitly",
    declared.tools?.find((tool) => tool.name === "write_file")?.openrealtime?.confirm === "never");
  check("computer-use declarations name the bounded client target",
    Boolean(declared.tools?.find((tool) => tool.name === "computer.click")?.openrealtime?.target));

  // --- connect -------------------------------------------------------------

  await evaluate("document.getElementById('filter-media').checked = false");
  await evaluate(`
    document.getElementById('transport').value = ${JSON.stringify(MODE)};
    document.getElementById('connect').click();`);

  const live = ["connected", "listening", "thinking", "responding"];
  await waitFor("the session to open", async () =>
    live.includes(await evaluate("document.getElementById('state').textContent")));
  const state = await evaluate("document.getElementById('state').textContent");
  check(`the session connects over ${MODE}`, live.includes(state), `state=${state}`);

  await waitFor("session.updated to arrive", async () =>
    (await evaluate("document.getElementById('negotiated').textContent")).length > 0);
  const negotiated = await evaluate("document.getElementById('negotiated').textContent");
  check("the extension negotiated", negotiated.includes("video.input"), negotiated);
  check("the observers the server will actually run are reported",
    /Observers: [a-z]/.test(negotiated), negotiated);
  await waitFor("a debug event to reach the timeline", async () =>
    Number(await evaluate("document.querySelectorAll('.timeline-row').length")) > 0);
  check("the debug timeline receives timestamped server evidence",
    Number(await evaluate("document.querySelectorAll('.timeline-row').length")) > 0);

  if (MODE === "websocket") {
    await waitFor("captured audio to reach the protocol",
      () => sawEvent("out", "input_audio_buffer.append"));
    check("microphone audio reaches the protocol",
      await sawEvent("out", "input_audio_buffer.append"));
  } else {
    check("the page sends no audio events when the adapter owns media",
      !(await sawEvent("out", "input_audio_buffer.append")));
    check("the peer connection reports connected",
      (await stats())["peer connection"] === "connected");
  }

  // The fake microphone is already live. Assert its onset before long-running
  // video coverage starts: the raw inspector is deliberately bounded, and a
  // debug-heavy run can otherwise evict this early event while three visual
  // sources are being narrated.
  await waitFor("the gate to hear the fake microphone", () =>
    sawEvent("in", "input_audio_buffer.speech_started"));
  check("the server's voice activity gate hears the microphone",
    await sawEvent("in", "input_audio_buffer.speech_started"));

  // --- the three video channels --------------------------------------------

  await evaluate("document.getElementById('browser').click()");
  await waitFor("browser frames to reach the protocol", async () =>
    (await events("out")).filter((line) =>
      line.includes("input_video_frame.append") && line.includes('"source":"browser"')).length > 0);
  const browserFrames = (await events("out")).filter((line) =>
    line.includes("input_video_frame.append") && line.includes('"source":"browser"')).length;
  check("the browser channel carries frames", browserFrames > 0, `${browserFrames} frames`);

  const outbound = await events("out");
  const firstBrowserEvent = outbound.find((line) => line.includes('"source":"browser"'));
  check("the browser source was declared before any of its frames",
    firstBrowserEvent?.includes("input_video_source.update"),
    firstBrowserEvent?.slice(0, 120));

  await evaluate("document.getElementById('screen').click()");
  await waitFor("screen-share frames to reach the protocol", async () =>
    (await events("out")).some((line) =>
      line.includes("input_video_frame.append") && line.includes('"source":"screen"')));
  check("the optional screen-share channel carries frames",
    (await events("out")).some((line) =>
      line.includes("input_video_frame.append") && line.includes('"source":"screen"')));
  const firstScreenEvent = (await events("out")).find((line) => line.includes('"source":"screen"'));
  check("the shared screen is declared before any of its frames",
    firstScreenEvent?.includes("input_video_source.update"), firstScreenEvent?.slice(0, 120));

  // The video observer intentionally samples at a much lower cadence than
  // capture. Wait for this source to be committed before adding another one:
  // seeing a screen frame leave the browser proves transport, while seeing a
  // source-labelled observation come back proves the complete perception path.
  const observedScreen = await waitFor("the shared screen to be narrated",
    async () => (await channel("obs.screen")).entries.length > 0, 30000);
  check("the agent receives observations from the shared screen",
    Boolean(observedScreen), JSON.stringify((await channel("obs.screen")).entries));

  await evaluate("document.getElementById('camera').click()");
  await waitFor("camera frames to reach the protocol", async () =>
    (await events("out")).some((line) =>
      line.includes("input_video_frame.append") && line.includes('"source":"camera"')));
  check("the camera channel carries frames",
    (await events("out")).some((line) =>
      line.includes("input_video_frame.append") && line.includes('"source":"camera"')));

  await waitFor("an observation to come back", () => sawEvent("in", "openrealtime.observation.added"));
  const observedCamera = await waitFor("the camera to be narrated",
    async () => (await channel("obs.camera")).entries.length > 0);
  check("what the agent perceived is rendered on the channel it came in on",
    Boolean(observedCamera), JSON.stringify((await channel("obs.camera")).entries[0]));
  check("observed content is rendered as observed rather than as speech",
    await evaluate(`document.querySelectorAll('.channel[data-channel="obs.camera"] .entries li.observed').length > 0`));

  // A source that stops must stop sending.
  //
  // Encoding a frame is asynchronous, so a capture in flight when the channel
  // is turned off would arrive after the source declared itself closed - and
  // the server refuses it, correctly, with an error about a source that "was
  // never declared". A live run turned the camera off to save context and
  // produced exactly that, which reads like a client that cannot count.
  await evaluate("document.getElementById('camera').click()");
  await waitFor("the camera to declare itself closed", async () =>
    (await events("out")).some((line) =>
      line.includes("input_video_source.update") && line.includes('"source":"camera"')
        && line.includes('"state":"closed"')));

  const outboundAfterClose = await events("out");
  const closedAt = outboundAfterClose.findLastIndex((line) =>
    line.includes("input_video_source.update") && line.includes('"source":"camera"')
      && line.includes('"state":"closed"'));
  const framesAfterClose = outboundAfterClose.slice(closedAt + 1).filter((line) =>
    line.includes("input_video_frame.append") && line.includes('"source":"camera"'));
  check("no frame follows the close of the source that sent it",
    framesAfterClose.length === 0, `${framesAfterClose.length} late frames`);
  check("no session error followed stopping a source",
    !(await events("in")).slice(-40).some((line) => line.includes("was never declared")));

  // --- a turn, and a tool that runs on this machine -------------------------

  await evaluate("document.getElementById('mic').click()");
  await waitFor("the turn to end after muting", () =>
    sawEvent("in", "input_audio_buffer.speech_stopped"));
  check("muting ends the turn rather than leaving the floor held",
    await sawEvent("in", "input_audio_buffer.speech_stopped"));

  await waitFor("the file to be read", async () =>
    (await channel("obs.tools")).entries.some((entry) => entry.body.includes("deadline moved to Friday")));
  const toolCalls = await channel("act.tools");
  const toolResults = await channel("obs.tools");
  check("a tool call is shown on the action side",
    toolCalls.entries.some((entry) => entry.title === "read_file"),
    JSON.stringify(toolCalls.entries.map((entry) => entry.title)));
  check("the call is marked done once it answered",
    toolCalls.entries.some((entry) => entry.title === "read_file" && entry.outcome === "done"));
  check("the tool's result comes back on the observation side",
    toolResults.entries.some((entry) => entry.body.includes("deadline moved to Friday")));
  check("what the agent heard is rendered on the audio channel",
    (await channel("obs.audio")).entries.some((entry) => entry.title.includes("speech")),
    JSON.stringify((await channel("obs.audio")).entries.map((entry) => entry.title)));
  check("the tool round trip was measured", "last tool round trip" in await stats());

  // --- generative UI --------------------------------------------------------

  await evaluate(`
    document.getElementById('typed').value = 'show me the deadline';
    document.getElementById('compose').dispatchEvent(new Event('submit', {cancelable: true}));`);
  check("typed text is rendered on the text channel",
    (await channel("obs.text")).entries.some((entry) => entry.body.includes("show me the deadline")));

  await waitFor("an artifact to be rendered", async () =>
    await evaluate("document.querySelectorAll('.artifact-frame').length > 0"));
  const artifactSource = await evaluate(
    "document.querySelector('.artifact-frame')?.getAttribute('src') ?? ''");
  check("the artifact is framed from its own route, not from srcdoc",
    artifactSource.startsWith("/artifacts/deadline?v="), artifactSource);
  check("the artifact frame withholds its origin",
    (await evaluate("document.querySelector('.artifact-frame').getAttribute('sandbox')"))
      .split(" ").every((token) => token !== "allow-same-origin"));
  check("the artifact is shown on the action side",
    (await channel("act.artifact")).entries.some((entry) => entry.title === "display_artifact"));

  // An artifact that loaded is one whose own script ran and whose button can
  // be pressed. It is a sandboxed cross-origin frame, so the only way to find
  // that out is a real input event at the coordinates it occupies.
  const frame = await evaluate(`(() => {
    const element = document.querySelector('.artifact-frame');
    const box = element.getBoundingClientRect();
    return { x: box.x, y: box.y, width: box.width, height: box.height };
  })()`);
  check("the artifact frame has been laid out", frame.width > 100 && frame.height > 100,
    JSON.stringify(frame));

  const buttonX = Math.round(frame.x + 20 + 110);
  const buttonY = Math.round(frame.y + 120 + 24);
  for (const type of ["mousePressed", "mouseReleased"]) {
    await call("Input.dispatchMouseEvent", {
      type, x: buttonX, y: buttonY, button: "left", clickCount: 1,
    });
  }

  const acknowledged = await waitFor("the artifact to speak for the person", async () =>
    (await channel("obs.text")).entries.some((entry) => entry.title.startsWith("artifact")));
  check("clicking inside an artifact reaches the session as the person speaking",
    Boolean(acknowledged),
    JSON.stringify((await channel("obs.text")).entries.map((entry) => entry.title)));
  check("what the artifact sent is what the person is recorded as saying",
    (await channel("obs.text")).entries.some((entry) =>
      entry.body.includes("I acknowledged the deadline")));

  // The artifact interaction opened the next scripted turn, which publishes
  // an actual file rather than placing bytes in the transcript.
  await waitFor("a generated file to be published", async () =>
    await evaluate("document.querySelectorAll('.download').length > 0"));
  const download = await evaluate(`(() => {
    const link = document.querySelector('.download');
    return { href: link?.getAttribute('href'), filename: link?.getAttribute('download') };
  })()`);
  check("the generated file is offered as a bounded same-origin download",
    download.href?.startsWith("/downloads/deadline-report?v=") && download.filename === "deadline.csv",
    JSON.stringify(download));
  const downloaded = await evaluate(
    "fetch(document.querySelector('.download').href).then((response) => response.text())");
  check("the download route serves the file the agent published",
    downloaded.includes("developer,Friday"), downloaded);
  check("the file is shown on the download action channel",
    (await channel("act.download")).entries.some((entry) => entry.title === "publish_download"));

  // --- computer use ---------------------------------------------------------

  await evaluate(`
    document.getElementById('typed').value = 'press the browser button';
    document.getElementById('compose').dispatchEvent(new Event('submit', {cancelable: true}));`);

  await waitFor("the agent to act on the browser", async () =>
    (await channel("act.computer")).entries.length > 0);
  const acted = await channel("act.computer");
  check("a computer-use action is shown on its own channel",
    acted.entries.some((entry) => entry.title === "computer.click"),
    JSON.stringify(acted.entries.map((entry) => entry.title)));
  check("the action was performed rather than refused",
    acted.entries.some((entry) => entry.title === "computer.click" && entry.outcome === "done"),
    JSON.stringify(acted.entries.map((entry) => entry.outcome)));
  check("the action names the source it acted on",
    acted.entries.some((entry) => entry.body.includes("browser")),
    JSON.stringify(acted.entries.map((entry) => entry.body)));

  // The mark is the one thing that could not be assembled from the event log
  // afterwards: a click at (200, 152) is a number, and a dot on the screenshot
  // is whether the agent hit the button.
  check("where the action landed is drawn on the source it named",
    (await channel("obs.browser")).marks > 0,
    `${(await channel("obs.browser")).marks} marks`);

  // And the page it named actually changed. Everything above would pass on a
  // surface that displayed a click it never performed.
  const pressed = await waitFor("the target page to react to the click", async () =>
    (await channel("obs.browser")).caption.includes("#pressed"));
  check("the browser the agent clicked really moved",
    Boolean(pressed), (await channel("obs.browser")).caption);

  // Written output is a distinct action channel, not the transcript of audio.
  // Reconfigure the same live session and require one real server turn to use
  // it so the end-to-end test cannot pass on a page that merely has a card for
  // text output.
  const beforeText = (await events("in")).filter((line) =>
    line.includes('"type":"session.updated"')).length;
  await evaluate(`(() => {
    const editor = document.getElementById('session-json');
    const session = JSON.parse(editor.value);
    session.output_modalities = ['text'];
    editor.value = JSON.stringify(session, null, 2);
    document.getElementById('apply-session').click();
  })()`);
  await waitFor("text output mode to be acknowledged", async () =>
    (await events("in")).filter((line) => line.includes('"type":"session.updated"')).length > beforeText);
  await evaluate(`
    document.getElementById('typed').value = 'write one short status line';
    document.getElementById('compose').dispatchEvent(new Event('submit', {cancelable: true}));`);
  await waitFor("written output to reach its action channel", async () =>
    (await channel("act.text")).entries.length > 0);
  check("written output is rendered separately from speech",
    (await channel("act.text")).entries.some((entry) => entry.body.length > 0),
    JSON.stringify((await channel("act.text")).entries));

  // --- the prompt, and coverage ---------------------------------------------

  await evaluate(`
    document.getElementById('instructions').value = 'Answer in one word.';
    document.getElementById('apply-instructions').click();`);
  // Wait for "in force", not the transient "sent": the former means the
  // server echoed the instructions it is actually running. Returning as soon
  // as the local send marker appears makes this assertion race its own
  // session.updated acknowledgement under the race detector.
  const promptState = await waitFor("the prompt to be confirmed", async () => {
    const shown = await evaluate("document.getElementById('instructions-state').textContent");
    return shown === "in force" ? shown : null;
  }, 15000);
  check("the session prompt can be changed mid-session",
    promptState === "in force", promptState);
  check("the page says whether the prompt on screen is the one the agent has",
    (await evaluate("document.getElementById('instructions-state').textContent")) === "in force",
    await evaluate("document.getElementById('instructions-state').textContent"));
  check("changing the prompt sends only the prompt",
    (await events("out")).some((line) =>
      line.includes('"instructions":"Answer in one word."') && !line.includes("openrealtime")),
    "a session.update carrying instructions and nothing negotiated");

  const carried = [];
  const silent = [];
  for (const id of ["obs.audio", "obs.text", "obs.screen", "obs.camera", "obs.browser", "obs.tools",
                    "act.speech", "act.text", "act.computer", "act.tools", "act.artifact", "act.download"]) {
    ((await channel(id)).count > 0 ? carried : silent).push(id);
  }
  console.log(`\ncarried: ${carried.join(", ")}`);
  console.log(`silent:  ${silent.join(", ") || "none"}`);

  check("all twelve input and output channels carried end to end",
    silent.length === 0, `silent: ${silent.join(", ")}`);

  check("no uncaught exception during the session", pageErrors.length === 0, pageErrors.join("; "));
} catch (failure) {
  check("the run completed", false, failure.stack ?? failure.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (${MODE})`);
process.exit(failed.length === 0 ? 0 : 1);
