export default {
  name: "openrealtime.presentation.client.text-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const state = context.services.get("presentation.client.session_state");
    if (!slots || !state) throw new Error("text view dependencies are unavailable");
    const section = document.createElement("section");
    section.className = "conversation-panel";
    section.innerHTML = `<style>
      :root { color-scheme:dark; font-family:Inter,ui-sans-serif,system-ui,sans-serif; background:#0c1019; color:#e9edf5; }
      * { box-sizing:border-box; } body { margin:0; }
      button,input,textarea,select { font:inherit; }
      button { border:1px solid #354058; border-radius:12px; background:#202a3c; color:#e9edf5; padding:.75rem 1rem; cursor:pointer; }
      button:hover { background:#303e56; } button:disabled { opacity:.45; cursor:not-allowed; }
      button[aria-pressed=true] { background:#275c50; border-color:#58c5a5; }
      button:focus-visible,input:focus-visible,summary:focus-visible { outline:3px solid #8ac9ff; outline-offset:3px; }
      input,textarea,select { background:#111827; color:#e9edf5; border:1px solid #354058; border-radius:10px; padding:.8rem; min-width:0; }
      .room-shell { display:grid; grid-template-columns:minmax(0,1fr) 360px; gap:20px; max-width:1720px; margin:auto; padding:24px; min-height:100dvh; }
      .room-stage { min-width:0; } .room-conversation { min-width:0; }
      .room-text-only { grid-template-columns:1fr; max-width:1000px; }
      .room-tools { grid-column:1/-1; border-top:1px solid #263147; padding:18px 0; color:#9dabc1; }
      .room-tools summary { cursor:pointer; } .room-tools section { max-width:100%; margin:20px 0; padding:16px; overflow:auto; }
      .conversation-panel { height:calc(100dvh - 90px); min-height:500px; display:flex; flex-direction:column; background:#151d2b; border:1px solid #263147; border-radius:20px; padding:20px; }
      .conversation-panel header { display:flex; flex-wrap:wrap; align-items:center; gap:10px; }
      .conversation-panel h1 { font-size:18px; margin:0; flex:1; } #state { font-size:12px; color:#a7dcca; }
      #connect { background:#396fea; } #error { color:#ffb5b5; font-size:13px; overflow-wrap:anywhere; }
      #conversation { flex:1; overflow:auto; padding:16px 0; min-height:180px; }
      #conversation:empty::before { content:"Your conversation appears here. Join the room to talk, type, or share something you see."; color:#92a0b6; line-height:1.7; }
      #conversation article { background:#202b3e; border-radius:14px; padding:14px; margin:0 0 12px; white-space:pre-wrap; overflow-wrap:anywhere; font-size:14px; line-height:1.6; }
      #conversation article[data-role=user] { background:#243857; margin-left:22px; }
      .conversation-panel form { display:flex; gap:8px; } #text { width:100%; } .conversation-panel details { font-size:11px; color:#94a3b8; margin-top:12px; overflow-wrap:anywhere; }
      @media(max-width:1000px) { .room-shell { grid-template-columns:1fr; padding:12px; } .conversation-panel { height:440px; min-height:0; } }
      @media(prefers-reduced-motion:reduce) { * { animation:none!important; transition:none!important; } }
    </style><header><h1>Conversation</h1><span id="state">idle</span><button id="connect">Join room</button><button id="leave" hidden>Leave</button></header>
    <p id="error" role="alert"></p><div id="conversation" role="log" aria-label="Conversation" aria-live="polite"></div>
    <form><input id="text" aria-label="Message" placeholder="Message the room…" autocomplete="off"><button>Send</button></form>
    <details><summary>Connection details</summary><p id="identity"></p></details>`;
    const status = section.querySelector("#state");
    const conversation = section.querySelector("#conversation");
    const error = section.querySelector("#error");
    section.querySelector("#identity").textContent = `client ${context.manifest.plan.fingerprint}`;
    const render = (snapshot, diagnostic = "") => {
      status.textContent = snapshot.connection.phase;
      const connected = snapshot.connection.phase === "connected";
      section.querySelector("#connect").hidden = connected;
      section.querySelector("#leave").hidden = !connected;
      section.querySelector("#text").disabled = !connected;
      error.textContent = diagnostic || snapshot.last_error || "";
      conversation.replaceChildren(...snapshot.conversation.filter((turn, index, turns) => {
        if (turn.role !== "observation") return true;
        if (turn.text === "The user attached an image." || turn.text.startsWith("Current shared ")) return false;
        return !turns.slice(0, index).some((prior) => prior.role === "user" && prior.text === turn.text);
      }).map((turn) => {
        const article = document.createElement("article");
        article.dataset.role = turn.role;
        article.dataset.channel = turn.channel;
        article.textContent = `${turn.role === "assistant" ? "OpenRealtime" : turn.role === "observation" ? "Heard" : "You"}: ${turn.text}`;
        return article;
      }));
    };
    const unregister = slots.register("root", section, 0);
    const unsubscribe = state.subscribe(render);
    const connect = () => state.connect().catch((failure) => { error.textContent = failure.message; });
    section.querySelector("#connect").addEventListener("click", connect);
    const leave = () => state.disconnect();
    section.querySelector("#leave").addEventListener("click", leave);
    const form = section.querySelector("form");
    const submit = (event) => {
      event.preventDefault();
      const input = section.querySelector("#text");
      try { state.sendText(input.value); input.value = ""; }
      catch (failure) { error.textContent = failure.message; }
    };
    form.addEventListener("submit", submit);
    context.lifecycle.defer("view-events", () => {
      section.querySelector("#connect").removeEventListener("click", connect);
      form.removeEventListener("submit", submit);
      section.querySelector("#leave").removeEventListener("click", leave);
    });
    context.lifecycle.defer("view-state", unsubscribe);
    context.lifecycle.defer("view-slot", unregister);
  },
};
