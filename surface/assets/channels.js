// The two spaces, as a thing you can look at.
//
// Everything on this page is one of twelve channels: six the world uses to
// reach the agent and six the agent uses to reach the world. They are all
// live at once, which is the reason this page exists - a voice agent that can
// also see three screens and click on one of them fails in ways that are
// obvious when you can watch every channel together and nearly impossible to
// reconstruct from an event log afterwards.
//
// So each channel is a card with three things: whether it is carrying
// anything, how much it has carried, and the last few things it carried. The
// count matters more than it looks. A channel at zero when you expected
// traffic is the single most common finding in an end-to-end run, and it is
// invisible in a transcript, which only shows what did arrive.

export const OBSERVATION = "observation";
export const ACTION = "action";

// The channel definitions. Order is display order, and it is roughly the order
// a person thinks about them rather than the order the protocol lists them.
export const CHANNELS = [
  { id: "obs.audio", side: OBSERVATION, label: "Audio", media: false,
    hint: "speech, continuously — the recogniser decides where turns end" },
  { id: "obs.text", side: OBSERVATION, label: "Text", media: false,
    hint: "typed by you, or sent back by an artifact you clicked" },
  { id: "obs.screen", side: OBSERVATION, label: "Screen", media: true, source: "screen",
    hint: "a display you share — the space computer use acts in" },
  { id: "obs.camera", side: OBSERVATION, label: "Camera", media: true, source: "camera",
    hint: "the physical world — observed, never acted on" },
  { id: "obs.browser", side: OBSERVATION, label: "Browser", media: true, source: "browser",
    hint: "a browser this surface drives — seen and clicked in one loop" },
  { id: "obs.tools", side: OBSERVATION, label: "Tool results", media: false,
    hint: "what this machine answered when the agent asked" },

  { id: "act.speech", side: ACTION, label: "Speech", media: false,
    hint: "audio out, and what it was saying while it played" },
  { id: "act.text", side: ACTION, label: "Text", media: false,
    hint: "written output — the half of a reply nobody hears" },
  { id: "act.computer", side: ACTION, label: "Computer use", media: false,
    hint: "clicks, keys, and scrolls, each against a declared source" },
  { id: "act.tools", side: ACTION, label: "Tool calls", media: false,
    hint: "what the agent asked this machine to do" },
  { id: "act.artifact", side: ACTION, label: "Artifacts", media: false,
    hint: "HTML it wrote for you to look at" },
  { id: "act.download", side: ACTION, label: "Downloads", media: false,
    hint: "generated files offered with a bounded attachment URL" },
];

const MAX_ENTRIES = 40;

export class Channels {
  #cards = new Map();
  #meters = new Map();
  #counts = new Map();

  // build lays out the cards and the meter strip.
  constructor(observationHost, actionHost, meterHost) {
    for (const definition of CHANNELS) {
      const host = definition.side === OBSERVATION ? observationHost : actionHost;
      const card = this.#buildCard(definition);
      host.append(card.element);
      this.#cards.set(definition.id, card);
      this.#counts.set(definition.id, 0);
      if (meterHost) {
        const meter = document.createElement("span");
        meter.className = `meter ${definition.side}`;
        meter.title = `${definition.label} — ${definition.hint}`;
        meter.dataset.channel = definition.id;
        meterHost.append(meter);
        this.#meters.set(definition.id, meter);
      }
    }
  }

  #buildCard(definition) {
    const element = document.createElement("section");
    element.className = "channel";
    element.dataset.channel = definition.id;

    const header = document.createElement("header");
    const dot = document.createElement("i");
    dot.className = "dot";
    const label = document.createElement("h3");
    label.textContent = definition.label;
    const count = document.createElement("span");
    count.className = "count";
    count.textContent = "0";
    header.append(dot, label, count);

    const hint = document.createElement("p");
    hint.className = "hint";
    hint.textContent = definition.hint;

    element.append(header, hint);

    let stage = null;
    if (definition.media) {
      stage = document.createElement("div");
      stage.className = "stage";
      const empty = document.createElement("p");
      empty.className = "stage-empty";
      empty.textContent = "not sending";
      stage.append(empty);
      element.append(stage);
    }

    const entries = document.createElement("ol");
    entries.className = "entries";
    element.append(entries);

