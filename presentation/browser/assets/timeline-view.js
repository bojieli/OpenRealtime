// The turn timeline: five lanes across time - what the recogniser heard,
// what the interaction policy chose, what the model was asked and said, what
// was synthesised and played, and what the background passes concluded - fed
// live by the server's `timeline` debug category and kept for replay.
//
// It draws what the server projected and interprets nothing. The vocabulary
// (lanes, kinds, spans) is the server's, defined once in the timeline package
// and written to the same shape in the operator's timeline log, so the page
// and the file agree about every event.
const LANES = Object.freeze([
  { id: "asr", label: "ASR", color: "#7cc4ff" },
  { id: "policy", label: "Policy", color: "#ffd166" },
  { id: "model", label: "LLM", color: "#9be29b" },
  { id: "tts", label: "TTS", color: "#f4a6c1" },
  { id: "background", label: "Background", color: "#c7b6ff" },
]);
const MAX_ROWS = 5000;
const MAX_LIST_ROWS = 240;
const WINDOW_SECONDS = Object.freeze([10, 30, 60, 120, 300]);
const LANE_HEIGHT = 34;
const AXIS_HEIGHT = 18;
const LABEL_WIDTH = 78;

function laneIndex(id) {
  return LANES.findIndex((lane) => lane.id === id);
}

function readRow(event) {
  if (event?.type !== "openrealtime.debug.event" || event.category !== "timeline") return null;
  const attributes = event.attributes ?? {};
  const lane = String(attributes.lane ?? "");
  if (laneIndex(lane) < 0) return null;
  const at = Number(event.timestamp_ms);
  if (!Number.isFinite(at)) return null;
  const payload = event.payload;
  let text = typeof payload?.text === "string" ? payload.text : "";
  if (!text && attributes.payloads_redacted === true) text = "(payload withheld)";
  return Object.freeze({
    at, lane,
    kind: String(attributes.kind ?? ""),
    phase: String(attributes.phase ?? "point"),
    span: String(attributes.span ?? ""),
    detail: String(attributes.detail ?? ""),
    text,
    duration: Number(event.duration_ms) || 0,
  });
}

function clock(at) {
  return new Date(at).toISOString().slice(11, 23);
}

function describe(row) {
  const phase = row.phase === "start" ? " start" : row.phase === "end" ? " end" : "";
  const parts = [row.kind + phase];
  if (row.detail) parts.push(row.detail);
  if (row.text) parts.push(JSON.stringify(row.text));
  return parts.join(" ");
}

