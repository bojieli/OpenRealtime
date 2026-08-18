# GPT-Live and TML interaction capability audit

This audit compares OpenRealtime with **GPT-Live** and Thinking Machines Lab's
interaction models. LiveKit is not the product comparison target. It appears
only as incidental transport in the upstream Full-Duplex-Bench v3 harness and
as the source of a separately inventoried endpointing component benchmark.

The machine-readable claim/evidence ledger is
`benchmarks/external/gpt-live-and-tml-capabilities-2026-08.json`. A published
claim, an implemented invariant, and a completed benchmark result are kept as
three different evidence classes.

## Architectural finding

The common principle is not “pick a faster model.” It is two orthogonal loops:

```text
continuous media / incremental evidence ──→ bounded interaction work
                         │                         │
                         └── canonical context ───┴──→ deeper continuation/tools
```

GPT-Live keeps full-duplex voice on a dedicated path and delegates deeper work
asynchronously. TML interleaves 200 ms input/output micro-turns and sends a rich
shared context to an asynchronous background model. OpenRealtime implements
the same *separation of concerns* with modular components: 20 ms media frames,
50 ms scheduler opportunities, stateful 200 ms ASR advances, revision-safe
fast→slow preparation, and one canonical fast→slow→tool-result trajectory.

That does not make the architectures identical. GPT-Live removes turn detection
from its audio path, and TML trains a speech-native interaction model to choose
silence, overlap, and interjection inside each micro-turn. The current local
gateway still uses server VAD to promote speech into a canonical response and
normally cancels speech when new user speech begins. Its fine cadence starts
useful cognition early; it does not yet provide learned, speech-native
simultaneous output.

## Capability-by-capability status

| Capability | GPT-Live / TML public claim | OpenRealtime status | Decisive evidence |
| --- | --- | --- | --- |
| Listen and speak concurrently | Native continuous/full-duplex streams | Partial | Duplex media and barge-in are implemented; canonical response commitment remains VAD-gated |
| Several interaction decisions per second | GPT-Live: many times per second; TML: 200 ms micro-turns | Partial | 50 ms opportunities and stateful 200 ms ASR/preparation; no canonical 200 ms speech policy |
| Backchannels, pauses, side speech, background speech | Learned interaction behavior | Full run pending | Official 498-example FDB v1.5 run is queued |
| Proactive/time-aware speech and simultaneous translation | Native interaction behavior | Not end-to-end | Deterministic time and translation fixtures exist, but no learned proactive speech policy |
| Asynchronous deeper reasoning | Frontier/background model off the live path | Implemented | Every observation runs fast→slow; no content router or model-authored workflow state |
| Shared fast/slow context | Full conversation shared with background reasoning | Implemented | One versioned canonical trajectory; portable history across model families |
| Tools without split brain | Interaction model knows tools; deep path executes | Implemented, full runs pending | Both phases receive schemas; fast can only propose; slow calls execute; results resume slow |
| Media/application isolation | Tools and delegation cannot stall audio | Implemented for local WebSocket profile | Concurrent workers enqueue to a single safe-point owner; no tool callback mutates media state |
| Persistent prewarm/cache | Stateful sessions and prefilled deep model | Partial | Stateful ASR and pre-endpoint preparation; no claim of cross-family KV continuity |
| Provisional vs authoritative history | Mutable current turn plus stable record | Implemented | Exact-fingerprint speculative branches and append-only canonical commit |
| Context compaction with seamless model handoff | Warm replacement, parallel prefill, cutover | Not implemented | Explicit future gap |
| Long-context/tool intelligence | GPQA/BrowseComp/internal τ variant; TML FDB v3 | Full runs pending | Public 278-task τ-Voice and 100-recording official FDB v3 are queued |

The first preserved failure in the frozen τ control cell further separates
the concerns: the event loop stayed responsive and the slow model produced
schema-valid tool calls, but Qwen3-ASR 0.6B corrupted a spoken identifier. A
complete Qwen3-ASR 1.7B paired cell is therefore preregistered. This changes a
perception-capacity variable rather than adding identifier patterns, task
lookup, or a semantic router.

## Benchmark interpretation

The local matrix deliberately uses complementary suites:

- FDB v1.5 measures observable duplex behavior across interruption,
  backchannel, side-conversation, and background-speech recordings.
- Public τ-Voice measures long multi-turn task intelligence and tool use across
  airline, retail, and telecom. OpenAI's published GPT-Live number uses an
  internal variant and customized user model, so it is contextual evidence,
  not a directly comparable score.
- FDB v3 measures 100 human-recorded disfluent tool-use examples against the
  upstream 12-tool API and official evaluator.
- Deterministic gateway tests establish synchronization, authority, and result
  resumption invariants that aggregate benchmark scores cannot prove.

TML reports 0.40 s on FDB v1 turn-taking latency, 77.8 on FDB v1.5, and
82.8% response quality / 68.0% Pass@1 on FDB v3 for
`TML-Interaction-Small`. The article marks reasoning/tool results that use its
background agent. The model is described as a 276B-parameter MoE with 12B
active and a limited research preview, so these remain published reference
values rather than a locally executable baseline.

## GPT-Live availability boundary

The official launch page says GPT-Live-1 and GPT-Live-1 mini are rolling out in
ChatGPT and that an API is planned; the engineering article likewise calls the
GPT-Live API upcoming. An authenticated model-list probe on 2026-08-18 exposed
GPT-Realtime and GPT-Live-Transcribe families but no GPT-Live voice model for
this account. We therefore do not relabel GPT-Realtime-2 as GPT-Live and do not
claim a direct GPT-Live benchmark.

## Sources

- [Introducing GPT-Live](https://openai.com/index/introducing-gpt-live/)
- [How OpenAI built continuous voice interaction with GPT-Live](https://openai.com/index/continuous-voice-interaction-with-gpt-live/)
- [Thinking Machines Lab: Interaction Models](https://thinkingmachines.ai/blog/interaction-models/)
- [Full-Duplex-Bench](https://github.com/DanielLin94144/Full-Duplex-Bench)
