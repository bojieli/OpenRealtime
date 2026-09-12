import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

const PAGE_URL = process.argv[2];
const PORT = Number(process.env.CDP_PORT ?? 19410);
const profile = mkdtempSync(join(tmpdir(), "openrealtime-plugin-client-"));
const chromium = // A fresh profile makes every launch a first run, so Chromium spends real time
// on GCM registration, component updates, and PKI metadata before it settles.
// Every page these drivers open is served from a loopback listener in the test
// process, so a browser reaching the network is doing work nothing asked for -
// and on a CI runner that work is what the waits below end up queued behind.
// bench/meeting and bench/realtimecu already launch Chromium this way.
spawn(process.env.CHROMIUM ?? "chromium", [
  "--disable-background-networking", "--disable-component-update",
  "--disable-default-apps", "--no-first-run", "--disable-sync",
  "--disable-client-side-phishing-detection",
  "--headless=new", `--remote-debugging-port=${PORT}`, "--no-sandbox", "--disable-gpu",
  "--use-fake-device-for-media-stream", "--use-fake-ui-for-media-stream", "--autoplay-policy=no-user-gesture-required", `--user-data-dir=${profile}`, "about:blank",
], { stdio: ["ignore", "pipe", "pipe"] });
let chromiumErrors = "";
chromium.stderr.on("data", (chunk) => { chromiumErrors += chunk.toString(); });

async function endpoint() {
  for (let attempt = 0; attempt < 100; attempt++) {
    try {
      const response = await fetch(`http://127.0.0.1:${PORT}/json/version`);
      return (await response.json()).webSocketDebuggerUrl;
    } catch { await sleep(100); }
  }
  throw new Error(`Chromium did not start:\n${chromiumErrors}`);
}

class CDP {
  #socket; #next = 1; #pending = new Map(); #handlers = new Map();
  static async connect(url) {
    const client = new CDP();
    client.#socket = new WebSocket(url);
    await new Promise((resolve, reject) => {
      client.#socket.addEventListener("open", resolve, { once: true });
      client.#socket.addEventListener("error", reject, { once: true });
    });
    client.#socket.addEventListener("message", (message) => {
      const frame = JSON.parse(message.data);
      if (frame.id && client.#pending.has(frame.id)) {
        const pending = client.#pending.get(frame.id); client.#pending.delete(frame.id);
        frame.error ? pending.reject(new Error(JSON.stringify(frame.error))) : pending.resolve(frame.result);
      } else if (frame.method) {
        for (const handler of client.#handlers.get(frame.method) ?? []) handler(frame.params);
      }
    });
    return client;
  }
  on(method, handler) {
    const handlers = this.#handlers.get(method) ?? []; handlers.push(handler); this.#handlers.set(method, handlers);
  }
  send(method, params = {}, sessionId) {
    const id = this.#next++;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject });
      this.#socket.send(JSON.stringify({ id, method, params, sessionId }));
    });
  }
}

