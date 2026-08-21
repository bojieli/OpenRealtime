# ADR-0007: Transports are protocol clients, never second entrances

## Status

Accepted, v1.0.

## Context

A WebSocket endpoint carrying raw PCM is not enough on its own for most
clients. Over a real network, echo cancellation, jitter buffering, packet loss
concealment, and adaptive bitrate are the difference between a demo and a
product, and leaving all of them to the client means most clients get them
wrong or not at all.

So the project needs WebRTC. The question was where it attaches.

The tempting answer is a second entrance into the session core: a WebRTC
handler that constructs a session directly, skipping the wire. It is faster, it
avoids a serialisation hop, and it is what most systems do.

## Decision

The protocol over WebSocket is the only entrance to a session. A transport
adapter is a protocol client: it terminates media and then speaks exactly the
events any other client speaks, over a real connection, even when it runs
in the same process.

No adapter has privileged access. If an adapter can express something a plain
WebSocket client cannot, that is a defect.

## Consequences

**One thing to specify, version, and test.** A transport that reached the
session core directly would become a second place the protocol can drift, and
every such place multiplies the compatibility surface until a feature exists on
one path and not the other.

**The rule is tested, not promised.** The adapter suite checks every event the
adapter sends against the set a plain client can send. The adapter originates
exactly two of its own — both expressible by any client — and forwards the rest
byte for byte. A third would fail the test.

**One exception, with a reason.** The adapter sets the session's audio format,
because it terminates media: a client's opinion about the format of a
connection its audio never travels on would break the media path without
meaning anything. That single field is dropped from a client's session update
and everything else in the same event survives.

**LiveKit can leave.** Because the integration is a client rather than a
component, it ships as a separate module on its own release cycle, and could be
replaced by an equivalent for another provider — or rewritten by somebody else
entirely — without touching the server.

**The cost is a serialisation hop.** Audio crosses a local WebSocket rather
than a function call. On the hop that matters, client to adapter, the network
is what dominates; the adapter-to-protocol hop is local, and paying it buys a
compatibility surface that does not fork.
