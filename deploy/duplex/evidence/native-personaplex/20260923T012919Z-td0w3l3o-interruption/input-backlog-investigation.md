# Input backlog investigation — provisional, 2026-09-23

The pinned checkout at 1d221b9b has a transport-specific configuration wait
in bench/session.go: WebRTC waits on recorder.configured; WebSocket proceeds
to beginEpisode and real-time replay without waiting. Session creation may
therefore accumulate audio before the native model is ready. Ordered delivery
does not imply wall-clock-paced delivery after a slow initialization.

MoshiSidecar drops oldest frames above a 25-frame (2 s) input backlog. Early
PersonaPlex sessions report roughly 3–5 drops each despite typical model
compute below 80 ms/frame. This is consistent with a startup burst, but neither
the existing timeline nor the old counters establishes the cause.

Do not reinterpret drops as harmless or change this running campaign. Follow-up
must capture setup/ready timing, input packet arrivals, queue occupancy and
first-drop/model-frame counters. Main commit 2639ea48 adds the bounded queue
and first-drop counters; it is not loaded in this pinned campaign. Compare the
same fixtures with and without a verified readiness gate, retaining both runs.
A readiness-gated run measures warmed-session behavior and must not silently
replace connection-start behavior or shift latency origins without declaration.

Further source trace: gateway/server.go ServeHTTP calls websocket.Accept before
newSession, and logs session started only after newSession returns. The native
binding calls sidecar.Dial and waits for Ready in
binding/sidecarbinding/runtime.go. This establishes that WebSocket acceptance
precedes model readiness; bench/session.go starts WebSocket replay without a
configuration acknowledgement gate. Actual buffered audio volume still needs
measurement. The current campaign must not be described as already warmed at
the start of each recorded utterance.
