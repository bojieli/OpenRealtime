# Glossary

**Backchannel** — A short listener signal such as acknowledgement that does not
claim the main speaking turn.

**Capture time** — Monotonic media-ingress time associated with an observation.
It is distinct from the later time at which a component emits an event.

**Commit horizon** — The boundary between replaceable planned output and audio
already played to the user.

**Endpointed cascade (B0)** — A pipeline that waits for detected end of user
speech before final perception, response generation, and synthesis.

**Fast path** — Deadline-bounded foreground decisions for listening,
acknowledging, yielding, routing, or short low-risk answers.

**Incremental hypothesis** — A perception result with a stable prefix, an
unstable suffix, timestamps, uncertainty, and a monotonic revision identity.

**Microturn** — A bounded opportunity to update belief or act while an
interaction is still unfolding. It is a scheduler event, not necessarily an
audible utterance.

**Native speech system (N1)** — A model that consumes and generates audio in
one realtime interaction path without requiring a text-only component boundary.

**Perceived responsiveness** — User-observed progress and appropriateness,
which includes but is not reducible to first-audio latency.

**Response candidate** — A replaceable semantic action derived from a named
evidence revision and guarded by confidence, risk, and validity conditions.

**Slow path** — Cancellable asynchronous deliberation, retrieval, planning, or
tool work whose result is rejected when its goal or revision is stale.

**Stable prefix** — The portion of an incremental hypothesis declared unlikely
to change under a stated provider contract. It is evidence, not committed
truth.

**Trace** — An append-only, schema-versioned, causally linked event stream used
as the source of truth for replay, analysis, and visualization.

**Turn gap** — Time between the measured end of one speaker’s contribution and
the start of the next relevant semantic audio. Negative values represent
overlap.
