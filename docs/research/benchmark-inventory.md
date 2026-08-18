# Benchmark and workload inventory

Retrieved 2026-08-17. “Adapter policy” states the safe initial integration; it
is not legal advice and must be rechecked at the pinned revision.

| Resource | Coverage | Public artifact state | License observation | Adapter policy |
| --- | --- | --- | --- | --- |
| Project controlled timing fixtures | pauses, endpoints, predictable/ambiguous continuations | M0 tone exists; M1 speech pending | original fixtures CC0 | vendored with hashes |
| Full-Duplex-Bench v1/v1.5 | pause, backchannel, turn-taking, interruption, overlap | official code/data repository | repository license CC BY-NC 4.0 | external opt-in; no vendoring |
| Full-Duplex-Bench v2 | dynamic multi-turn examiner | official repository, actively evolving | same repository license; inspect sub-artifacts | experimental external adapter |
| Full-Duplex-Bench v3 | disfluent speech and tool use | exact 736,136,419-byte/100-recording release, code, mock APIs, evaluator, and paper tables pinned; full local run queued | inspect every sub-artifact | standard OpenAI Realtime adapter, actual tool results, terminal result resumption, exact plus official LLM judge |
| FD-Bench | long-form full-duplex response timing, interruptions, noise, TTS sensitivity, WER/CPPL, and subjective quality | exact 8,310,251,185-byte input release pinned: 13 archives expand to 21 cells, 6,147 conversations, and 77.2184 hours; only three ChatTTS cells omit IDs 60 and 120 from the nominal 293 | NTUitive non-commercial license with internally inconsistent distribution clauses; external only | complete standard-Realtime matrix queued after the ASR ablation; exact Silero settings and upstream timing core, with unavailable non-Moshi WER/CPPL stages disclosed rather than fabricated |
| TOBench | 100 closed-loop omni-modal tasks, 27 MCP servers, 324 tools | official MIT repository and external task bundle | per-task services and asset terms require inventory | comparison/inventory only: upstream harness requires Python and is not a realtime duplex voice benchmark |
| τ-Voice | 278 grounded airline/retail/telecom tasks with real tools, long policy context, full-duplex interaction, accents/noise, interruptions, backchannels, tics, and non-directed speech | paper and MIT `tau2-bench` harness pinned at `c339866`; patch passes 89 affected tests; provider gate and exploratory tasks complete; pre-freeze pilot running and frozen-runtime population queued | repository MIT; seven generated Fish voices have separate provenance | primary M10 joint task/interaction benchmark; retain the standard OpenAI adapter against the local gateway, disable only false Pong-deadline closure during deliberate simulator receive gaps, allow at most three exception-only retries with every attempt preserved, exclude the pre-freeze pilot from causal comparisons, and report only complete hash-bound cells |
| GPT-Live / TML capability audit | continuous full-duplex decisions, async background reasoning, tools, cache/handoff, long-context intelligence | official engineering/launch articles and account-scoped model availability recorded | published claims only; no model outputs redistributed | product/architecture comparison; never substitute GPT-Realtime or LiveKit for GPT-Live, and do not claim a local score without an executable endpoint |
| FT-Bench (TREX) | 10 autonomous LLM fine-tuning tasks | paper and task dataset public | task datasets and training assets require per-task review | out of scope: it evaluates training-agent research workflows, not live speech or realtime protocols |
| SimulEval | simultaneous text/speech translation quality and latency | official repository archived 2025-09-18 | CC BY-SA 4.0 stated by repository | protocol adapter or clean metric implementation |
| Moshi | native full-duplex open baseline | official code and weights public | component-specific terms require inventory | optional N1 adapter; never core dependency |
| Rapid audio games | reaction, timing, rule adherence | project workload pending | original scripts/fixtures target CC0 | build in M5 |
| Difficult questions | fast response, continued reasoning, and tools | symbolic M4 and original four-task interleaved workload exist; one live opaque-record voice trial completed | project prompts/record fixtures are original; generated/model outputs require provenance | retain M4 as independent control; expand paired interleaved conditions in M9 |
| Trajectory consistency | contradiction, repeated work, capability denial, proposal/call authority, interruption/resumption | proposal-versus-execute invariants and one positive/one negative live integration artifact exist; paired corpus pending | target original fixtures and event scripts | build paired independent/interleaved conditions in M9 |
| Paralinguistic challenge | emotion, sarcasm, laughter, sighs, non-speech | source not selected | voice/performer rights are critical | do not collect until ethics checklist |

## Selection gates

A benchmark enters a release only when its exact revision, task taxonomy,
metric implementation, license, data origin, speaker rights, redistribution
policy, required models, expected compute, and known limitations are recorded.
Access alone does not imply redistribution permission.

TOBench is relevant to general tool-using agent quality, but it does not isolate
realtime voice turn-taking or provider audio protocols. Its official harness
also requires Python 3.12 plus a large mixed MCP/Node environment. The current
live-provider experiment therefore uses Full-Duplex-Bench v1.5 as the compact
paired duplex benchmark, Full-Duplex-Bench v3 as the tool-use benchmark, and a
separately pinned full FD-Bench matrix as the long-form interruption/noise/TTS
sensitivity benchmark. It does not relabel a partial TOBench port as a local
compatible result.

The similarly named 2026 FT-Bench belongs to TREX and measures autonomous LLM
fine-tuning over ten training tasks. It is not a voice-agent benchmark. If
“FTbench” is intended to mean Full-Duplex-Bench/FD-Bench, both families now
have separately named and pinned adapters; their results are never merged.

τ-Voice is a better fit than TOBench for the fast/slow target because it keeps
grounded tool/task evaluation inside a full-duplex voice trajectory. Its 200 ms
simulation tick is compatible with the project's cadence study but is not
treated as an architectural mandate. The persistent local Realtime gateway,
provider conformance, and disclosed Fish voice registry are complete. The
running control is preserved as a pre-freeze pilot. A new complete control and
regular baseline is queued on the exact gateway executable shared by every
causal condition and external adapter population. A complete
1.7B-versus-0.6B ASR ablation is preregistered behind the primary queue. No
incomplete local score is claimed.
