# Interleaved continuation benchmark v0.1

This small, original workload is a deterministic integration benchmark for the
canonical fast/slow trajectory. It is not presented as a broad intelligence
leaderboard.

Each task contains a user request, a task-local immutable record corpus, exact
answer substrings, and required tool calls. Both continuations receive the real
`records.read` schema. Fast has proposal-only authority: a native tool-call
event becomes a `tool_proposal` and cannot reach the executor. Slow has execute
authority, independently emits the authoritative call, consumes its result from
the same trajectory, and produces a self-contained final answer.

The workload checks four concrete properties:

1. the fast model does not need to deny an available capability;
2. a fast proposal is visible to slow but cannot execute or accept a result;
3. the slow model inherits fast assistant/proposal state exactly;
4. executable calls and results appear once in causal order;
5. the final answer uses information unavailable in the original prompt.

Run the homogeneous Gemini condition:

```bash
GEMINI_API_KEY=... go run ./cmd/interleavebench \
  --fast-provider gemini --fast-effort minimal \
  --slow-provider gemini --slow-effort high --slow-max-tokens 2048 \
  --replicates 3
```

Run a local-Qwen/Gemini condition after starting a vLLM server:

```bash
GEMINI_API_KEY=... go run ./cmd/interleavebench \
  --fast-provider vllm --fast-model qwen-fast \
  --fast-base-url http://127.0.0.1:8000/v1 \
  --slow-provider gemini --slow-effort high
```

Reports contain assistant text, tool proposals, executable calls/results,
timing, token usage, and a trajectory digest. They exclude credentials,
plaintext reasoning, and opaque provider state. The task data and harness are
released under the repository's license; the task records were authored
specifically for OpenRealtime.

The complete voice-path integration and a passed opaque-record trial are
documented in [the live cascade report](../../../docs/live-cascade.md).
