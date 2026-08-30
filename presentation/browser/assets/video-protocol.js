const SOURCES = Object.freeze(["camera", "screen"]);

export default {
  name: "openrealtime.presentation.client.video-protocol",
  revision: 1,
  async mount(context) {
    const connection = context.services.get("presentation.client.connection");
    const state = context.services.get("presentation.client.session_state");
    const media = context.services.get("presentation.client.media");
    const configuration = context.services.get("presentation.client.session_configuration");
    if (!connection || !state || !media || !configuration) {
      throw new Error("video protocol dependencies are unavailable");
    }

    const listeners = new Set();
    const active = new Map();
    let diagnostic = "";
    let latest = state.snapshot();
    let disposed = false;
    const contribution = configuration.contribute({
      supports: ["observations", "video.input"],
      observers: ["audio", "video"],
    });

    const snapshot = () => Object.freeze({
      enabled: latest.connection.phase === "connected" &&
        latest.session.openrealtime.enabled.includes("video.input"),
      active: Object.freeze([...active.keys()].sort()),
      diagnostic,
      media: media.snapshot().video,
    });
    const publish = () => {
      const current = snapshot();
      for (const listener of listeners) {
        try { listener(current); } catch {}
      }
    };
    const send = (event) => {
      if (latest.connection.phase !== "connected") return false;
      try {
        connection.send(event);
        diagnostic = "";
        return true;
      } catch (error) {
        diagnostic = error?.message ?? String(error);
        return false;
      }
    };
    const observe = (event) => {
      if (disposed) return;
      const current = active.get(event.source);
      if (current && current.generation !== event.generation) return;
      switch (event.kind) {
        case "opened":
          send({
            type: "openrealtime.input_video_source.update", source: event.source,
            state: "active", width: event.width, height: event.height,
          });
          break;
        case "frame":
          send({
            type: "openrealtime.input_video_frame.append", source: event.source,
            frame: event.frame, timestamp_ms: event.captured_at_ms,
          });
          break;
        case "closed":
          if (current?.generation === event.generation) active.delete(event.source);
          send({
            type: "openrealtime.input_video_source.update", source: event.source, state: "closed",
          });
          break;
        case "failed":
          diagnostic = event.error;
          break;
        case "dropped":
          diagnostic = event.reason;
          break;
      }
      publish();
    };
    const stop = (source) => {
      if (!SOURCES.includes(source)) throw new Error(`unknown video source ${source}`);
      media.stopVideo(source);
      active.delete(source);
      publish();
    };
    const stopAll = () => {
      for (const source of [...active.keys()]) stop(source);
    };
    const unsubscribeState = state.subscribe((next) => {
      latest = next;
      if (next.connection.phase !== "connected" ||
          !next.session.openrealtime.enabled.includes("video.input")) stopAll();
      publish();
    });

    const service = Object.freeze({
      snapshot,
      subscribe(listener) {
        if (typeof listener !== "function") throw new Error("video listener must be a function");
        listeners.add(listener);
        try { listener(snapshot()); } catch {}
        return () => listeners.delete(listener);
      },
      async start(source) {
        if (disposed) throw new Error("video service is disposed");
        if (!SOURCES.includes(source)) throw new Error(`unknown video source ${source}`);
        if (latest.connection.phase !== "connected" ||
            !latest.session.openrealtime.enabled.includes("video.input")) {
          throw new Error("video input is not negotiated for this session");
        }
        stop(source);
        const limits = latest.session.openrealtime.video;
        const handle = await media.startVideo(source, limits, observe);
        if (disposed) {
          handle.stop();
          throw new Error("video service was disposed while capture started");
        }
        active.set(source, handle);
        diagnostic = "";
        publish();
        return snapshot();
      },
      stop,
      stopAll,
    });
    context.publish("presentation.client.video", service);
    context.lifecycle.defer("video-protocol", () => {
      disposed = true;
      unsubscribeState();
      contribution();
      stopAll();
      listeners.clear();
    });
  },
};
