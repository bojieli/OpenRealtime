// A millisecond-resolution view of the optional server debug stream.
//
// Raw events answer "what was sent". This view answers the harder debugging
// questions: what subsystem was active, which events belong to one operation,
// and where the time went. It intentionally consumes only
// openrealtime.debug.event, so a normal protocol transcript stays a normal
// protocol transcript.

const MAX_ENTRIES = 2000;

function formatTime(timestamp, origin) {
  const relative = Math.max(0, timestamp - origin);
  return `+${relative.toFixed(0).padStart(5, " ")} ms`;
}

function formatDuration(duration) {
  if (!Number.isFinite(duration) || duration <= 0) return "—";
  return duration < 10 ? `${duration.toFixed(1)} ms` : `${Math.round(duration)} ms`;
}

export class DebugTimeline {
  #host;
  #summary;
  #filter;
  #entries = [];
  #origin = null;

  constructor(host, summary, filter) {
    this.#host = host;
    this.#summary = summary;
    this.#filter = filter;
    this.#filter?.addEventListener("input", () => this.render());
  }

  add(event) {
    if (!Number.isFinite(event.timestamp_ms)) return;
    if (this.#origin === null) this.#origin = event.timestamp_ms;
    this.#entries.push(event);
    while (this.#entries.length > MAX_ENTRIES) this.#entries.shift();
    this.#renderEntry(event);
    this.#renderSummary();
    this.#host.scrollTop = this.#host.scrollHeight;
  }

  clear() {
    this.#entries = [];
    this.#origin = null;
    this.#host.innerHTML = "";
    this.#summary.textContent = "Waiting for debug events.";
  }

  render() {
    this.#host.innerHTML = "";
    for (const event of this.#entries) this.#renderEntry(event);
    this.#renderSummary();
  }

  #matches(event) {
    const query = this.#filter?.value?.trim().toLowerCase();
    if (!query) return true;
    return [event.category, event.name, event.phase, event.correlation_id, event.message]
      .some((value) => String(value ?? "").toLowerCase().includes(query));
  }

  #renderEntry(event) {
    if (!this.#matches(event)) return;
    const row = document.createElement("details");
    row.className = "timeline-row";
    row.dataset.category = event.category ?? "unknown";
    row.dataset.phase = event.phase ?? "instant";

    const heading = document.createElement("summary");
    const at = document.createElement("time");
    at.textContent = formatTime(event.timestamp_ms, this.#origin ?? event.timestamp_ms);
    at.dateTime = new Date(event.timestamp_ms).toISOString();
    at.title = `${at.dateTime} · server timestamp`;
    const category = document.createElement("span");
    category.className = "timeline-category";
    category.textContent = event.category ?? "unknown";
    const name = document.createElement("span");
    name.className = "timeline-name";
    name.textContent = event.name ?? "event";
    const duration = document.createElement("span");
    duration.className = "timeline-duration";
    duration.textContent = formatDuration(Number(event.duration_ms));
    heading.append(at, category, name, duration);

    const bar = document.createElement("i");
    bar.className = "timeline-bar";
    const width = Math.min(100, Math.max(1, Math.log10(Number(event.duration_ms ?? 0) + 1) * 30));
    bar.style.width = `${width}%`;

    const detail = document.createElement("pre");
    detail.textContent = JSON.stringify({
      phase: event.phase,
      correlation_id: event.correlation_id,
      message: event.message,
      attributes: event.attributes,
      payload: event.payload,
    }, null, 2);
    row.append(heading, bar, detail);
    this.#host.append(row);
  }

  #renderSummary() {
    const visible = this.#entries.filter((event) => this.#matches(event));
    const durations = visible.map((event) => Number(event.duration_ms)).filter((value) => value > 0)
      .sort((a, b) => a - b);
    const categories = new Set(visible.map((event) => event.category));
    const percentile = (fraction) => durations.length
      ? durations[Math.min(durations.length - 1, Math.floor(durations.length * fraction))]
      : 0;
    this.#summary.textContent =
      `${visible.length} events · ${categories.size} subsystems · ` +
      `timed p50 ${formatDuration(percentile(.5))} · p95 ${formatDuration(percentile(.95))}`;
  }
}
