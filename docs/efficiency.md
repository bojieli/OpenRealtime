# Efficiency gates

This page records resource and latency diagnostics for a named reference
machine. They measure runtime overhead in specific configurations, not overall
model quality or a hardware-independent performance promise.

The release matrix exposes `local.efficiency` as an optional diagnostic. Its
report captures the machine and measured values for that run. See
[Release validation](release-validation.md#optional-efficiency-diagnostic) for
selection and evidence requirements.

Reproduce them:

```sh
openrealtime efficiency -seconds 60
```

## Reference machine

| | |
| --- | --- |
| CPU | 13th Gen Intel Core i9-13900KS, 32 threads |
| GPU | NVIDIA RTX PRO 6000 Blackwell Workstation Edition, 96 GB |
| OS | Linux, amd64 |
| Go | 1.25.0 |

The GPU is shared with the local recogniser, fast model, and synthesiser, which
is the configuration a cascade deployment actually runs.

## 1. An idle video source

The headline number. A client sends 1080p at 3 fps into a screen nobody is
touching, for a minute.

| | |
| --- | --- |
| Frames received | 180 |
| Admitted by the gate | 1 |
| Narrated | 1 |
| Gate cost | **304 ns per frame** |
| Total gate cost for the minute | 55 µs |

179 of 180 frames are rejected without decoding anything, and the whole
minute's gating costs less than a tenth of a millisecond. This is what "costs
almost nothing and produces nothing on unchanged input" means in practice.

The gate compares a fingerprint — the frame length and 128 sampled bytes —
rather than the frame itself. Comparing 200 KB in full costs 37 µs, which would
make the idle path, the one that runs most of the time, by far the most
expensive thing in the system. A fingerprint collision costs one wasted decode
that the pixel comparison then rejects; there is no correctness consequence.

## 2. A changing video source

The other end: a screen where something moves at every sample.

| | |
| --- | --- |
| Frames received | 180 |
| Admitted by the cheap gate | 180 |
| Narrated after pixel comparison | 137 |
| Gate cost | 243 ns per frame |
| Decode and compare | **7.4 ms per admitted frame** |

43 frames pass the cheap gate and are then rejected by the pixel comparison,
which is the second stage doing its job. 7.4 ms of decode against a 333 ms
sampling interval leaves the budget intact.

## 3. Added latency of an enabled video observer on a voice task

Measured against the live stack — real recogniser, real fast model, real hosted
reasoner, real synthesiser — with the same recording, alternating between a
server with a video observer configured and one without, no video frames sent.

| | Voice only | Video observer enabled |
| --- | --- | --- |
| First audio after the endpoint, median | 1025 ms | 1014 ms |
| Range over 7 runs | 909–1160 ms | 908–1182 ms |

**No measurable difference.** The run-to-run spread of the hosted reasoner is
roughly 250 ms, which is more than an order of magnitude larger than any
difference between the two conditions. That is what the architecture predicts:
the video path is a separate observer on separate events with its own gate, so
a voice turn does not pay for it until frames arrive.

## 4. Bandwidth, with and without client-side gating

| Arrangement | 1080p at 3 fps |
| --- | --- |
| Ungated, as sent | **597 KB/s** |
| Server-gated (what this system does) | 597 KB/s |
| Client-gated (hypothetical) | 3.3 KB/s |

The honest number, and it is the one that says what the design gives up.
Gating on the server saves compute and context, not bandwidth: the client
already sent the frame.

That trade is deliberate. Selective perception is what this system is *for*,
and a client that must implement pixel-change gating at a specific threshold is
a client nobody writes — every client would reimplement it differently or not
at all, and the observation a session produces would stop being a property of
the server.

Bandwidth is a transport problem. A WebRTC video track removes exactly this
redundancy through inter-frame prediction, which is where it should be removed:
a static screen costs almost nothing on a media track, and solving it in the
client would be solving it in the wrong layer, twice.

## 5. Context growth from narration

A minute of the worst case — 137 narrated observations, every one carrying a
keyframe handle.

| | |
| --- | --- |
| Narration text | 112 bytes each |
| Trajectory after one minute | **63 KB** |

That is what gets copied for every continuation request, which is why the
trajectory references media by handle and never inlines it. The same minute
with the keyframes inlined at 200 KB each would be 27 MB per snapshot.

Real usage is far below this: a screen that changes at every one of 180 samples
for a full minute is a video, not a workspace.

## 6. The audio gate

The path that runs continuously, whatever else is happening.

| | |
| --- | --- |
| Cost per 20 ms frame | **519 ns** |
| Fraction of realtime | 0.0026% |

An acoustic gate that spent a measurable fraction of its budget would be a
problem, because it runs on every frame of every session for the whole session.
It does not.
