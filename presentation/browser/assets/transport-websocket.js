export default {
  name: "openrealtime.presentation.client.websocket",
  revision: 1,
  async mount(context) {
    if (!context.permissions.allows("network.connect", "host-realtime", "websocket")) {
      throw new Error("WebSocket transport lacks its deployment grant");
    }
    const endpoint = context.manifest.endpoints?.find((candidate) => candidate.name === "realtime.websocket");
    if (!endpoint || endpoint.method !== "GET") throw new Error("WebSocket endpoint is not declared");
    let socket;
    let connecting;
    let state = "idle";
    const listeners = new Set();
    const states = new Set();
    const notifyState = () => {
      for (const listener of states) {
        try { listener(state); } catch {}
      }
    };
    const notifyMessage = (message) => {
      for (const listener of listeners) {
        try { listener(message); } catch {}
      }
    };
    const connection = Object.freeze({
      kind: "websocket",
      state: () => state,
      onState(listener) {
        if (typeof listener !== "function") throw new Error("transport state listener must be a function");
        states.add(listener);
        try { listener(state); } catch {}
        return () => states.delete(listener);
      },
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("transport message listener must be a function");
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
      async connect() {
        if (socket?.readyState === WebSocket.OPEN) return;
        if (connecting) return connecting;
        state = "connecting"; notifyState();
        const scheme = location.protocol === "https:" ? "wss:" : "ws:";
        const candidate = new WebSocket(`${scheme}//${location.host}${endpoint.path}`);
        socket = candidate;
        candidate.addEventListener("message", (message) => {
          if (socket === candidate) notifyMessage(message.data);
        });
        candidate.addEventListener("close", () => {
          if (socket !== candidate) return;
          socket = undefined;
          state = "closed"; notifyState();
        });
        connecting = (async () => {
          try {
            await new Promise((resolve, reject) => {
              const closed = () => reject(new Error("WebSocket closed before connecting"));
              candidate.addEventListener("close", closed, { once: true });
              candidate.addEventListener("open", () => {
                candidate.removeEventListener("close", closed);
                resolve();
              }, { once: true });
              candidate.addEventListener("error", () => reject(new Error("WebSocket connection failed")), { once: true });
            });
          } catch (error) {
            if (socket === candidate) {
              socket = undefined;
              state = "failed"; notifyState();
              candidate.close();
            }
            throw error;
          }
          if (socket !== candidate) throw new Error("WebSocket connection was superseded");
          state = "connected"; notifyState();
        })();
        try {
          await connecting;
        } finally {
          connecting = undefined;
        }
      },
      send(event) {
        if (!socket || socket.readyState !== WebSocket.OPEN) throw new Error("WebSocket is not connected");
        socket.send(typeof event === "string" ? event : JSON.stringify(event));
      },
      close() {
        const current = socket;
        socket = undefined;
        current?.close(1000, "client closed");
        state = "closed"; notifyState();
      },
    });
    context.publish("presentation.client.connection", connection);
    context.lifecycle.defer("websocket", () => connection.close());
  },
};
