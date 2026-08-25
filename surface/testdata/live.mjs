// Drive the surface against real models and report what the agent chose to do.
//
//   node live.mjs <surface url>
//
// The scripted gate proves the channels carry. This proves something the
// scripted gate structurally cannot: that a real model, given these tool
// descriptions and nothing else, reaches for them and puts something usable
// inside. A scripted provider issues the call the test asked for, so it says
// nothing about whether display_artifact is described well enough that a model
// thinks to render rather than to read aloud, or whether the HTML it writes is
// a document or an apology.
//
// So the assertions here are about the model's choices and the substance of
// what it produced, not about plumbing. Timeouts are long because real models
// are slow, and every wait is on a condition rather than a duration.

import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const SURFACE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19224);
const SPEECH = process.env.OPENREALTIME_LIVE_AUDIO;

if (!SURFACE_URL) {
  console.error("usage: node live.mjs <surface url>");
  process.exit(2);
}

const profile = mkdtempSync(join(tmpdir(), "openrealtime-live-"));
const chromium = spawn(process.env.CHROMIUM ?? "chromium", [
  "--headless=new",
  `--remote-debugging-port=${PORT}`,
  "--no-sandbox",
  "--disable-gpu",
  "--use-fake-device-for-media-stream",
  "--use-fake-ui-for-media-stream",
  // A real recogniser needs real speech: a tone transcribes to nothing, so
  // without a file the audio channel is left out of this run rather than
  // asserted on evidence that cannot exist.
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
  browser.on("Runtime.exceptionThrown", (params) =>
    pageErrors.push(params.exceptionDetails.exception?.description ?? params.exceptionDetails.text));

  // Errors the session reported, which is a different thing from errors the
  // page threw and is the one that was missing. A run where the provider
  // refused every request for six minutes looked, from here, exactly like a
  // model choosing not to act - and the surface had been showing the reason on
  // its own tool-results channel the whole time.
  const sessionErrors = () => evaluate(`
    [...document.querySelectorAll('.channel[data-channel="obs.tools"] .entries li')]
      .filter(entry => entry.querySelector('.entry-title').textContent === 'surface')
      .map(entry => entry.querySelector('.entry-body').textContent.slice(0, 300))`);

  const call = (method, params) => browser.send(method, params, sessionId);

  // A confirmation nobody answers is a run that stalls rather than fails, and
  // an unattended run has nobody. So the dialog is answered here - approved,
  // because the point of this run is to find out what the models do, and a
  // refusal would answer that question for them. The dispatcher still refuses
  // anything outside the declared target after the approval, which is the
  // check that matters and the one a person cannot wave through.
  const approvals = [];
  setInterval(async () => {
    try {
      const asked = await evaluate(`(() => {
        const dialog = document.getElementById('confirm');
        if (!dialog?.open) return null;
        const name = document.getElementById('confirm-name').textContent;
        dialog.returnValue = 'allow';
        dialog.close('allow');
        return name;
      })()`);
      if (asked) approvals.push(asked);
    } catch {}
  }, 1000);
  await call("Runtime.enable");
  await call("Page.enable");
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

  const channel = (id) => evaluate(`(() => {
    const card = document.querySelector('.channel[data-channel="${id}"]');
    if (!card) return null;
    return {
      count: Number(card.querySelector('.count').textContent),
      caption: card.querySelector('.stage-caption')?.textContent ?? "",
      marks: card.querySelectorAll('.mark').length,
      entries: [...card.querySelectorAll('.entries li')].map(entry => ({
        title: entry.querySelector('.entry-title').textContent,
        body: entry.querySelector('.entry-body').textContent.slice(0, 600),
        outcome: entry.dataset.outcome ?? null,
      })),
    };
  })()`);

  const waitFor = async (what, predicate, timeoutMs = 180000) => {
    const deadline = Date.now() + timeoutMs;
    let last;
    while (Date.now() < deadline) {
      last = await predicate();
      if (last) return last;
      await sleep(500);
    }
    console.log(`  (timed out after ${Math.round(timeoutMs / 1000)}s waiting for ${what})`);
    return last;
  };

  const say = (text) => evaluate(`
    document.getElementById('typed').value = ${JSON.stringify(text)};
    document.getElementById('compose').dispatchEvent(new Event('submit', {cancelable: true}));`);


  // --- real speech, if any was supplied -------------------------------------

  if (SPEECH) {
    // The speech is decoded in the page and routed through a stream
    // destination rather than handed to Chromium's fake-file capture flag.
    // That flag depends on a format and a lifetime this test does not control:
    // the file starts when the browser does, so a clip has finished before
    // there is a session to hear it, and a rate the build does not like is
    // silence with no error. Decoding it here means the audio starts when the
    // microphone opens, loops for as long as the turn needs, and is the same
    // samples whatever the file was.
    const wav = readFileSync(SPEECH).toString("base64");
    const installed = await evaluate(`(async () => {
      const bytes = Uint8Array.from(atob(${JSON.stringify(wav)}), c => c.charCodeAt(0));
      const context = new AudioContext();
      const decoded = await context.decodeAudioData(bytes.buffer);
      const destination = context.createMediaStreamDestination();
      const source = context.createBufferSource();
      source.buffer = decoded;
      source.loop = true;
      source.connect(destination);
      source.start();
      const real = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
      navigator.mediaDevices.getUserMedia = async (constraints) => {
        if (!constraints || !constraints.audio) return real(constraints);
        const stream = await real({ ...constraints, audio: false }).catch(() => new MediaStream());
        const voice = new MediaStream([destination.stream.getAudioTracks()[0]]);
        stream.getVideoTracks().forEach((track) => voice.addTrack(track));
        return voice;
      };
      return decoded.duration;
    })()`);
    check("the speech decoded in the page", installed > 0, installed + " seconds");
  }

  // --- connect -------------------------------------------------------------

  await evaluate("document.getElementById('connect').click()");
  const live = ["connected", "listening", "thinking", "responding"];
  await waitFor("the session to open", async () =>
    live.includes(await evaluate("document.getElementById('state').textContent")), 60000);
  check("the session connects to the live endpoint",
    live.includes(await evaluate("document.getElementById('state').textContent")),
    await evaluate("document.getElementById('state').textContent"));

  await waitFor("session.updated", async () =>
    (await evaluate("document.getElementById('negotiated').textContent")).length > 0, 60000);
  const negotiated = await evaluate("document.getElementById('negotiated').textContent");
  console.log(`\nnegotiated: ${negotiated}\n`);

  // The three video channels are only offered when the server negotiated
  // video input, which a deployment with no vision model cannot do. Saying so
  // is the point rather than a caveat: a channel that is dark because nobody
  // configured a narrator and a channel that is dark because it is broken look
  // identical, and this is the line that tells them apart.
  const videoNegotiated = negotiated.includes("video.input");
  if (!videoNegotiated) {
    console.log("  (this endpoint negotiated no video input, so the screen, camera, and");
    console.log("   browser channels are not exercised in this run — it has no vision model)\n");
  } else {
    await evaluate("document.getElementById('browser').click()");
    await waitFor("browser frames", async () => (await channel("obs.browser")).caption.length > 0, 60000);
  }

  // --- real speech, if there is any -----------------------------------------

  if (SPEECH) {
    const events = () => evaluate(
      `[...document.querySelectorAll('#events div')].map(line => line.textContent)`);
    await evaluate("document.getElementById('filter-media').checked = false");
    const heard = await waitFor("the gate to hear the speech", async () =>
      (await events()).some((line) => line.includes('"input_audio_buffer.speech_started"')), 60000);
    check("the server's gate hears real speech", Boolean(heard));

    // Silence, not absence: muting keeps the track flowing, which is what lets
    // the gate decide the turn ended. A client that simply stopped sending
    // would leave the floor held.
    await evaluate("document.getElementById('mic').click()");
    const transcribed = await waitFor("a real recogniser to transcribe it", async () =>
      (await channel("obs.audio")).entries.some((entry) => entry.title.includes("speech")), 120000);
    check("a real recogniser transcribed real speech", Boolean(transcribed),
      JSON.stringify((await channel("obs.audio")).entries.map((entry) => entry.body.slice(0, 160))));
  }

  // --- the video channels, if this endpoint can see -------------------------

  if (videoNegotiated) {
    await evaluate("document.getElementById('camera').click()");

    const narratedBrowser = await waitFor("a real vision model to narrate the browser",
      async () => (await channel("obs.browser")).entries.length > 0);
    check("a real vision model narrated the browser", Boolean(narratedBrowser),
      JSON.stringify((await channel("obs.browser")).entries.map((e) => e.body.slice(0, 200))));

    const narratedCamera = await waitFor("a real vision model to narrate the camera",
      async () => (await channel("obs.camera")).entries.length > 0);
    check("a real vision model narrated the camera", Boolean(narratedCamera),
      JSON.stringify((await channel("obs.camera")).entries.map((e) => e.body.slice(0, 160))));

    // Stopped once it has narrated. A camera pointed at a room produces an
    // observation every third of a second forever, and the first run of this
    // spent forty thousand tokens of context on a synthetic test pattern
    // before it got to the question it was asking - at which point the
    // provider refused every request and the model's silence looked like a
    // decision. Perception is not free, and a bench that pretends otherwise
    // measures the wrong thing.
    await evaluate("document.getElementById('camera').click()");

    // The hardest thing this whole surface can be asked to do, and the reason
    // the browser channel is worth having: a model that has only ever been
    // told about the page in words, by another model that looked at it, is
    // asked to press something on it. Nothing in the prompt says where the
    // button is. If the coordinate is right, perception and action met in the
    // same space.
    //
    // The instruction goes through the surface's own prompt panel first,
    // because the first run of this got "I see a green button labeled 'Press
    // me' at position (460, 156). Shall I click it?" - which is a model doing
    // something reasonable and a test learning nothing. An agent that asks
    // before acting is a configuration, not a capability, and the point here
    // is the capability.
    await evaluate(`
      document.getElementById('instructions').value = ${JSON.stringify(
        "You are operating a browser on the person's behalf. When they ask you to press or click " +
        "something on the screen, call computer.click immediately, using the coordinates from the " +
        "most recent observation of that source. Do not ask for confirmation and do not describe " +
        "what you are about to do first - click, then say what happened in a few words.")};
      document.getElementById('apply-instructions').click();`);
    await waitFor("the new prompt to be in force", async () =>
      (await evaluate("document.getElementById('instructions-state').textContent")) === "in force",
      30000);
    check("the prompt that governs the action is the one on screen",
      (await evaluate("document.getElementById('instructions-state').textContent")) === "in force");

    await say("Press the green button on the browser screen now.");
    const acted = await waitFor("the model to act on what it was shown", async () =>
      (await channel("act.computer")).entries.length > 0, 240000);
    check("a real model chose a computer-use action from what it was shown", Boolean(acted),
      JSON.stringify((await channel("act.computer")).entries.map((e) => `${e.title} ${e.body}`)));
    check("the action was performed rather than refused",
      (await channel("act.computer")).entries.some((entry) => entry.outcome === "done"),
      JSON.stringify((await channel("act.computer")).entries.map((entry) => entry.outcome)));

    // And the page it aimed at actually moved. A coordinate inside the
    // viewport is accepted by the dispatcher whether or not anything was
    // there, so this is the only assertion that separates a model that saw
    // the button from a model that guessed the middle of the screen.
    const landed = await waitFor("the page to react to what the model pressed", async () =>
      (await channel("obs.browser")).caption.includes("#pressed"), 120000);
    check("the model pressed the button it had been told about, not the page",
      Boolean(landed), (await channel("obs.browser")).caption);

    // And stopped, for the same reason the camera was. The browser channel has
    // answered the question it was turned on for, and leaving it narrating
    // spends the rest of the session's context describing a page nobody is
    // going to ask about again.
    await evaluate("document.getElementById('browser').click()");
  }

  // --- a real model, a real tool -------------------------------------------

  await say("Read the file notes.txt and tell me the deadline.");
  check("what was typed is rendered on the text channel",
    (await channel("obs.text")).entries.some((entry) => entry.body.includes("notes.txt")));

  const readIt = await waitFor("the model to read the file", async () =>
    (await channel("act.tools")).entries.some((entry) => entry.title === "read_file"));
  check("a real model chose to call a tool", Boolean(readIt),
    JSON.stringify((await channel("act.tools")).entries.map((entry) => entry.title)));

  const returned = await waitFor("the tool result to come back", async () =>
    (await channel("obs.tools")).entries.some((entry) => entry.body.includes("14 November")));
  check("the file this machine holds reached the session", Boolean(returned),
    JSON.stringify((await channel("obs.tools")).entries.map((entry) => entry.title)));

  const answered = await waitFor("the agent to say the answer", async () => {
    const spoken = (await channel("act.speech")).entries.map((entry) => entry.body).join(" ");
    const written = (await channel("act.text")).entries.map((entry) => entry.body).join(" ");
    return /14 November|November 14|Friday/i.test(spoken + written);
  });
  check("the agent answered from what it read rather than from what it guessed",
    Boolean(answered),
    JSON.stringify((await channel("act.speech")).entries.map((entry) => entry.body.slice(0, 120))));

  // --- generative UI, chosen by the model -----------------------------------

  await say("Now show me that as a small HTML table artifact I can look at.");
  const rendered = await waitFor("the model to render an artifact", async () =>
    (await channel("act.artifact")).entries.length > 0);
  check("a real model chose to render generative UI", Boolean(rendered),
    JSON.stringify((await channel("act.artifact")).entries.map((entry) => entry.body)));

  await waitFor("the artifact frame", async () =>
    await evaluate("document.querySelectorAll('.artifact-frame').length > 0"), 60000);
  const source = await evaluate(
    "document.querySelector('.artifact-frame')?.getAttribute('src') ?? ''");
  check("the artifact is framed from its own route", source.startsWith("/artifacts/"), source);
  // Without this the next fetch asks for "" and gets this page back, and a
  // check named "the artifact is a real HTML document" passes on the surface's
  // own markup. A false pass is worse than the failure it hides.
  if (!source.startsWith("/artifacts/")) {
    throw new Error("no artifact was rendered, so there is nothing to inspect");
  }

  // What the model actually wrote. An artifact that renders is not the same as
  // an artifact worth rendering, and the difference is visible only in the
  // document itself.
  const html = await evaluate(
    `fetch(${JSON.stringify(source)}).then(response => response.text())`);
  console.log(`\n--- the artifact the model wrote (${html.length} bytes) ---\n${html.slice(0, 1200)}\n---\n`);
  check("the artifact is a real HTML document", /<html|<!doctype/i.test(html), `${html.length} bytes`);
  check("the artifact contains the table that was asked for", /<table/i.test(html));
  check("the artifact carries the answer the model had read",
    /14 November|November 14|Friday/i.test(html));

  const frame = await evaluate(`(() => {
    const element = document.querySelector('.artifact-frame');
    const box = element.getBoundingClientRect();
    return { width: box.width, height: box.height };
  })()`);
  check("the artifact frame has been laid out", frame.width > 100 && frame.height > 100,
    JSON.stringify(frame));

  // --- what carried ---------------------------------------------------------

  const carried = [];
  const silent = [];
  for (const id of ["obs.audio", "obs.text", "obs.screen", "obs.camera", "obs.browser", "obs.tools",
                    "act.speech", "act.text", "act.computer", "act.tools", "act.artifact"]) {
    ((await channel(id)).count > 0 ? carried : silent).push(id);
  }
  console.log(`\ncarried: ${carried.join(", ")}`);
  console.log(`silent:  ${silent.join(", ") || "none"}`);

  const requiredChannels = ["obs.text", "obs.tools", "act.tools", "act.artifact"];
  if (SPEECH) requiredChannels.push("obs.audio");
  if (videoNegotiated) requiredChannels.push("obs.browser", "obs.camera", "act.computer");
  for (const required of requiredChannels) {
    check(`${required} carried something against a real model`, carried.includes(required));
  }
  if (approvals.length) console.log(`\nconfirmations approved: ${approvals.join(", ")}`);
  const reported = await sessionErrors();
  if (reported.length) {
    console.log(`\nthe session reported ${reported.length} error(s):`);
    for (const failure of reported.slice(0, 5)) console.log(`  - ${failure}`);
  }
  check("the session reported no errors of its own", reported.length === 0,
    reported.slice(0, 2).join(" | "));
  check("no uncaught exception during the live session", pageErrors.length === 0,
    pageErrors.join("; "));
} catch (failure) {
  check("the run completed", false, failure.stack ?? failure.message);
} finally {
  chromium.kill("SIGKILL");
  rmSync(profile, { recursive: true, force: true });
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (live)`);
process.exit(failed.length === 0 ? 0 : 1);
