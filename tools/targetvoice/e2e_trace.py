"""Local diagnostic WebSocket relay: retain events, omit audio and credentials.

Adds ASR/policy/TTS debug subscriptions to the harness's session update. Does
not change audio, instructions, model selection, or turn policy. Use only with
a local test gateway. Timings are relative to the first microphone packet.
"""

import argparse
import asyncio
import json
import time
from pathlib import Path
import websockets


def scrub(value):
    if isinstance(value, dict):
        out = {}
        for key, item in value.items():
            if key.lower() in ("token", "authorization", "api_key", "audio"):
                continue
            if key == "delta" and value.get("type") == "response.output_audio.delta":
                out["audio_base64_chars"] = len(item)
            else:
                out[key] = scrub(item)
        return out
    if isinstance(value, list):
        return [scrub(x) for x in value]
    return value


async def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--port", type=int, default=18767)
    p.add_argument("--upstream", default="ws://127.0.0.1:18766/v1/realtime")
    p.add_argument("--out", required=True)
    args = p.parse_args()
    directory = Path(args.out)
    directory.mkdir(parents=True, exist_ok=False)
    counter = 0

    async def handle(client):
        nonlocal counter
        counter += 1
        path = directory / f"session-{counter:03d}.jsonl"
        started = None
        with path.open("w") as log:
            async with websockets.connect(args.upstream, max_size=16 << 20) as upstream:

                async def forward(source, target, direction):
                    nonlocal started
                    async for raw in source:
                        event = json.loads(raw)
                        if (
                            direction == "input"
                            and event.get("type") == "input_audio_buffer.append"
                        ):
                            if started is None:
                                started = time.perf_counter()
                        else:
                            if (
                                direction == "input"
                                and event.get("type") == "session.update"
                            ):
                                event["session"]["openrealtime"]["debug"] = {
                                    "enabled": True,
                                    "categories": [
                                        "session",
                                        "asr",
                                        "vad",
                                        "policy",
                                        "tts",
                                        "cognition",
                                        "error",
                                    ],
                                    "include_payloads": True,
                                }
                                raw = json.dumps(event)
                            log.write(
                                json.dumps(
                                    {
                                        "at_ms": (time.perf_counter() - started) * 1000
                                        if started
                                        else 0,
                                        "direction": direction,
                                        "event": scrub(event),
                                    }
                                )
                                + "\n"
                            )
                            log.flush()
                        await target.send(raw)

                tasks = [
                    asyncio.create_task(forward(client, upstream, "input")),
                    asyncio.create_task(forward(upstream, client, "output")),
                ]
                try:
                    done, pending = await asyncio.wait(
                        tasks, return_when=asyncio.FIRST_COMPLETED
                    )
                    for t in pending:
                        t.cancel()
                    await asyncio.gather(*tasks, return_exceptions=True)
                finally:
                    for t in tasks:
                        t.cancel()

    async with websockets.serve(handle, "127.0.0.1", args.port, max_size=16 << 20):
        await asyncio.Future()


if __name__ == "__main__":
    asyncio.run(main())
