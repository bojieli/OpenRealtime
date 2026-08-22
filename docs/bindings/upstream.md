# The `upstream` binding

A remote Realtime endpoint owns perception, the voice, and action. The engine
adds the background reasoner.

```sh
export OPENAI_API_KEY=...
openrealtime serve \
  -binding upstream \
  -upstream-url wss://api.openai.com/v1/realtime \
  -upstream-model gpt-realtime
```

## What it adds

A single-model Realtime server cannot reason in the background while it talks:
there is no second model and no shared log to put one on. This binding supplies
both. It mirrors the remote conversation into the canonical trajectory, runs the
engine's slow provider over it, executes the tools that provider calls, and
hands the finished answer back for the remote to say.

## Two distinctions that carry the design

**What the remote said is evidence, not authorship.** It is committed with
observer authority rather than as an assistant item, because the reasoner did
not produce that text and must not mistake it for its own prior reasoning. It
also does not open a turn: a mirrored voice that did would have the reasoner
answering the voice model instead of the person.

**The remote is told about the tools but never given authority over them.** The
same proposal-versus-execute boundary every other binding holds. Calls from the
engine's reasoner go to your client or to a server-side dispatcher; the remote
is never asked to execute one.

## The hand-off

The reasoner's answer reaches the remote as a conversation item followed by a
response request. That uses nothing but the base protocol, which is what makes
this binding portable across any Realtime-compatible endpoint rather than tied
to one vendor's internals.

## Floor

The remote's own endpointing decides turns by default. `-floor engine` is the
comparison, and it is worth running if your tasks involve spelled identifiers
or digit strings.

## What it cannot do

Video input and computer use are reported as unsupported. The base protocol
gives no way to ask a remote endpoint whether it accepts them, and reporting
honestly is better than forwarding events the remote will reject.

## Resource guidance

Nothing local runs the voice. An upstream session holds one WebSocket to the
provider and one background reasoner, so a deployment with no GPU at all can
serve it - which is the point of the binding, and why the quickstart reaches a
working session in the same number of steps either way.

What it costs instead is a credential and a round trip. The provider's latency
is the floor on every turn, and it is not something the engine can page over:
budget for the network rather than for capacity. The reasoner is the only
component you choose the placement of, and hosting it near the provider rather
than near the client is usually the wrong instinct - it is not on the voice
path.
