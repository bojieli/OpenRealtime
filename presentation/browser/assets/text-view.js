export default {
  name: "openrealtime.presentation.client.text-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const state = context.services.get("presentation.client.session_state");
    if (!slots || !state) throw new Error("text view dependencies are unavailable");
    const section = document.createElement("section");
    section.innerHTML = `<style>
      :root { color-scheme: light dark; font-family: system-ui, sans-serif; }
      body { margin: 0; } section { max-width: 54rem; margin: 2rem auto; padding: 1rem; }
      header { display:flex; gap:.75rem; align-items:center; } #conversation { min-height:12rem; }
      article { margin:.6rem 0; padding:.75rem; border:1px solid #8885; border-radius:.6rem; }
      article[data-role=user] { margin-left:15%; } article[data-role=assistant] { margin-right:15%; }
      form { display:flex; gap:.5rem; } input { flex:1; padding:.7rem; } button { padding:.7rem 1rem; }
      #error { color:#c33; }
    </style><header><h1>OpenRealtime</h1><span id="state">idle</span><button id="connect">Connect</button></header>
    <p id="identity"></p><p id="error" role="alert"></p><div id="conversation"></div>
    <form><input id="text" aria-label="Message" autocomplete="off"><button>Send</button></form>`;
    const status = section.querySelector("#state");
    const conversation = section.querySelector("#conversation");
    const error = section.querySelector("#error");
    section.querySelector("#identity").textContent = `client ${context.manifest.plan.fingerprint}`;
    const render = (snapshot, diagnostic = "") => {
      status.textContent = snapshot.connection.phase;
      error.textContent = diagnostic || snapshot.last_error || "";
      conversation.replaceChildren(...snapshot.conversation.map((turn) => {
        const article = document.createElement("article");
        article.dataset.role = turn.role;
        article.dataset.channel = turn.channel;
        article.textContent = `${turn.role}: ${turn.text}`;
        return article;
      }));
    };
    const unregister = slots.register("root", section, 0);
    const unsubscribe = state.subscribe(render);
    const connect = () => state.connect().catch((failure) => { error.textContent = failure.message; });
    section.querySelector("#connect").addEventListener("click", connect);
    const form = section.querySelector("form");
    const submit = (event) => {
      event.preventDefault();
      const input = section.querySelector("#text");
      state.sendText(input.value); input.value = "";
    };
    form.addEventListener("submit", submit);
    context.lifecycle.defer("view-events", () => {
      section.querySelector("#connect").removeEventListener("click", connect);
      form.removeEventListener("submit", submit);
    });
    context.lifecycle.defer("view-state", unsubscribe);
    context.lifecycle.defer("view-slot", unregister);
  },
};
