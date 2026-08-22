// OpenAI's own Realtime client, unmodified, against an OpenRealtime server.
//
//   node websocket.mjs ws://127.0.0.1:8765/v1/realtime
//
// Nothing here is written against OpenRealtime. It is the `@openai/agents-realtime`
// package as published, configured the way its own documentation says to
// configure it, with one URL pointed somewhere else. That is the entire point:
// the project claims an official client connects unchanged, and a claim like
// that is worth exactly as much as the thing that checks it.
//
// It runs a tool-using turn, because tool use is where a Realtime
// implementation is most likely to diverge - the call goes out on one event,
// the result comes back as a conversation item, and the SDK is particular
// about the shape of both.

import { RealtimeAgent, RealtimeSession, OpenAIRealtimeWebSocket, tool } from "@openai/agents-realtime";
import { z } from "zod";

const url = process.argv[2] ?? "ws://127.0.0.1:8765/v1/realtime";
const prompt = process.argv[3] ?? "What is the weather in Cambridge? Use your tool.";

const results = [];
function check(name, ok, detail = "") {
  results.push({ name, ok });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? `  — ${detail}` : ""}`);
}

let toolCalls = 0;
const weather = tool({
  name: "get_weather",
  description: "Look up the current weather for a city.",
  parameters: z.object({ city: z.string().describe("The city to look up.") }),
  async execute({ city }) {
    toolCalls++;
    console.log(`  (the SDK executed get_weather for ${JSON.stringify(city)})`);
    return `It is 14 degrees and raining in ${city}.`;
  },
});

const agent = new RealtimeAgent({
  name: "Assistant",
  instructions: "You are concise. When asked about weather, call your tool and then say the answer.",
  tools: [weather],
});

// The transport is the SDK's own, with its own URL option. useInsecureApiKey is
// how the SDK is told the credential is a plain key rather than an ephemeral
// client secret, which is the right shape for a server that issues neither.
const transport = new OpenAIRealtimeWebSocket({ url, useInsecureApiKey: true });
const session = new RealtimeSession(agent, {
  transport,
  model: process.env.OPENREALTIME_MODEL ?? "openrealtime",
  config: {
    outputModalities: ["audio"],
    audio: {
      input: { format: { type: "audio/pcm", rate: 24000 } },
      output: { format: { type: "audio/pcm", rate: 24000 } },
    },
  },
});

const seen = new Set();
const transcript = [];
let audioBytes = 0;
let failure = null;

session.on("transport_event", (event) => {
  seen.add(event.type);
  if (event.type === "response.output_audio.delta" && typeof event.delta === "string") {
    audioBytes += Math.floor((event.delta.length * 3) / 4);
  }
  if (event.type === "response.output_audio_transcript.delta" && event.delta) {
    transcript.push(event.delta);
  }
});
const errors = [];
session.on("error", (error) => {
  // The SDK surfaces every server error here. Recording rather than throwing is
  // the point of this test: an error about one field must not stop a session,
  // and the only way to know the SDK agrees is to send it one and keep going.
  const reported = error?.error?.error ?? error?.error ?? error;
  errors.push(reported);
  failure = reported;
  console.log(`  (session error: ${JSON.stringify(reported)?.slice(0, 200)})`);
});

const deadline = (label, ms) => new Promise((_, reject) =>
  setTimeout(() => reject(new Error(`timed out waiting for ${label}`)), ms));

const waitFor = async (label, predicate, ms = 30000) => {
  const start = Date.now();
  while (Date.now() - start < ms) {
    if (predicate()) return true;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`timed out waiting for ${label}`);
};

try {
  await Promise.race([
    session.connect({ apiKey: process.env.OPENREALTIME_TOKEN ?? "not-a-real-key" }),
    deadline("the session to connect", 20000),
  ]);
  check("the official SDK connects to an OpenRealtime server", true, url);

  await waitFor("session.created", () => seen.has("session.created"));
  check("the server completes the SDK's session handshake",
    seen.has("session.created") && seen.has("session.updated"),
    [...seen].filter((type) => type.startsWith("session.")).join(", "));

  // The SDK sends its own session.update built from the agent: instructions,
  // tools, modalities, audio formats. All of that applies. What does not is
  // semantic_vad, which this deployment does not have - and the server says so
  // by name rather than substituting a detector the client did not ask for.
  const unsupported = errors.filter((error) => error?.code === "unsupported_value");
  check("the unsupported field is refused by name, not silently substituted",
    unsupported.some((error) => error?.param === "session.audio.input.turn_detection.type"),
    JSON.stringify(unsupported.map((error) => error?.param)));
  check("nothing else in the session was refused",
    errors.length === unsupported.length,
    JSON.stringify(errors.filter((error) => error?.code !== "unsupported_value")));

  session.sendMessage(prompt);

  await waitFor("the tool to be called and answered", () => toolCalls > 0, 40000);
  check("the SDK's tool was called and executed by the SDK", toolCalls > 0, `${toolCalls} call(s)`);

  await waitFor("the response to complete", () => seen.has("response.done"), 40000);
  check("a response completed", seen.has("response.done"));
  check("the SDK received the tool call on the event it expects",
    seen.has("response.function_call_arguments.done"),
    [...seen].filter((type) => type.includes("function_call")).join(", "));
  check("speech came back as audio and as a transcript",
    audioBytes > 0 && transcript.length > 0,
    `${audioBytes} bytes of audio, ${transcript.join("").length} characters of transcript`);
  // The whole turn happened after the server reported that error, which is
  // what makes it an error about a field rather than a failed session. If the
  // SDK treated it as fatal, none of the checks above would have passed.
  check("the session completed a turn despite the error", toolCalls > 0 && seen.has("response.done"));

  console.log(`\n  the agent said: ${transcript.join("").trim() || "(nothing)"}`);
} catch (thrown) {
  check("the run completed", false, thrown.message);
} finally {
  session.close();
}

const failed = results.filter((result) => !result.ok);
console.log(`\n${results.length - failed.length}/${results.length} checks passed (official SDK, WebSocket)`);
process.exit(failed.length === 0 ? 0 : 1);
