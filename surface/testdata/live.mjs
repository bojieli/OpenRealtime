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
import { mkdtempSync, rmSync } from "node:fs";
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
  // Looped, because the file starts playing when the browser does and the
  // page connects seconds later; a clip that played once has finished before
  // there is a session to hear it. The silence a recogniser needs to endpoint
  // comes from muting instead, which is what a person stopping talking looks
  // like on the wire.
  ...(SPEECH ? [`--use-file-for-fake-audio-capture=${SPEECH}`] : []),
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

  const call = (method, params) => browser.send(method, params, sessionId);
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
  for (const required of requiredChannels) {
    check(`${required} carried something against a real model`, carried.includes(required));
  }
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
