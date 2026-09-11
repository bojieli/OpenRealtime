"""Opt-in live Deepgram check: same recording, raw and filtered, at real time.

Requires DEEPGRAM_API_KEY and websockets 16. Audio is sent to Deepgram.
Reports ASR text and first-partial latency; makes no accuracy claim without a
human reference. Filter timing includes the local service round trip.
"""

import argparse
import asyncio
import http.client
import json
import os
import time
import uuid
import wave
from urllib.parse import urlencode, urlsplit
import websockets


async def run(pcm, rate, args, filtered):
    query = urlencode(
        dict(
            model="nova-3",
            language="en-US",
            encoding="linear16",
            sample_rate=rate,
            channels=1,
            interim_results="true",
            endpointing=300,
        )
    )
    endpoint = urlsplit(args.filter_url)
    connection = http.client.HTTPConnection(endpoint.hostname, endpoint.port, timeout=1)
    path = endpoint.path.rstrip("/") + "/v1/filter/" + uuid.uuid4().hex
    timings, finals, first = [], [], None
    first_partial = None
    async with websockets.connect(
        "wss://api.deepgram.com/v1/listen?" + query,
        additional_headers={"Authorization": "Token " + os.environ["DEEPGRAM_API_KEY"]},
    ) as socket:
        started = time.perf_counter()

        async def receive():
            nonlocal first, first_partial
            async for message in socket:
                event = json.loads(message)
                if event.get("type") != "Results":
                    continue
                text = event["channel"]["alternatives"][0]["transcript"]
                if text and first is None:
                    first = (time.perf_counter() - started) * 1000
                if text and not event.get("is_final") and first_partial is None:
                    first_partial = (time.perf_counter() - started) * 1000
                if text and event.get("is_final"):
                    finals.append(text)

        receiver = asyncio.create_task(receive())
        chunk = rate // 10 * 2
        try:
            for sequence, offset in enumerate(range(0, len(pcm), chunk)):
                await asyncio.sleep(
                    max(0, started + sequence * 0.1 - time.perf_counter())
                )
                body = pcm[offset : offset + chunk]
                if filtered:
                    began = time.perf_counter()

                    def process():
                        connection.request(
                            "POST",
                            path,
                            body,
                            {"X-Sample-Rate": str(rate), "X-Sequence": str(sequence)},
                        )
                        response = connection.getresponse()
                        result = response.read()
                        if response.status != 200 or len(result) != len(body):
                            raise RuntimeError("filter response failed")
                        return result

                    body = await asyncio.to_thread(process)
                    timings.append((time.perf_counter() - began) * 1000)
                # No audio reaches Deepgram until its filter request completes.
                await socket.send(body)
            await socket.send(json.dumps({"type": "CloseStream"}))
            await asyncio.wait_for(receiver, 10)
        finally:
            receiver.cancel()
            if filtered:
                connection.request("DELETE", path)
                connection.getresponse().read()
            connection.close()
    return {
        "filtered": filtered,
        "first_nonempty_result_ms": first,
        "first_partial_ms": first_partial,
        "transcript": " ".join(finals),
        "filter_max_roundtrip_ms": max(timings, default=0),
        "filter_deadline_misses": sum(t > args.budget_ms for t in timings),
        "filter_before_asr": filtered,
    }


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--wav", required=True)
    parser.add_argument("--filter-url", default="http://127.0.0.1:8125")
    parser.add_argument("--budget-ms", type=float, default=50)
    parser.add_argument("--report", required=True)
    args = parser.parse_args()
    with wave.open(args.wav, "rb") as source:
        if source.getnchannels() != 1 or source.getsampwidth() != 2:
            parser.error("mono PCM16 WAV required")
        rate, pcm = source.getframerate(), source.readframes(source.getnframes())
    results = []
    for filtered in (False, True):
        results.append(await run(pcm, rate, args, filtered))
    report = {
        "input": args.wav,
        "runs": results,
        "scope": "one recorded utterance, sequential cloud runs; not a controlled latency or WER benchmark",
    }
    with open(args.report, "w") as target:
        json.dump(report, target, indent=2)
        target.write("\n")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    asyncio.run(main())