const checks = [];
const check = (label, ok) => { checks.push({label, ok}); console.log(`${ok ? "PASS" : "FAIL"} ${label}`); };
try {
  const browser = await CDP.connect(await endpoint());
  const {targetId} = await browser.send("Target.createTarget", {url:"about:blank"});
  const {sessionId} = await browser.send("Target.attachToTarget", {targetId, flatten:true});
  const call = (method, params) => browser.send(method, params, sessionId);
  await call("Runtime.enable"); await call("Page.enable");
  const errors = [];
  browser.on("Runtime.exceptionThrown", ({exceptionDetails}) => errors.push(exceptionDetails.exception?.description ?? exceptionDetails.text));
  const evaluate = async (expression) => {
    const {result, exceptionDetails} = await call("Runtime.evaluate", {expression, awaitPromise:true, returnByValue:true, userGesture:true});
    if (exceptionDetails) throw new Error(exceptionDetails.exception?.description ?? exceptionDetails.text);
    return result.value;
  };
  const wait = async (expression, ms=20000) => { const end=Date.now()+ms; do { if(await evaluate(expression)) return true; await sleep(100); } while(Date.now()<end); return false; };
  await call("Page.navigate", {url:PAGE_URL});
  check("room booted", await wait(`document.getElementById('openrealtime-root')?.dataset.state === 'ready'`));
  await evaluate(`document.getElementById('connect').click()`);
  check("joined server over WebRTC", await wait(`document.getElementById('state')?.textContent === 'connected'`));
  check("all twelve practice scenarios are available", await evaluate(`document.getElementById('room-scenario')?.options.length === 13`));
  check("scenario pipeline negotiated live video", await wait(`document.getElementById('video-camera')?.disabled === false`));
  await evaluate(`document.getElementById('microphone').click(); document.getElementById('speaker').click()`);
  check("independent microphone and speaker mute", await evaluate(`document.getElementById('microphone').getAttribute('aria-pressed') === 'true' && document.getElementById('speaker').getAttribute('aria-pressed') === 'true'`));
  await evaluate(`document.getElementById('text').value='What is two plus two? Answer with only the number.'; document.querySelector('.conversation-panel form').requestSubmit()`);
  check("pipeline answered typed message", await wait(`Array.from(document.querySelectorAll('#conversation article[data-role=assistant]')).some(n=>/4|four/i.test(n.textContent))`, 90000));
  await evaluate(`document.getElementById('video-camera').click()`);
  check("camera frames captured", await wait(`Number(document.getElementById('camera-preview')?.dataset.frames)>0`));
  await evaluate(`document.getElementById('video-screen').click()`);
  check("camera and screen simultaneously active", await wait(`Number(document.getElementById('screen-preview')?.dataset.frames)>0 && document.getElementById('camera-preview')?.srcObject?.active`));
  await evaluate(`document.getElementById('video-screen').click()`);
  check("screen stops without stopping camera", await wait(`document.getElementById('screen-preview')?.srcObject === null && document.getElementById('camera-preview')?.srcObject?.active`));
  await evaluate(`document.getElementById('record').click()`);
  check("room recording started", await wait(`document.getElementById('record')?.getAttribute('aria-pressed') === 'true'`));
  await sleep(1500);
  await evaluate(`document.getElementById('record').click()`);
  check("recording finalized locally", await wait(`document.getElementById('recording-download')?.hidden === false`));
  check("recording contains media bytes", await evaluate(`(async()=>{const a=document.getElementById('recording-download'); if(!a?.href.startsWith('blob:'))return false;const v=document.createElement('video');v.src=a.href;return await new Promise(resolve=>{v.onloadedmetadata=()=>{resolve(v.videoWidth===1280 && v.videoHeight===720);v.removeAttribute('src');v.load();};v.onerror=()=>resolve(false);setTimeout(()=>resolve(false),5000);});})()`));
  await evaluate(`document.getElementById('video-camera').click(); document.getElementById('agent-visual').click()`);
  check("agent visual independent from audio", await evaluate(`document.getElementById('agent-avatar').hidden && document.getElementById('speaker').getAttribute('aria-pressed')==='true'`));
  await sleep(3000);
  // Decode an uploaded image through the same input path as the file picker.
  await evaluate(`(async()=>{const c=document.createElement('canvas');c.width=64;c.height=64;c.getContext('2d').fillRect(0,0,64,64);const blob=await new Promise(r=>c.toBlob(r,'image/png'));const d=new DataTransfer();d.items.add(new File([blob],'test.png',{type:'image/png'}));const input=document.getElementById('image-input');input.files=d.files;input.dispatchEvent(new Event('change'));})()`);
  check("image submitted", await wait(`document.getElementById('video-state').textContent.includes('Image sent')`));
  if(process.env.ROOM_SCREENSHOT) {
    const {writeFileSync}=await import('node:fs');
    await call('Emulation.setDeviceMetricsOverride',{width:1440,height:1000,deviceScaleFactor:1,mobile:false});
    const {data}=await call('Page.captureScreenshot',{format:'png'});writeFileSync(process.env.ROOM_SCREENSHOT,Buffer.from(data,'base64'));
  }
  check("no server error surfaced", await evaluate(`document.getElementById('error').textContent === ''`));
  console.log("room diagnostic:", await evaluate(`document.getElementById('error').textContent + ' ' + document.getElementById('video-state').textContent`));
  await evaluate(`document.getElementById('leave').click()`);
  check("leave releases camera and screen", await wait(`document.getElementById('state').textContent === 'disconnected' && document.getElementById('camera-preview').srcObject === null && document.getElementById('screen-preview').srcObject === null`));
  await evaluate(`window.roomOriginalGetUserMedia = navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices); navigator.mediaDevices.getUserMedia = async options => { if(options.audio) throw new DOMException('Test denial','NotAllowedError'); return window.roomOriginalGetUserMedia(options); }; document.getElementById('connect').click()`);
  check("room connects when microphone permission is denied", await wait(`document.getElementById('state').textContent === 'connected'`));
  await evaluate(`navigator.mediaDevices.getUserMedia = window.roomOriginalGetUserMedia; document.getElementById('microphone').click()`);
  check("microphone can be enabled later without reconnecting", await wait(`document.getElementById('microphone').getAttribute('aria-pressed') === 'false' && document.getElementById('state').textContent === 'connected'`));
  await evaluate(`document.getElementById('leave').click()`);
  check("no browser exceptions", errors.length===0); if(errors.length) console.log(errors);
  await evaluate(`window.__openrealtime.dispose()`);
  check("all room elements disposed", await evaluate(`document.getElementById('openrealtime-root').children.length===0`));
} catch(error) { check(error.stack ?? error.message,false); }
finally { chromium.kill('SIGKILL'); await sleep(300); rmSync(profile,{recursive:true,force:true}); }
process.exit(checks.every(x=>x.ok)?0:1);
