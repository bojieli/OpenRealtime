# Fish Speech 1.5

The speech the agent produces is synthesised by Fish Speech 1.5 running locally.
It replaced S2-Pro on SGLang-Omni, which answered a one-sentence request in about
900ms; this path answers a comma-length phrase in 111-230ms.

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

    git clone https://github.com/fishaudio/fish-speech .runtime/fish/fish-speech
    cd .runtime/fish/fish-speech && git checkout v1.5.1
    git apply /path/to/tools/fish15/cuda-graph-ownership.patch
    touch .project-root
    ln -s <hf snapshot of fishaudio/fish-speech-1.5> checkpoints/fish-speech-1.5

    PYTHONPATH=$PWD python tools/fish15/server.py --port 8123

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
