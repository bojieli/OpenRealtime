// Generative UI: HTML the agent wrote, displayed to the person.
//
// There is no protocol here. An artifact arrives as the result of an ordinary
// function call, exactly like the contents of a file, and the only thing this
// page does differently is render it instead of printing it. That is worth
// being explicit about, because "generative UI" sounds like it ought to need a
// channel of its own and it does not: a model that can call a function can
// already produce one, against any Realtime server, including one that has
// never heard of this project.
//
// What the surface adds is the frame it goes in, and the frame is the security
// boundary. Sandboxed without allow-same-origin, so the artifact runs in an
// opaque origin and can read neither this page's DOM nor its storage; loaded
// from a route rather than srcdoc, so it carries its own content security
// policy instead of inheriting this page's; and that policy denies it the
// network entirely. HTML a model wrote can therefore draw anything and reach
// nothing.
//
// The one channel out is postMessage, which is deliberate. A person who clicks
// a button inside an artifact is a person saying something, and what they said
// goes back to the session as an ordinary user message - on the channel user
// messages already use, with the provenance a user message already has.

export class ArtifactPanel extends EventTarget {
  #host = null;
  #tabs = null;
  #frames = new Map();
  #current = null;

  constructor(tabsElement, hostElement) {
    super();
    this.#tabs = tabsElement;
    this.#host = hostElement;
    // A message from any frame this panel owns. The source check is what makes
    // it trustworthy: an artifact can only speak for itself, and a message
    // from anywhere else is discarded rather than attributed to the person.
    window.addEventListener("message", (message) => this.#relay(message));
  }

  get count() {
    return this.#frames.size;
  }

  // show renders or revises one artifact.
  //
  // A revision reloads the frame that is already there rather than adding a
  // second one, which is what makes an artifact self-updating: an agent that
  // recomputes a table mid-session changes what the person is looking at
  // instead of stacking a new copy underneath it and leaving them to notice.
  show(artifact) {
    let entry = this.#frames.get(artifact.id);
    if (!entry) {
      const frame = document.createElement("iframe");
      // No allow-same-origin. With it, the artifact would share this origin
      // and the sandbox would be decoration.
      frame.setAttribute("sandbox", "allow-scripts allow-forms");
      frame.className = "artifact-frame";
      frame.title = artifact.title ?? artifact.id;
      const tab = document.createElement("button");
      tab.className = "artifact-tab";
      tab.addEventListener("click", () => this.select(artifact.id));
      this.#tabs.append(tab);
      this.#host.append(frame);
      entry = { frame, tab };
      this.#frames.set(artifact.id, entry);
    }
    entry.tab.textContent = artifact.title ?? artifact.id;
    entry.tab.title = `${artifact.id} · version ${artifact.version}`;
    // The version is in the URL rather than in a reload call, so a browser
    // that would otherwise serve the previous body from cache cannot.
    entry.frame.src = `/artifacts/${encodeURIComponent(artifact.id)}?v=${artifact.version}`;
    this.select(artifact.id);
    this.dispatchEvent(new CustomEvent("shown", { detail: artifact }));
  }

  select(id) {
    this.#current = id;
    for (const [key, entry] of this.#frames) {
      const active = key === id;
      entry.frame.classList.toggle("active", active);
      entry.tab.classList.toggle("active", active);
    }
  }

  #relay(message) {
    for (const [id, entry] of this.#frames) {
      if (message.source !== entry.frame.contentWindow) continue;
      const text = typeof message.data === "string" ? message.data : message.data?.text;
      if (typeof text !== "string" || !text.trim()) return;
      this.dispatchEvent(new CustomEvent("interaction", {
        detail: { artifactId: id, text: text.trim().slice(0, 4000) },
      }));
      return;
    }
  }

  clear() {
    for (const [, entry] of this.#frames) {
      entry.frame.remove();
      entry.tab.remove();
    }
    this.#frames.clear();
    this.#current = null;
  }
}

// DownloadPanel renders same-origin attachment URLs returned by the local tool
// host. It never turns model output into an arbitrary href: ids are validated
// by the host, and only /downloads/{id} can be selected here.
export class DownloadPanel {
  #host;
  #entries = new Map();

  constructor(host) {
    this.#host = host;
  }

  show(download) {
    this.#host.querySelector(".hint")?.remove();
    let entry = this.#entries.get(download.id);
    if (!entry) {
      entry = document.createElement("a");
      entry.className = "download";
      this.#host.append(entry);
      this.#entries.set(download.id, entry);
    }
    entry.href = `/downloads/${encodeURIComponent(download.id)}?v=${download.version}`;
    entry.download = download.filename;
    entry.replaceChildren();
    const name = document.createElement("strong");
    name.textContent = download.filename;
    const meta = document.createElement("span");
    meta.textContent = `${download.media_type} · ${download.bytes.toLocaleString()} bytes · v${download.version}`;
    entry.append(name, meta);
  }

  clear() {
    this.#entries.clear();
    this.#host.innerHTML = '<p class="hint">No downloadable files published yet.</p>';
  }
}
