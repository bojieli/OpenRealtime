// The bridge between a tool call on the wire and this machine.
//
// The session emits a function call, this page forwards it to the surface
// process, and the result goes back as an ordinary conversation item. No part
// of that is special: it is what the Realtime protocol already says happens.
// Three different things happen on the other end of this socket - a file is
// read, a browser is clicked, an artifact is rendered - and the protocol
// cannot tell them apart, which is the correct outcome. Only this page groups
// them, and only so a person can see which one occurred.
//
// Confirmation deliberately runs the other way round. The surface process
// asks, this page renders the question, and the answer goes back before
// anything executes. Putting the veto in the process that touches the world
// rather than in the page means a session that talked this page into skipping
// the dialog still could not make anything happen.

// The protocol carries function call arguments as a JSON-encoded string, not
// as an object. Parsing here means the tool messages carry the natural shape;
// a string that does not parse is forwarded as it arrived, so the tool host
// reports what was wrong with it rather than this page swallowing it.
function parseArguments(args) {
  if (typeof args !== "string") return args ?? {};
  try {
    return JSON.parse(args);
  } catch {
    return args;
  }
}

export class ToolBridge extends EventTarget {
  #socket = null;
  #pending = new Map();
  #confirm = null;

  constructor(confirmHandler) {
    super();
    this.#confirm = confirmHandler;
  }

  async connect() {
    const url = new URL("/api/tools", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    this.#socket = new WebSocket(url);
    this.#socket.addEventListener("message", (message) => this.#receive(message.data));
    this.#socket.addEventListener("close", () => this.#failAll("the tool host disconnected"));
    await new Promise((resolve, reject) => {
      this.#socket.addEventListener("open", resolve, { once: true });
      this.#socket.addEventListener("error",
        () => reject(new Error("could not reach the tool host")), { once: true });
    });
  }

  get connected() {
    return this.#socket?.readyState === WebSocket.OPEN;
  }

  // call runs one tool and resolves with what the session should receive. It
  // never rejects: a refusal, a failure, and a disconnection are all outcomes
  // the model has to be told about, and an unanswered call is the one thing
  // that would leave the session waiting on something never coming.
  call(callId, name, args) {
    return new Promise((resolve) => {
      if (!this.connected) {
        resolve({ error: "this surface has no tool host attached" });
        return;
      }
      this.#pending.set(callId, resolve);
      this.#socket.send(JSON.stringify({
        type: "call", id: callId, name, arguments: parseArguments(args),
      }));
    });
  }

  async #receive(payload) {
    let message;
    try {
      message = JSON.parse(payload);
    } catch {
      return;
    }
    if (message.type === "confirm") {
      const approved = await this.#confirm(message);
      this.#socket?.send(JSON.stringify({ type: "decide", id: message.id, approved }));
      this.dispatchEvent(new CustomEvent("decided", { detail: { ...message, approved } }));
      return;
    }
    if (message.type === "result") {
      const resolve = this.#pending.get(message.id);
      if (!resolve) return;
      this.#pending.delete(message.id);
      resolve({
        output: message.output, error: message.error,
        channel: message.channel, artifact: message.artifact,
      });
    }
  }

  #failAll(reason) {
    for (const [, resolve] of this.#pending) resolve({ error: reason });
    this.#pending.clear();
  }

  close() {
    this.#socket?.close();
    this.#socket = null;
  }
}

// The tool declarations this surface offers, in the shape session.update
// wants. They come from the host over /api/config rather than being written
// here, so what the model is told a tool is and what will actually run are one
// statement rather than two that can drift.
export function declarations(tools) {
  return tools.map((tool) => ({
    type: "function",
    name: tool.name,
    description: tool.description,
    parameters: tool.parameters,
    openrealtime: {
      confirm: tool.session_confirm,
      ...(tool.target ? { target: tool.target } : {}),
    },
  }));
}

// Which action channel a tool belongs to, as the host declared it.
export function channelOf(tools, name) {
  return tools.find((tool) => tool.name === name)?.channel ?? "tool";
}
