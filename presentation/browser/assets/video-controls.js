export default {
  name: "openrealtime.presentation.client.video-controls",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const video = context.services.get("presentation.client.video");
    if (!slots || !video) throw new Error("video control dependencies are unavailable");
    const section = document.createElement("section");
    section.dataset.view = "video";
    section.innerHTML = `<style>
      [data-view=video] { max-width:54rem; margin:0 auto 1rem; padding:0 1rem; }
      [data-view=video] button { margin-right:.5rem; padding:.55rem .8rem; }
      [data-view=video] output { display:block; margin-top:.5rem; font:12px ui-monospace,monospace; }
    </style><h2>Video sources</h2><button id="video-camera">Start camera</button>
    <button id="video-screen">Start screen</button><button id="video-stop">Stop video</button>
    <output id="video-state"></output>`;
    const output = section.querySelector("#video-state");
    const buttons = {
      camera: section.querySelector("#video-camera"),
      screen: section.querySelector("#video-screen"),
      stop: section.querySelector("#video-stop"),
    };
    const render = (snapshot) => {
      buttons.camera.disabled = !snapshot.enabled;
      buttons.screen.disabled = !snapshot.enabled;
      buttons.stop.disabled = snapshot.active.length === 0;
      const media = Object.entries(snapshot.media ?? {}).map(([source, row]) =>
        `${source}: ${row.frames} frames, ${row.drops} drops, ${row.last_frame_bytes} B`).join("; ");
      output.textContent = snapshot.diagnostic || media ||
        (snapshot.enabled ? "ready" : "video input not negotiated");
    };
    const startCamera = () => video.start("camera").catch((error) => { output.textContent = error.message; });
    const startScreen = () => video.start("screen").catch((error) => { output.textContent = error.message; });
    const stop = () => video.stopAll();
    buttons.camera.addEventListener("click", startCamera);
    buttons.screen.addEventListener("click", startScreen);
    buttons.stop.addEventListener("click", stop);
    const unregister = slots.register("session.media", section, 20);
    const unsubscribe = video.subscribe(render);
    context.lifecycle.defer("video-control-events", () => {
      buttons.camera.removeEventListener("click", startCamera);
      buttons.screen.removeEventListener("click", startScreen);
      buttons.stop.removeEventListener("click", stop);
    });
    context.lifecycle.defer("video-control-state", unsubscribe);
    context.lifecycle.defer("video-control-slot", unregister);
  },
};
