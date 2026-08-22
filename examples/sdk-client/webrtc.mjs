// OpenAI's own Realtime client over WebRTC, unmodified, against an
// OpenRealtime WebRTC adapter.
//
// This half has to run in a browser: the SDK's WebRTC transport uses
// RTCPeerConnection and getUserMedia, and there is no Node equivalent. It is
// bundled with esbuild and loaded by webrtc.html, which the test serves.
//
// The transport's own `baseUrl` option is the only thing pointed anywhere
// unusual. Everything else - the SDP exchange, the `oai-events` data channel,
// the session configuration, the tool round trip - is the SDK doing what it
// does against OpenAI.

import { RealtimeAgent, RealtimeSession, OpenAIRealtimeWebRTC, tool } from "@openai/agents-realtime";
import { z } from "zod";

const results = [];
const record = (name, ok, detail = "") => {
  results.push({ name, ok, detail });
  window.__results = results;
};

let toolCalls = 0;
const weather = tool({
  name: "get_weather",
  description: "Look up the current weather for a city.",
  parameters: z.object({ city: z.string().describe("The city to look up.") }),
  async execute({ city }) {
    toolCalls++;
    return `It is 14 degrees and raining in ${city}.`;
  },
});

const agent = new RealtimeAgent({
  name: "Assistant",
  instructions: "You are concise. When asked about weather, call your tool and then say the answer.",
  tools: [weather],
});

const seen = new Set();
const transcript = [];
let failure = null;

window.__run = async ({ baseUrl, apiKey, prompt }) => {
  const transport = new OpenAIRealtimeWebRTC({ baseUrl, useInsecureApiKey: true });
  const session = new RealtimeSession(agent, { transport, model: "openrealtime" });

  session.on("transport_event", (event) => {
    seen.add(event.type);
    if (event.type === "response.output_audio_transcript.delta" && event.delta) {
      transcript.push(event.delta);
    }
  });
  const errors = [];
  session.on("error", (error) => {
    const reported = error?.error?.error ?? error?.error ?? error;
    errors.push(reported);
    failure = reported;
  });

  const waitFor = async (label, predicate, ms = 30000) => {
    const start = Date.now();
    while (Date.now() - start < ms) {
      if (predicate()) return;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    throw new Error(`timed out waiting for ${label}`);
  };

  try {
    await session.connect({ apiKey });
    record("the official SDK connects over WebRTC", true, baseUrl);

    await waitFor("the session handshake",
      () => seen.has("session.created") && seen.has("session.updated"));
    record("the server completes the SDK's session handshake", true);
    // semantic_vad is the SDK's default and this deployment does not have it.
    // The field is refused by name; everything else in the same event applies.
    const unsupported = errors.filter((error) => error?.code === "unsupported_value");
    record("the unsupported field is refused by name, not silently substituted",
      unsupported.some((error) => error?.param === "session.audio.input.turn_detection.type"),
      JSON.stringify(unsupported.map((error) => error?.param)));
    record("nothing else in the session was refused",
      errors.length === unsupported.length,
      JSON.stringify(errors.filter((error) => error?.code !== "unsupported_value")));

    // The peer connection carries the microphone, so the fake device is
    // already talking and holding the floor. Muting sends silence, which is
    // what lets the turn end.
    session.mute(true);
    await new Promise((resolve) => setTimeout(resolve, 2000));

    session.sendMessage(prompt);
    await waitFor("the tool to be called", () => toolCalls > 0, 40000);
    record("the SDK's tool was called and executed by the SDK", toolCalls > 0, `${toolCalls} call(s)`);

    await waitFor("the response to complete", () => seen.has("response.done"), 40000);
    record("a response completed", true);
    record("the SDK received the tool call on the event it expects",
      seen.has("response.function_call_arguments.done"));
    record("a transcript came back", transcript.join("").length > 0,
      `${transcript.join("").length} characters`);
    record("the session completed a turn despite the error",
      toolCalls > 0 && seen.has("response.done"));
  } catch (thrown) {
    record("the run completed", false, thrown.message);
  } finally {
    session.close();
  }
  window.__done = true;
  return results;
};

window.__ready = true;
