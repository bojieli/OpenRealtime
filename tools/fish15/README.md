# Fish Speech 1.5

This directory provides a local Fish Speech 1.5 server for the runtime's Fish
adapter. It is an optional model service, not a dependency of every voice
configuration.

Earlier measurements observed 111–230 ms for short phrases on this path and
about 900 ms for a one-sentence request through S2-Pro on SGLang-Omni. The inputs
and stacks differ, so those numbers are diagnostic context rather than a
controlled model comparison.

## Why not the upstream server

`tools/api_server.py` in the fish-speech tree routes every request through
`TTSInferenceEngine`, which empties the CUDA caching allocator and runs a full
garbage collection after each synthesis. Measured against this checkpoint:

    generation of a one-second phrase    ~90ms
    firefly-gan vocoder                  ~40ms
    upstream server, same request        375ms

The 245ms difference is allocator thrash and float64 numpy round-trips, not
model time. `server.py` speaks the same JSON `/v1/tts` contract, so the Go
adapter needs no change, but goes straight from the semantic token queue to the
decoder and leaves the allocator alone.

## Setup

You need a CUDA-capable PyTorch environment, the Fish Speech v1.5.1 dependencies,
and a local `fishaudio/fish-speech-1.5` checkpoint. Install the upstream runtime
dependencies for your GPU before starting this service. From the OpenRealtime
repository root:

```bash
git clone --branch v1.5.1 https://github.com/fishaudio/fish-speech .runtime/fish/fish-speech
git -C .runtime/fish/fish-speech apply ../../../tools/fish15/cuda-graph-ownership.patch
touch .runtime/fish/fish-speech/.project-root
```

Set the checkpoint path to the downloaded weights, then start this repository's
server with the upstream model code on `PYTHONPATH`:

```bash
export FISH15_CHECKPOINT="/absolute/path/to/fish-speech-1.5"
PYTHONPATH="$PWD/.runtime/fish/fish-speech" python tools/fish15/server.py \
  --checkpoint "$FISH15_CHECKPOINT" --port 8123
```

Startup compiles the CUDA graphs and warms the decoder shapes, which takes about
21 seconds. Requests that arrive before `ready on :PORT` would otherwise pay that
compilation themselves.

## The patch

`decode_one_token` is captured as a CUDA graph, so its result lives in a buffer
the next call overwrites. `decode_n_tokens` keeps a view of that result in
`cur_token` and feeds it to the next call, which torch detects and refuses. The
release predates the torch version that checks for this, so upstream never hit
it. Cloning at the call site - the caller keeps the token past the call that
produced it, so the caller owns it - is the whole fix.