export default {
  name: "openrealtime.presentation.client.timeline-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const protocol = context.services.get("presentation.client.protocol_events");
    const state = context.services.get("presentation.client.session_state");
    if (!slots || !protocol || !state) throw new Error("timeline view dependencies are unavailable");

    const section = document.createElement("section");
    section.dataset.view = "timeline";
    section.innerHTML = `<style>
      [data-view=timeline] { margin:0 auto 1rem; padding:0 1rem; font-family:system-ui,sans-serif; }
      [data-view=timeline] header { display:flex; flex-wrap:wrap; align-items:center; gap:.5rem .75rem; }
      [data-view=timeline] h2 { font-size:15px; margin:0 .5rem 0 0; }
      [data-view=timeline] header button, [data-view=timeline] header select { padding:.3rem .6rem; border-radius:8px; font-size:12px; }
      [data-view=timeline] header input[type=range] { flex:1; min-width:120px; }
      [data-view=timeline] #timeline-status { font:12px ui-monospace,monospace; color:#a7dcca; }
      [data-view=timeline] .timeline-canvas { position:relative; margin-top:.5rem; }
      [data-view=timeline] canvas { display:block; width:100%; border:1px solid #263147; border-radius:10px; background:#0f1521; }
      [data-view=timeline] #timeline-tip { position:absolute; display:none; max-width:min(60ch,90%); padding:.5rem .6rem; border:1px solid #354058;
        border-radius:8px; background:#151d2b; color:#e9edf5; font:12px ui-monospace,monospace; white-space:pre-wrap; overflow-wrap:anywhere; pointer-events:none; z-index:2; }
      [data-view=timeline] ol { list-style:none; margin:.5rem 0 0; padding:0; max-height:220px; overflow:auto; border:1px solid #263147; border-radius:10px;
        font:12px ui-monospace,monospace; }
      [data-view=timeline] li { display:grid; grid-template-columns:9ch 11ch 1fr; gap:.6ch; padding:.15rem .5rem; border-bottom:1px solid #1d2636; white-space:pre-wrap; overflow-wrap:anywhere; }
      [data-view=timeline] li[data-lane=asr] b { color:#7cc4ff; } [data-view=timeline] li[data-lane=policy] b { color:#ffd166; }
      [data-view=timeline] li[data-lane=model] b { color:#9be29b; } [data-view=timeline] li[data-lane=tts] b { color:#f4a6c1; }
      [data-view=timeline] li[data-lane=background] b { color:#c7b6ff; }
      [data-view=timeline] li time { color:#8ea0bb; }
    </style>
    <header><h2>Turn timeline</h2>
      <button id="timeline-live" aria-pressed="true">Live</button>
      <button id="timeline-replay">Replay from here</button>
      <input id="timeline-scrub" type="range" min="0" max="0" value="0" step="100" aria-label="Timeline position">
      <label>Window <select id="timeline-window" aria-label="Window seconds"></select></label>
      <button id="timeline-clear">Clear</button>
      <span id="timeline-status">waiting for events</span></header>
    <div class="timeline-canvas"><canvas id="timeline-canvas" height="${AXIS_HEIGHT + LANES.length * LANE_HEIGHT}" aria-label="Turn timeline"></canvas><div id="timeline-tip"></div></div>
    <ol id="timeline-rows" aria-label="Timeline events"></ol>`;
    const canvas = section.querySelector("#timeline-canvas");
    const tip = section.querySelector("#timeline-tip");
    const list = section.querySelector("#timeline-rows");
    const status = section.querySelector("#timeline-status");
    const liveButton = section.querySelector("#timeline-live");
    const replayButton = section.querySelector("#timeline-replay");
    const scrub = section.querySelector("#timeline-scrub");
    const windowSelect = section.querySelector("#timeline-window");
    for (const seconds of WINDOW_SECONDS) {
      const option = document.createElement("option");
      option.value = String(seconds);
      option.textContent = `${seconds}s`;
      option.selected = seconds === 30;
      windowSelect.append(option);
    }

    let rows = [];
    let sessionID = "";
    let live = true;
    let replaying = false;
    let replayStartedAt = 0;
    let replayFrom = 0;
    let windowEnd = 0;
    let clockOffset = 0;
    let listDirty = true;
    let disposed = false;
    let frame = 0;
    let hovered = null;

    const serverNow = () => Date.now() + clockOffset;
    const windowMS = () => Number(windowSelect.value) * 1000;

    const setLive = (next) => {
      live = next;
      liveButton.setAttribute("aria-pressed", String(live));
      liveButton.textContent = live ? "Live" : "Paused";
      if (live) replaying = false;
    };

    const syncScrubRange = () => {
      const now = serverNow();
      scrub.min = String(Math.floor(rows.length ? rows[0].at : now));
      scrub.max = String(Math.ceil(now + 500));
    };
    const ingest = (event) => {
      const row = readRow(event);
      if (!row) return;
      clockOffset = row.at - Date.now();
      rows.push(row);
      if (rows.length > MAX_ROWS) rows = rows.slice(rows.length - MAX_ROWS);
      listDirty = true;
      section.dataset.events = String(rows.length);
      syncScrubRange();
    };
    const unsubscribeProtocol = protocol.subscribe(ingest);
    const unsubscribeState = state.subscribe((snapshot) => {
      const id = snapshot?.session?.id ?? "";
      if (id && id !== sessionID) {
        sessionID = id;
        rows = [];
        listDirty = true;
        section.dataset.events = "0";
        setLive(true);
      }
    });

    // Spans: a start row and the end row that shares its lane and span id.
    const bars = () => {
      const open = new Map();
      const result = [];
      for (const row of rows) {
        if (!row.span || row.phase === "point") continue;
        const key = row.lane + " " + row.span + " " + row.kind;
        if (row.phase === "start") {
          open.set(key, row);
        } else if (row.phase === "end") {
          const start = open.get(key);
          if (start) {
            result.push({ start, end: row });
            open.delete(key);
          } else {
            result.push({ start: null, end: row });
          }
        }
      }
      for (const start of open.values()) result.push({ start, end: null });
      return result;
    };

    const marks = [];
    const draw = () => {
      const width = canvas.clientWidth || 800;
      const ratio = window.devicePixelRatio || 1;
      const height = AXIS_HEIGHT + LANES.length * LANE_HEIGHT;
      if (canvas.width !== Math.floor(width * ratio) || canvas.height !== Math.floor(height * ratio)) {
        canvas.width = Math.floor(width * ratio);
        canvas.height = Math.floor(height * ratio);
      }
      const context2d = canvas.getContext("2d");
      context2d.setTransform(ratio, 0, 0, ratio, 0, 0);
      context2d.clearRect(0, 0, width, height);
      const span = windowMS();
      const end = windowEnd;
      const begin = end - span;
      const plotLeft = LABEL_WIDTH;
      const plotWidth = Math.max(1, width - plotLeft - 8);
      const x = (at) => plotLeft + ((at - begin) / span) * plotWidth;
      marks.length = 0;

      context2d.font = "11px ui-monospace, monospace";
      context2d.textBaseline = "middle";
      // Axis: a tick every few seconds, labelled with the server clock.
      const step = span >= 120_000 ? 30_000 : span >= 60_000 ? 10_000 : span >= 30_000 ? 5_000 : 1_000;
      context2d.strokeStyle = "#1d2636";
      context2d.fillStyle = "#8ea0bb";
      for (let tick = Math.ceil(begin / step) * step; tick <= end; tick += step) {
        const tx = x(tick);
        context2d.beginPath(); context2d.moveTo(tx, AXIS_HEIGHT); context2d.lineTo(tx, height); context2d.stroke();
        context2d.fillText(clock(tick).slice(0, 8), tx + 3, AXIS_HEIGHT / 2);
      }
      LANES.forEach((lane, index) => {
        const top = AXIS_HEIGHT + index * LANE_HEIGHT;
        context2d.fillStyle = index % 2 ? "#111826" : "#0f1521";
        context2d.fillRect(0, top, width, LANE_HEIGHT);
        context2d.fillStyle = lane.color;
        context2d.fillText(lane.label, 8, top + LANE_HEIGHT / 2);
      });
      // Bars first, so points draw over them.
      for (const bar of bars()) {
        const row = bar.start ?? bar.end;
        const index = laneIndex(row.lane);
        const from = bar.start ? bar.start.at : bar.end.at - Math.max(bar.end.duration, 1);
        const to = bar.end ? bar.end.at : end;
        if (to < begin || from > end) continue;
        const top = AXIS_HEIGHT + index * LANE_HEIGHT + 6;
        const left = Math.max(x(from), plotLeft);
        const right = Math.min(x(to), plotLeft + plotWidth);
        context2d.fillStyle = LANES[index].color + (bar.end ? "55" : "33");
        context2d.fillRect(left, top, Math.max(right - left, 2), LANE_HEIGHT - 12);
        marks.push({ x0: left, x1: Math.max(right, left + 2), lane: index, row, bar });
      }
      const labelEnd = LANES.map(() => -Infinity);
      for (const row of rows) {
        if (row.at < begin || row.at > end) continue;
        const index = laneIndex(row.lane);
        const cx = x(row.at);
        const cy = AXIS_HEIGHT + index * LANE_HEIGHT + LANE_HEIGHT / 2;
        context2d.fillStyle = LANES[index].color;
        context2d.beginPath();
        if (row.phase === "point") context2d.arc(cx, cy, 3.5, 0, Math.PI * 2);
        else context2d.rect(cx - 1.5, cy - 8, 3, 16);
        context2d.fill();
        marks.push({ x0: cx - 4, x1: cx + 4, lane: index, row });
        const label = row.text || row.kind;
        if (cx > labelEnd[index] + 4 && row.phase !== "end") {
          const shown = label.length > 42 ? label.slice(0, 41) + "…" : label;
          context2d.fillStyle = "#e9edf5";
          context2d.fillText(shown, cx + 6, cy);
          labelEnd[index] = cx + 6 + context2d.measureText(shown).width;
        }
      }
      // Live edge.
      const nowX = x(serverNow());
      if (nowX >= plotLeft && nowX <= plotLeft + plotWidth) {
        context2d.strokeStyle = "#58c5a5";
        context2d.beginPath(); context2d.moveTo(nowX, AXIS_HEIGHT); context2d.lineTo(nowX, height); context2d.stroke();
      }
    };

    const renderList = () => {
      if (!listDirty) return;
      listDirty = false;
      const end = windowEnd;
      const begin = end - windowMS();
      const visible = rows.filter((row) => row.at >= begin && row.at <= end);
      const shown = visible.slice(-MAX_LIST_ROWS);
      const atBottom = list.scrollHeight - list.scrollTop - list.clientHeight < 24;
      list.replaceChildren(...shown.map((row) => {
        const item = document.createElement("li");
        item.dataset.lane = row.lane;
        item.dataset.kind = row.kind;
        item.dataset.phase = row.phase;
        const time = document.createElement("time");
        time.textContent = clock(row.at);
        const lane = document.createElement("b");
        lane.textContent = LANES[laneIndex(row.lane)].label;
        const body = document.createElement("span");
        body.textContent = describe(row);
        item.append(time, lane, body);
        return item;
      }));
      if (live || atBottom) list.scrollTop = list.scrollHeight;
      status.textContent = `${rows.length} events · ${visible.length} in window · ${live ? "live" : replaying ? "replaying" : "paused"}`;
    };

    const tick = () => {
      if (disposed) return;
      frame = requestAnimationFrame(tick);
      const now = serverNow();
      if (live) {
        windowEnd = now + 500;
        listDirty = true;
      } else if (replaying) {
        windowEnd = replayFrom + (performance.now() - replayStartedAt);
        listDirty = true;
        if (windowEnd >= now) setLive(true);
      }
      syncScrubRange();
      if (live || replaying) scrub.value = String(Math.floor(windowEnd));
      draw();
      renderList();
    };

    liveButton.addEventListener("click", () => {
      setLive(!live);
      listDirty = true;
    });
    scrub.addEventListener("input", () => {
      setLive(false);
      replaying = false;
      windowEnd = Number(scrub.value);
      listDirty = true;
    });
    replayButton.addEventListener("click", () => {
      setLive(false);
      replaying = true;
      replayFrom = Number(scrub.value) || windowEnd;
      replayStartedAt = performance.now();
      listDirty = true;
    });
    windowSelect.addEventListener("change", () => { listDirty = true; });
    section.querySelector("#timeline-clear").addEventListener("click", () => {
      rows = [];
      listDirty = true;
      section.dataset.events = "0";
    });
    const onMove = (pointer) => {
      const bounds = canvas.getBoundingClientRect();
      const px = pointer.clientX - bounds.left;
      const py = pointer.clientY - bounds.top;
      const lane = Math.floor((py - AXIS_HEIGHT) / LANE_HEIGHT);
      let best = null;
      for (const mark of marks) {
        if (mark.lane !== lane || px < mark.x0 - 2 || px > mark.x1 + 2) continue;
        if (!best || (mark.x1 - mark.x0) < (best.x1 - best.x0)) best = mark;
      }
      if (best === hovered) return;
      hovered = best;
      if (!best) { tip.style.display = "none"; return; }
      const row = best.row;
      const lines = [`${clock(row.at)} ${LANES[best.lane].label} · ${describe(row)}`];
      if (best.bar?.start && best.bar?.end) {
        lines.push(`${best.bar.end.at - best.bar.start.at} ms from start to end`);
      }
      tip.textContent = lines.join("\n");
      tip.style.display = "block";
      tip.style.left = `${Math.min(px + 12, Math.max(0, bounds.width - 320))}px`;
      tip.style.top = `${Math.max(0, py - 10)}px`;
    };
    canvas.addEventListener("mousemove", onMove);
    canvas.addEventListener("mouseleave", () => { hovered = null; tip.style.display = "none"; });

    const unregister = slots.register("session.timeline", section, 10);
    frame = requestAnimationFrame(tick);
    section.dataset.ready = "true";
    context.lifecycle.defer("timeline-view", () => {
      disposed = true;
      cancelAnimationFrame(frame);
      unsubscribeProtocol();
      unsubscribeState();
      canvas.removeEventListener("mousemove", onMove);
      unregister();
    });
  },
};
