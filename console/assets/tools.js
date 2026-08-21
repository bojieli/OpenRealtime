// The bridge between a tool call on the wire and this machine.
//
// The session emits a function call, this page forwards it to the console
// process, and the result goes back as an ordinary conversation item. No part
// of that is special: it is what the Realtime protocol already says happens,
// and the only reason it needs a page at all is that a browser cannot open a
// file.
//
// Confirmation deliberately runs the other way round. The console asks, this
// page renders the question, and the answer goes back before anything
// executes. Putting the veto in the process that touches the disk rather than
// in the page means a session that talked this page into skipping the dialog
// still could not make anything happen.

// The protocol carries function call arguments as a JSON-encoded string, not
// as an object. Parsing here means the console's own tool messages carry the
// natural shape; a string that does not parse is forwarded as it arrived, so
// the tool host reports what was wrong with it rather than this page swallowing
// it.
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
      this.#socket.addEventListener("error", () => reject(new Error("could not reach the tool host")), { once: true });
    });
  }

  get connected() {
    return this.#socket?.readyState === WebSocket.OPEN;
  }

  // call runs one tool and resolves with the text the session should receive.
  // It never rejects: a refusal, a failure, and a timeout are all outcomes the
  // model has to be told about, and an unanswered call is the one thing that
  // would leave the session waiting.
  call(callId, name, args) {
    return new Promise((resolve) => {
      if (!this.connected) {
        resolve({ error: "this console has no tool host attached" });
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
      resolve({ output: message.output, error: message.error });
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

// The tool declarations the console offers, in the shape session.update wants.
// The confirmation requirement travels with the definition, so what the model
// is told about a tool and what the console will enforce are one statement.
export function declarations(tools) {
  return tools.map((tool) => ({
    type: "function",
    name: tool.name,
    description: tool.description,
    parameters: tool.parameters,
    openrealtime: { confirm: tool.confirm },
  }));
}