    return { definition, element, dot, count, entries, stage, marks: [] };
  }

  // record appends one thing that happened on a channel.
  //
  // Returns the element, because some entries are revised in place rather than
  // appended to - a transcript that arrives in deltas is one entry that grows,
  // and rendering it as forty would make the channel unreadable at exactly the
  // moment it is most interesting.
  record(id, { title, body, kind = "", key = null } = {}) {
    const card = this.#cards.get(id);
    if (!card) return null;

    let entry = key ? card.entries.querySelector(`[data-key="${CSS.escape(key)}"]`) : null;
    const isNew = !entry;
    if (isNew) {
      entry = document.createElement("li");
      if (key) entry.dataset.key = key;
      const heading = document.createElement("span");
      heading.className = "entry-title";
      const text = document.createElement("span");
      text.className = "entry-body";
      entry.append(heading, text);
      card.entries.append(entry);
      while (card.entries.children.length > MAX_ENTRIES) card.entries.firstChild.remove();
      this.#counts.set(id, (this.#counts.get(id) ?? 0) + 1);
      card.count.textContent = String(this.#counts.get(id));
    }
    if (kind) entry.className = kind;
    if (title !== undefined) entry.querySelector(".entry-title").textContent = title;
    if (body !== undefined) entry.querySelector(".entry-body").textContent = body;
    card.entries.scrollTop = card.entries.scrollHeight;
    this.pulse(id);
    return entry;
  }

  // append adds to an entry that is arriving in pieces.
  append(id, key, delta, title) {
    const card = this.#cards.get(id);
    if (!card) return;
    let entry = card.entries.querySelector(`[data-key="${CSS.escape(key)}"]`);
    if (!entry) {
      this.record(id, { title, body: delta, key });
      return;
    }
    entry.querySelector(".entry-body").textContent += delta;
    card.entries.scrollTop = card.entries.scrollHeight;
    this.pulse(id);
  }

  // outcome marks an entry done, declined, or failed.
  outcome(id, key, state) {
    const card = this.#cards.get(id);
    const entry = card?.entries.querySelector(`[data-key="${CSS.escape(key)}"]`);
    if (entry) entry.dataset.outcome = state;
  }

  pulse(id) {
    const card = this.#cards.get(id);
    const meter = this.#meters.get(id);
    for (const element of [card?.dot, meter]) {
      if (!element) continue;
      element.classList.remove("pulse");
      // Reading offsetWidth restarts the animation; without it a channel
      // carrying steadily would light once and then look idle.
      void element.offsetWidth;
      element.classList.add("pulse");
    }
  }

  setState(id, state) {
    const card = this.#cards.get(id);
    if (card) card.element.dataset.state = state;
    const meter = this.#meters.get(id);
    if (meter) meter.dataset.state = state;
  }

  count(id) {
    return this.#counts.get(id) ?? 0;
  }

  // attach puts a source's live thumbnail in its card.
  attach(id, canvas) {
    const card = this.#cards.get(id);
    if (!card?.stage) return;
    card.stage.innerHTML = "";
    canvas.className = "thumb";
    card.stage.append(canvas);
    this.setState(id, "live");
  }

  detach(id) {
    const card = this.#cards.get(id);
    if (!card?.stage) return;
    card.stage.innerHTML = "";
    const empty = document.createElement("p");
    empty.className = "stage-empty";
    empty.textContent = "not sending";
    card.stage.append(empty);
    card.marks = [];
    this.setState(id, "");
  }

  // caption writes a line under a source's thumbnail — the page the browser is
  // on, the resolution a screen is being sent at.
  caption(id, text) {
    const card = this.#cards.get(id);
    if (!card?.stage) return;
    let caption = card.stage.querySelector(".stage-caption");
    if (!caption) {
      caption = document.createElement("p");
      caption.className = "stage-caption";
      card.stage.append(caption);
    }
    caption.textContent = text;
  }

  // mark draws where an action landed, on the source it named.
  //
  // This is the one piece of the page that could not be assembled from the
  // event log afterwards, and it is the reason computer use is worth watching
  // rather than reading. A click at (740, 313) is a number; a dot on the
  // screenshot is whether the agent hit the button.
  mark(sourceName, x, y, label) {
    const definition = CHANNELS.find((channel) => channel.source === sourceName);
    const card = definition ? this.#cards.get(definition.id) : null;
    const canvas = card?.stage?.querySelector("canvas");
    if (!canvas) return false;

    const dot = document.createElement("i");
    dot.className = "mark";
    dot.dataset.label = label ?? "";
    // Percentages rather than pixels, because the thumbnail is scaled to the
    // card and the coordinate is in the space the model was shown.
    dot.style.left = `${(x / canvas.width) * 100}%`;
    dot.style.top = `${(y / canvas.height) * 100}%`;
    card.stage.append(dot);
    card.marks.push(dot);
    // Marks fade rather than accumulating, but remain for a complete
    // multi-step interaction. A six-second lifetime let a slow tool turn erase
    // the click before the following screen explanation reached the person.
    // The hard cap below still prevents a long-running actor from covering the
    // screenshot in dots.
    setTimeout(() => dot.remove(), 30000);
    while (card.marks.length > 12) card.marks.shift()?.remove();
    return true;
  }

  reset() {
    for (const [id, card] of this.#cards) {
      card.entries.innerHTML = "";
      card.count.textContent = "0";
      this.#counts.set(id, 0);
      this.setState(id, "");
      if (card.stage) this.detach(id);
    }
  }

  // summary is what the run produced, per channel. It is what a person reads
  // at the end of a test to answer "did every channel actually carry
  // something", which is the question the whole page is for.
  summary() {
    return CHANNELS.map((definition) => ({
      id: definition.id, side: definition.side, label: definition.label,
      count: this.count(definition.id),
    }));
  }
}
