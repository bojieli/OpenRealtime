// Conference room presentation. Capture, protocol and session state remain separate providers.
const ROOM_PRESETS = /*ROOM_PRESETS*/[];

export default {
  name: "openrealtime.presentation.client.video-controls",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const video = context.services.get("presentation.client.video");
    const media = context.services.get("presentation.client.media");
    const state = context.services.get("presentation.client.session_state");
    const connection = context.services.get("presentation.client.connection");
    if (!slots || !video || !media || !state || !connection) throw new Error("room dependencies are unavailable");
    const section = document.createElement("section");
    section.dataset.view = "video";
    section.innerHTML = `<style>
      .room-heading { display:flex; align-items:center; gap:14px; margin:8px 0 24px; }
      .room-mark { background:#78d3b3; color:#102a25; width:44px; height:44px; display:grid; place-items:center; border-radius:14px; font-size:26px; }
      .room-heading h1 { font-size:23px; margin:0 0 4px; letter-spacing:-.6px; } .room-heading p { margin:0; color:#94a3b8; font-size:13px; }
      .room-heading .room-badge { margin-left:auto; border:1px solid #354058; padding:8px 12px; border-radius:24px; font-size:12px; }
      .room-scenario { margin-bottom:20px; color:#a7b7cf; line-height:1.6; } .room-scenario summary { cursor:pointer; } .room-scenario select { max-width:70%; }
      .room-grid { display:grid; grid-template-columns:1fr 1fr; gap:16px; }
      .participant { position:relative; overflow:hidden; min-height:310px; background:radial-gradient(ellipse at top,#293951,#182031 70%); border:1px solid #354058; border-radius:20px; display:grid; place-items:center; }
      .participant video { width:100%; height:100%; position:absolute; object-fit:cover; } .participant video[hidden] { display:none; }
      .participant.screen { margin-top:16px; min-height:360px; } .participant.screen video { object-fit:contain; background:#090d14; }
      .participant[hidden] { display:none; }
      .participant-label { position:absolute; bottom:16px; left:16px; padding:7px 12px; border-radius:8px; background:#0a111ccc; font-size:13px; }
      .participant-avatar { text-align:center; color:#a5b4c8; font-size:13px; }
      .participant-avatar b { display:grid; place-items:center; width:100px; height:100px; border-radius:50%; background:#344962; color:#eff5ff; font-size:28px; margin:0 auto 18px; }
      .agent-avatar b { background:#275a51; color:#99efd2; } .agent-avatar[hidden] { display:none; }
      .room-controls { display:flex; justify-content:center; flex-wrap:wrap; gap:10px; padding:20px 0 10px; }
      .room-controls button { font-size:13px; min-height:48px; } #record[aria-pressed=true] { background:#763f4c; border-color:#ff879e; }
      .room-help { color:#94a3b8; font-size:12px; line-height:1.6; text-align:center; }
      #video-state { display:block; color:#ffcca3; margin:12px 0; font-size:13px; overflow-wrap:anywhere; }
      #recording-download { display:block; color:#9fe8cf; text-align:center; padding:12px; }
      #recording-download[hidden] { display:none; }
      .room-controls label { cursor:pointer; border:1px solid #354058; background:#202a3c; border-radius:12px; padding:14px; font-size:13px; }
      @media(max-width:600px) { .room-grid { grid-template-columns:1fr; } .participant { min-height:240px; } .room-heading .room-badge { display:none; } }
    </style>
    <header class="room-heading"><span class="room-mark" aria-hidden="true">◉</span><div><h1>OpenRealtime</h1><p>Your realtime conversation room</p></div><span class="room-badge">Private session</span></header>
    <details class="room-scenario"><summary>Practice an interaction scenario</summary>
    <p><label for="room-scenario">Scenario </label><select id="room-scenario"><option value="">Free conversation</option></select> <button id="apply-scenario">Apply scenario</button></p>
    <p id="scenario-note"></p><ol id="scenario-script"></ol><output id="scenario-tool" role="status"></output></details>
    <div class="room-grid">
      <div class="participant"><div class="participant-avatar"><b>You</b><span id="camera-placeholder">Camera off</span></div><video id="camera-preview" muted autoplay playsinline hidden></video><span class="participant-label">You · <span id="mic-label">microphone ready</span></span></div>
      <div class="participant"><div class="participant-avatar agent-avatar" id="agent-avatar"><b>◉</b><span>Audio participant</span></div><span class="participant-label">OpenRealtime · <span id="agent-label">ready to join</span></span></div>
    </div>
    <div class="participant screen" id="screen-tile" hidden><video id="screen-preview" muted autoplay playsinline></video><span class="participant-label">Your shared screen</span></div>
    <nav class="room-controls" aria-label="Room controls">
      <button id="microphone" aria-pressed="false">Mute microphone</button>
      <button id="speaker" aria-pressed="false">Mute agent audio</button>
      <button id="video-camera" aria-pressed="false">Start camera</button>
      <button id="video-screen" aria-pressed="false">Share screen</button>
      <button id="agent-visual" aria-pressed="false">Hide agent tile</button>
      <button id="image-button">Add image</button><input id="image-input" type="file" accept="image/png,image/jpeg,image/webp" hidden>
      <button id="record" aria-pressed="false">Record room</button>
      <button id="video-stop">Stop all video</button>
    </nav>
    <output id="video-state" role="status" aria-live="polite"></output>
    <a id="recording-download" hidden>Download recording</a>
    <p class="room-help">Camera and screen sharing are independent. Recording stays on this device and includes the visible participants, shared screen, and unmuted audio.</p>`;
    const $ = (selector) => section.querySelector(selector);
    const output = $("#video-state");
    const events = [];
    const on = (selector, event, handler) => {
      const node = $(selector);
      const wrapped = async (...args) => { try { await handler(...args); } catch (error) { output.textContent = error.message; } };
      node.addEventListener(event, wrapped); events.push(() => node.removeEventListener(event, wrapped));
    };
    for (const [index, preset] of ROOM_PRESETS.entries()) {
      const option = document.createElement("option"); option.value = String(index); option.textContent = `${index + 1}. ${preset.name}`; $("#room-scenario").append(option);
    }
    const answeredTools = new Set();
    let selectedPreset;
    on("#room-scenario", "change", () => {
      const preset = ROOM_PRESETS[$("#room-scenario").value];
      $("#scenario-note").textContent = preset?.note ?? "Talk freely with the agent.";
      $("#scenario-script").replaceChildren(...(preset?.script ?? []).map((line) => {
        const item = document.createElement("li"); item.textContent = `${line.Speaker}: ${line.Text}`; return item;
      }));
    });
    on("#apply-scenario", "click", () => {
      if (!connected) throw new Error("Join the room before applying a scenario.");
      selectedPreset = ROOM_PRESETS[$("#room-scenario").value];
      state.updateSession({type:"realtime", instructions:selectedPreset?.instructions ?? "You are a realtime assistant. Follow the user's instructions about when to speak. Answer briefly.", tools:selectedPreset?.tools ?? []});
      output.textContent = selectedPreset ? `Ready to practice: ${selectedPreset.name}. Follow the script at your own pace.` : "Free conversation ready.";
    });
    let disposed = false, hiddenAgent = false, connected = false;
    const pending = new Set();
    let recorder, recordingURL, recordingCleanup, recordingTimer;
    let recordingStarting = false;
    const stopRecording = () => { if (recorder?.state === "recording") recorder.stop(); };
    const preview = (selector, stream) => {
      const node = $(selector);
      if (node.srcObject !== (stream ?? null)) { node.srcObject = stream ?? null; if (stream) node.play().catch(() => {}); }
      node.hidden = !stream;
    };
    const render = () => {
      if (disposed) return;
      const snapshot = video.snapshot(), audio = media.snapshot(), streams = media.streams();
      for (const source of ["camera", "screen"]) {
        const active = snapshot.active.includes(source);
        const button = $(`#video-${source}`);
        button.disabled = !snapshot.enabled || pending.has(source);
        button.setAttribute("aria-pressed", String(active));
        button.textContent = source === "camera" ? (active ? "Stop camera" : "Start camera") : (active ? "Stop sharing" : "Share screen");
        preview(`#${source}-preview`, streams[source]);
        $(`#${source}-preview`).dataset.frames = String(snapshot.media?.[source]?.frames ?? 0);
      }
      $("#screen-tile").hidden = !streams.screen;
      $("#video-stop").disabled = snapshot.active.length === 0;
      $("#microphone").disabled = !connected;
      $("#microphone").textContent = audio.microphone_muted ? "Unmute microphone" : "Mute microphone";
      $("#microphone").setAttribute("aria-pressed", String(audio.microphone_muted));
      $("#mic-label").textContent = !connected ? "not connected" : audio.microphone_muted ? "muted" : "microphone live";
      $("#speaker").textContent = audio.speaker_muted ? "Unmute agent audio" : "Mute agent audio";
      $("#speaker").setAttribute("aria-pressed", String(audio.speaker_muted));
      $("#agent-label").textContent = !connected ? "ready to join" : audio.speaker_muted ? "audio muted" : "connected";
      $("#image-button").disabled = !connected;
      $("#record").disabled = recordingStarting || (!connected && recorder?.state !== "recording");
      if (snapshot.diagnostic) output.textContent = snapshot.diagnostic;
    };
    for (const source of ["camera", "screen"]) on(`#video-${source}`, "click", async () => {
      if (pending.has(source)) return;
      pending.add(source); render();
      try { if (video.snapshot().active.includes(source)) video.stop(source); else await video.start(source); }
      finally { pending.delete(source); render(); }
    });
    on("#video-stop", "click", () => video.stopAll());
    on("#microphone", "click", async () => { await media.setMicrophoneMuted(!media.snapshot().microphone_muted); render(); });
    on("#speaker", "click", () => { media.setSpeakerMuted(!media.snapshot().speaker_muted); render(); });
    on("#agent-visual", "click", () => {
      hiddenAgent = !hiddenAgent; $("#agent-avatar").hidden = hiddenAgent;
      $("#agent-visual").setAttribute("aria-pressed", String(hiddenAgent));
      $("#agent-visual").textContent = hiddenAgent ? "Show agent tile" : "Hide agent tile";
    });
    on("#image-button", "click", () => $("#image-input").click());
    on("#image-input", "change", async () => {
      const file = $("#image-input").files[0]; $("#image-input").value = "";
      if (!file) return;
      if (!["image/jpeg", "image/png", "image/webp"].includes(file.type) || file.size > 10 * 1024 * 1024) throw new Error("Choose a JPEG, PNG, or WebP image smaller than 10 MB.");
      const bitmap = await createImageBitmap(file);
      let image;
      try {
        const scale = Math.min(1, 1280 / Math.max(bitmap.width, bitmap.height));
        const canvas = document.createElement("canvas"); canvas.width = Math.max(1, Math.round(bitmap.width * scale)); canvas.height = Math.max(1, Math.round(bitmap.height * scale));
        canvas.getContext("2d").drawImage(bitmap, 0, 0, canvas.width, canvas.height);
        image = canvas.toDataURL("image/jpeg", .8);
      } finally { bitmap.close(); }
      if (disposed || !connected) throw new Error("Join the room before sending an image.");
      connection.send({ type: "conversation.item.create", item: { type: "message", role: "user", content: [{ type: "input_image", image_url: image }] } });
      connection.send({ type: "response.create" });
      output.textContent = `Image sent: ${file.name}`;
    });
    on("#record", "click", async () => {
      if (recorder?.state === "recording") { stopRecording(); return; }
      if (!globalThis.MediaRecorder) throw new Error("Recording is unavailable in this browser.");
      if (recordingStarting) return;
      recordingStarting = true; render();
      try {
      // A canvas stream keeps the recording topology stable when sources are toggled.
      const canvas = document.createElement("canvas"); canvas.width = 1280; canvas.height = 720;
      const graphics = canvas.getContext("2d");
      const audio = new AudioContext();
      try { await audio.resume(); } catch(error) { await audio.close(); throw error; }
      if (disposed || !connected) { await audio.close(); throw new Error("Recording was cancelled before it started."); }
      const destination = audio.createMediaStreamDestination();
      const inputs = new Map();
      const draw = () => {
        const streams = media.streams(), snapshot = media.snapshot();
        for (const [name, stream] of [["microphone", streams.microphone], ["remote", streams.remote]]) {
          const existing = inputs.get(name);
          if (existing?.stream !== stream) {
            existing?.source.disconnect(); existing?.gain.disconnect(); inputs.delete(name);
            if (stream?.getAudioTracks().length) { const source = audio.createMediaStreamSource(stream), gain = audio.createGain(); source.connect(gain).connect(destination); inputs.set(name, {stream, source, gain}); }
          }
          const input = inputs.get(name); if (input) input.gain.gain.value = (name === "remote" ? snapshot.speaker_muted : snapshot.microphone_muted) ? 0 : 1;
        }
        graphics.fillStyle = "#0c1019"; graphics.fillRect(0, 0, 1280, 720);
        graphics.fillStyle = "#e9edf5"; graphics.font = "24px system-ui"; graphics.fillText("OpenRealtime · Conversation room", 28, 42);
        const paint = (node, x, y, w, h, label) => {
          graphics.fillStyle = "#202b3e"; graphics.fillRect(x, y, w, h);
          if (node?.srcObject && node.readyState >= 2) {
            const scale = Math.min(w / node.videoWidth, h / node.videoHeight);
            const dw = node.videoWidth * scale, dh = node.videoHeight * scale;
            graphics.drawImage(node, x + (w - dw) / 2, y + (h - dh) / 2, dw, dh);
          }
          graphics.fillStyle = "#e9edf5"; graphics.font = "20px system-ui"; graphics.fillText(label, x + 16, y + h - 20);
        };
        if (streams.screen) { paint($("#screen-preview"), 24, 70, 920, 620, "Shared screen"); paint($("#camera-preview"), 964, 70, 292, 280, "You"); paint(null, 964, 370, 292, 320, hiddenAgent ? "Agent tile hidden" : "OpenRealtime · Audio"); }
        else { paint($("#camera-preview"), 24, 70, 606, 620, "You"); paint(null, 650, 70, 606, 620, hiddenAgent ? "Agent tile hidden" : "OpenRealtime · Audio"); }
      };
      const stream = canvas.captureStream(24);
      for (const track of destination.stream.getAudioTracks()) stream.addTrack(track);
      const mimeType = ["video/webm;codecs=vp9,opus", "video/webm;codecs=vp8,opus", "video/mp4"].find((type) => MediaRecorder.isTypeSupported(type));
      const chunks = []; let bytes = 0;
      const cleanup = () => { clearInterval(recordingTimer); stream.getTracks().forEach((track) => track.stop()); inputs.forEach(({source, gain}) => { source.disconnect(); gain.disconnect(); }); audio.close().catch(() => {}); recordingCleanup = undefined; };
      recordingCleanup = cleanup;
      try {
        recorder = new MediaRecorder(stream, mimeType ? { mimeType } : {});
        recorder.ondataavailable = ({data}) => { if (data.size) { chunks.push(data); bytes += data.size; if (bytes > 256 * 1024 * 1024) { output.textContent = "Recording reached 256 MB and was stopped. Download it before recording again."; stopRecording(); } } };
        recorder.onerror = () => { output.textContent = "Recording failed; any captured data will be available to download."; stopRecording(); cleanup(); };
        recorder.onstop = () => {
          cleanup();
          if (recordingURL) URL.revokeObjectURL(recordingURL);
          recordingURL = URL.createObjectURL(new Blob(chunks, { type: recorder.mimeType }));
          const link = $("#recording-download"); link.href = recordingURL; link.download = `openrealtime-${new Date().toISOString().replaceAll(":", "-")}.${recorder.mimeType.includes("mp4") ? "mp4" : "webm"}`;
          link.hidden = false; $("#record").textContent = "Record room"; $("#record").setAttribute("aria-pressed", "false"); render();
          if (disposed) URL.revokeObjectURL(recordingURL);
        };
        draw(); recordingTimer = setInterval(draw, 1000 / 24); recorder.start(1000);
        $("#record").textContent = "Stop recording"; $("#record").setAttribute("aria-pressed", "true"); output.textContent = "Recording this room locally.";
      } catch (error) { cleanup(); throw error; }
      } finally { recordingStarting = false; render(); }
    });
    const unregister = slots.register("session.media", section, 20);
    const unsubscribeVideo = video.subscribe(render);
    const unsubscribeState = state.subscribe((snapshot) => { connected = snapshot.connection.phase === "connected";
      $("#apply-scenario").disabled = !connected;
      if (selectedPreset?.name === "a recorded menu") for (const tool of snapshot.tools ?? []) {
        if (tool.name !== "press_key" || tool.status !== "pending" || answeredTools.has(tool.call_id)) continue;
        answeredTools.add(tool.call_id);
        // This is the explicitly selected simulated menu; it has no external effects.
        let digit; try { digit = JSON.parse(tool.arguments).digit; } catch {}
        const destination = digit === "2" ? "order-status" : "other-option";
        $("#scenario-tool").textContent = `Simulated keypad: ${digit ?? "invalid"} → ${destination}`;
        queueMicrotask(() => { if (!disposed && connected) state.toolResult(tool.call_id, "done", JSON.stringify({ok:digit === "2", destination})); });
      }
      if (!connected) { stopRecording(); answeredTools.clear(); } render(); });
    context.lifecycle.defer("room", () => {
      disposed = true; stopRecording(); recordingCleanup?.();
      if (recordingURL) URL.revokeObjectURL(recordingURL);
      events.forEach((off) => off()); unsubscribeVideo(); unsubscribeState();
      preview("#camera-preview", null); preview("#screen-preview", null); unregister();
    });
  },
};
