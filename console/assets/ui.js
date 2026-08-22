// Rendering. Nothing here decides anything about the session.

const $ = (id) => document.getElementById(id);

export const elements = {
  state: $("state"), dot: $("dot"), log: $("log"), events: $("events"),
  stats: $("stats"), level: $("level"), mediaState: $("media-state"),
  thumbs: $("thumbs"), toolList: $("tool-list"), toolsRoot: $("tools-root"),
  negotiated: $("negotiated"), sessionJson: $("session-json"),
  transport: $("transport"), connect: $("connect"), disconnect: $("disconnect"),
  mic: $("mic"), screen: $("screen"), camera: $("camera"), endTurn: $("endturn"),
  compose: $("compose"), typed: $("typed"),
  confirm: $("confirm"), confirmName: $("confirm-name"),
  confirmArguments: $("confirm-arguments"), confirmRequirement: $("confirm-requirement"),
  filterAudio: $("filter-audio"), clearEvents: $("clear-events"), saveEvents: $("save-events"),
  applySession: $("apply-session"), resetSession: $("reset-session"),
};

const turns = new Map();

export function setState(text, kind = "") {
  elements.state.textContent = text;
  elements.dot.dataset.state = kind;
}

export function setMediaState(text) {
  elements.mediaState.textContent = text;
}

export function clearLog() {
  elements.log.innerHTML = "";
  turns.clear();
}

function turnElement(id, who, kind) {
  let element = turns.get(id);
  if (!element) {
    element = document.createElement("div");
    element.className = `turn ${kind}`;
    element.innerHTML = '<div class="who"></div><div class="text"></div>';
    element.querySelector(".who").textContent = who;
    elements.log.append(element);
    turns.set(id, element);
  }
  const atBottom = elements.log.scrollHeight - elements.log.scrollTop - elements.log.clientHeight < 80;
  if (atBottom) queueMicrotask(() => { elements.log.scrollTop = elements.log.scrollHeight; });
  return element;
}

export function setTurn(id, who, text, kind = who) {
  turnElement(id, who, kind).querySelector(".text").textContent = text;
}

export function appendTurn(id, who, delta, kind = who) {
  const element = turnElement(id, who, kind);
  const text = element.querySelector(".text");
  text.textContent += delta;
}

export function setTurnOutcome(id, outcome) {
  turns.get(id)?.setAttribute("data-outcome", outcome);
}

export function notice(text) {
  const id = `notice-${Date.now()}-${Math.random()}`;
  setTurn(id, "console", text, "notice");
}

// --- the event log ----------------------------------------------------------

const AUDIO_EVENTS = new Set([
  "input_audio_buffer.append",
  "response.output_audio.delta",
  "openrealtime.input_video_frame.append",
]);

const recorded = [];

export function recordEvent(direction, event) {
  const at = performance.now();
  recorded.push({ direction, at, event });

  if (elements.filterAudio.checked && AUDIO_EVENTS.has(event.type)) return;

  const row = document.createElement("div");
  row.className = "event";
  if (event.type === "error") row.classList.add("error");
  row.dataset.direction = direction;
  const name = document.createElement("span");
  name.className = "name";
  name.textContent = `${direction === "out" ? "→" : "←"} ${event.type ?? "(no type)"}`;
  const time = document.createElement("span");
  time.className = "at";
  time.textContent = `${(at / 1000).toFixed(2)}s`;
  const body = document.createElement("pre");
  // A frame or an audio delta is tens of kilobytes of base64 that tells a
  // reader nothing, so it is elided rather than made unreadable.
  body.textContent = JSON.stringify(event, (key, value) =>
    (key === "frame" || key === "delta" || key === "audio") && typeof value === "string" && value.length > 64
      ? `<${value.length} characters elided>`
      : value, 2);
  row.append(name, time, body);
  row.addEventListener("click", () => row.classList.toggle("open"));
  elements.events.append(row);

  while (elements.events.childElementCount > 800) elements.events.firstElementChild.remove();
  const atBottom = elements.events.scrollHeight - elements.events.scrollTop - elements.events.clientHeight < 120;
  if (atBottom) elements.events.scrollTop = elements.events.scrollHeight;
}

export function clearEvents() {
  elements.events.innerHTML = "";
  recorded.length = 0;
}

// saveTranscript writes the whole session out. A bug report that carries the
// event sequence is a bug report somebody can act on.
export function saveTranscript() {
  const blob = new Blob([JSON.stringify(recorded, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `openrealtime-session-${new Date().toISOString().replace(/[:.]/g, "-")}.json`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

// --- stats ------------------------------------------------------------------

const stats = new Map();

export function setStat(label, value) {
  stats.set(label, value);
  elements.stats.innerHTML = "";
  for (const [key, entry] of stats) {
    const term = document.createElement("dt");
    term.textContent = key;
    const description = document.createElement("dd");
    description.textContent = entry;
    elements.stats.append(term, description);
  }
}

export function resetStats() {
  stats.clear();
  elements.stats.innerHTML = "";
}

// --- tools ------------------------------------------------------------------

export function renderTools(root, tools) {
  elements.toolsRoot.textContent = tools.length
    ? `Running in ${root}. Every path a tool is given resolves inside it, after symlinks.`
    : "This console was started with no tools. Restart it with -tools to give the session something to call.";
  elements.toolList.innerHTML = "";
  for (const tool of tools) {
    const item = document.createElement("li");
    const name = document.createElement("span");
    name.className = "name";
    name.textContent = tool.name;
    const confirm = document.createElement("span");
    confirm.className = "confirm";
    confirm.dataset.confirm = tool.confirm;
    confirm.textContent = tool.confirm === "never" ? "no confirmation" : tool.confirm;
    const description = document.createElement("div");
    description.textContent = tool.description;
    item.append(confirm, name, description);
    elements.toolList.append(item);
  }
}

// askConfirmation resolves true only if a person pressed the affirmative
// button. Dismissing the dialog any other way is a refusal.
export function askConfirmation({ name, arguments: args, confirm }) {
  elements.confirmName.textContent = name;
  elements.confirmRequirement.textContent = confirm;
  let pretty = args;
  try {
    pretty = JSON.stringify(typeof args === "string" ? JSON.parse(args) : args, null, 2);
  } catch {
    pretty = String(args);
  }
  elements.confirmArguments.textContent = pretty;
  elements.confirm.showModal();
  return new Promise((resolve) => {
    elements.confirm.addEventListener("close", () => {
      resolve(elements.confirm.returnValue === "allow");
    }, { once: true });
  });
}

export function showThumbnail(name, canvas) {
  let holder = elements.thumbs.querySelector(`[data-source="${name}"]`);
  if (!holder) {
    holder = document.createElement("div");
    holder.dataset.source = name;
    holder.append(canvas);
    elements.thumbs.append(holder);
  }
}

export function hideThumbnail(name) {
  elements.thumbs.querySelector(`[data-source="${name}"]`)?.remove();
}

export function setPressed(button, pressed) {
  button.setAttribute("aria-pressed", String(pressed));
}
