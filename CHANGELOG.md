# Changelog

## Unreleased

### Highlights

- Historical candidate-source receipts reopen after runtime status became
  sparse. The archive verifier recognizes the former status encoding in
  completions and review contexts while enforcing the original file hashes,
  strict schemas, and canonical bytes. All 498 retained FDB recordings and
  advisory evaluations reopen with the current binary.
- FDB overlap evaluation now separates completed recordings from applicable
  behavior in scores, sealed reviews, comparisons, and release acceptance.
  Inapplicable recordings cannot earn passes. The retained 498-recording
  floor translates to 287 passes and 430 applicable cases, with per-case
  exposure requirements that prevent silence from improving acceptance.
  Historical artifacts and receipts remain unchanged.
- Interaction scenario scoring now rejects word-fragment false passes and
  undefined checks, and requires every appointment detail in the translation
  case. Scorer version 3 also requires recorded acoustic continuation across
  both acknowledgements, and retains activity and pause measurements beside
  the media. Speaking only before the backchannels or emitting silence cannot
  pass. Historical recordings retain their original scores and receipts.
- The descriptor-locked browser and native companion clients now share one
  clean Realtime server and explicit presentation APIs.
- Voice + vision deployments gain a bounded silent visual-action lane while the
  conversational model remains proposal-only.
- Fast/slow cognition, interruption, holding speech, and parallel event-loop
  work now preserve one open response and one canonical trajectory.
- Compatibility coverage includes the published OpenAI Realtime SDK over both
  WebSocket and WebRTC.
- Benchmark and review paths now fail closed on incomplete, mislabeled, or
  type-inconsistent evidence.
- Provider catalogs cover language, recognition, synthesis, and remote
  Realtime roles with live probing for stale defaults.

<details>
<summary>Detailed engineering record</summary>

### Developer experience

- **The examples page describes the example that exists.** It sent readers to
  `go run ./examples/v1/reference`, a program retired with the legacy
  experiment scaffolding. It now describes the official-SDK example and where
  the key-free component fakes actually live.

- **A native macOS client carries the complete session.** The SwiftUI app sends
  PCM16 microphone audio and typed text, plays audio and renders written text,
  captures a selected display and physical camera with ScreenCaptureKit and
  AVFoundation, renders interactive HTML artifacts, exposes generated files,
  and separates observations, actions, raw protocol, and server latency into
  inspectable views. Its initial system prompt is explicit session
  configuration rather than hidden client prose.

- **Browser and desktop computer use are different bounded targets.** Browser
  mode pins browser-use 0.12.6 and consumes its actual DOM selector map,
  highlighted set-of-mark screenshot, and action paths over a JSON-lines
  subprocess. Desktop mode maps CGEvents only into one selected display and
  requires Screen Recording and Accessibility permission. Files resolve after
  symlinks inside one workspace; writes, shell commands, and consequential
  actions wait for native confirmation.

- **The optional debug stream explains where milliseconds went.** A session
  can opt into timestamped, correlated VAD, ASR, video, cognition, policy, TTS,
  tool, session, and error events. Payloads are redacted unless separately
  requested, and raw media is never included. Both developer clients render a
  searchable timeline with duration distributions; ordinary Realtime sessions
  see no new event.

### Computer use

- **The source field names the sources.** A live run produced source
  `"video browser"` - the observer name and the source name run together, taken
  from an observation that had honestly reported both. That looks like a model
  failure and is a schema failure: every action's source field said "the
  declared video source this action targets" and nothing anywhere said what the
  declared video sources were called. `DefinitionsFor` narrows the field to an
  enum of the target's own sources; `Definitions` stays target-free, because
  that is the form published with the specification. Nothing changes on the
  wire, and with the enum in place the same model named `"browser"` and the
  click landed.

### Composable presentation

- **Presentation is a replaceable client composition, not server UI.** The
  standalone host mounts descriptor-locked browser profiles over explicit
  public Realtime and management endpoints. The host owns upstream endpoint
  selection and credentials; the browser manifest contains only relative
  public routes and exact module identities.

- **Browser behavior is assembled from independently declared modules.** Text,
  WebSocket/WebRTC transport, media, tools, effects, artifacts, inspection,
  authoring, and views are selected through client contracts and scoped
  permissions. Callers can add a source-digested module through the same clean
  composition API without acquiring implicit effect authority.

- **The duplicate presentation forks are retired with stronger coverage in
  place.** A real-Chromium gate mounts the public descriptor-locked host and
  client, hides the upstream endpoint and credential, negotiates a caller
  module's function tool, and requires the model to return an unpredictable
  value available only through that tool. Its provisioned real-model form is a
  release-matrix gate; the hermetic peer exercises the same browser and public
  relay path without making a behavioral provider claim.

### Measurement

- **What the fast/slow arrangement costs a tool call, on sixteen tasks.**
  Recorded as a smoke observation rather than a cell. `endpointed-slow-only`
  attempts a call on fifteen of sixteen FDB v3 tasks; `fast+slow` attempts four
  to seven depending on prompt and policy. The reasoner runs only when the voice
  hands the turn on, so most missing calls are a small model judging that it can
  finish the request itself rather than the reasoner failing to choose a tool.
  F2 therefore has to report escalation rate beside pass rate, or a paired
  design will attribute the difference to deliberation quality.

### The event loop

- **A parallel branch no longer waits behind deferred routine work.** The
  deferred set was merged flat before it ran, a merged batch carries one
  triage, and parallel does not survive being mixed — correctly, because
  routine work must keep the deferral that is holding it. Collapsing the two
  answers meant the branch that exists to answer a question *during* long work
  arrived after that work had finished, which is the one moment it has nothing
  left to do. The set is partitioned instead: routine events keep their
  deferral, parallel ones stop inheriting it.

### The protocol surface

- **Sidecar protocol version 4 has a document.** It is the version the
  graph-native runtime speaks - the locked omni, duplex, and upstream
  external-model references dial it and `elements/model` mounts it - and it
  was described only inside the implementation tracker. The reference now
  covers the descriptor-backed handshake, attested readiness, per-port wire
  and media negotiation, element frames, the limits, the Python base class,
  mounting, and the fifteen-check conformance suite. The version table in the
  v1 document and the `serve -sidecar-protocol` help say which versions the
  legacy presets negotiate and that version 4 is reached through a launch
  profile instead.

- **An agent that is thinking keeps its turn open.** The reasoning phase is
  silent by construction, so a turn that needs it goes quiet for as long as the
  question is hard — and nothing said whether work was owed or the conversation
  had simply ended. Deliberation now runs inside the response, so
  `response.created` without `response.done` is the signal, and a test fails if
  a future change moves it back outside. Measured on a real recording: a
  4.44-second response carrying 3.43 seconds of audio and a tool call, with the
  next response opening in the same instant the first closed — no window where
  the agent owes work and the wire is silent about it.

### The benchmark harness

- **A synthesised tool schema now agrees with the values it is scored against.**
  FDB v3 declares its tools from the dataset, and declared every parameter a
  string. The suite expects numbers for prices and quantities, so a model that
  obeyed the schema and sent `"200"` was marked wrong against `200` — which
  measures whether a model will disobey the harness, not whether it understood
  the caller. The declared type now follows the expected value.

- **Quiet is not completion while the agent still owes a response.** A
  conversation ended after three seconds without events, which is a reasonable
  test for a system that talks continuously and the wrong one for this system:
  the reasoning phase is silent by construction, so a turn that needs it goes
  quiet for exactly as long as the question is hard. The driver was scoring the
  agent on whatever it finished before a stopwatch. It now treats an open
  response as work still owed and applies a separate, longer bound to it, so a
  server that opens a response and never closes it still fails rather than
  hanging.

### Cognition

- **The voice no longer over-claims a tool result.** A tool returning
  `{"status":"ok"}` became "your order is on its way and scheduled for
  delivery" — a delivery claim, a routing claim and a schedule claim, none of
  which the tool said. An empty result became "being processed and will be
  shipped soon". This is where a caller is most likely to be misled and least
  likely to notice, because the work really was done and so the sentence sounds
  authoritative. There is now a boundary in `evals` that catches it in 150 ms.
- **The completion marker is no longer the voice's problem.** It decided
  nothing once an observation began deliberating, yet four paragraphs of the
  voice's instruction explained it. Removing them fixed the over-claiming
  outright on the local model — the rules that matter had been crowded out.
  Instruction length is not free, and a mechanism that does nothing is not
  free either. `StripMarkers` still runs, so a model that emits one out of
  habit does not say it aloud.
- **A holding turn the answer overtakes is not a failure.** The reasoner
  finishing cancels the turn's context, and a holding continuation still being
  produced reports that cancellation — which reached the client as a session
  error for a silence that had just been filled properly. It is the outcome the
  mechanism hopes for: the gap was covered, and then it stopped being a gap.
- **An agent that goes quiet while it reasons says so.** The reasoning half
  never speaks, so a question that needs it produces a silence whose length is
  a property of the question — and a caller cannot tell that from a broken
  agent. After `-holding-after` the voice says one short sentence, once, in the
  turn already in progress: not a second turn, because the base protocol has
  one active response and the turn that started the deliberation is still open.
  It costs the reasoner nothing, now that an assistant turn is no longer
  treated as evidence that invalidates it.
- **An item is stamped as it enters the log, not as it was produced.** A
  continuation is asked at one moment and commits at another, and with anything
  running beside it those interleave: a reasoner's instruction item carried a
  time from before everything the voice had said since, and the append was
  refused as "monotonic time moved backwards". The log is append-only and its
  times exist to agree with its order, so entry time is the honest one —
  production order is already recorded by the order of the items.
- **The voice talking no longer discards the reasoning.** A continuation
  committed against the trajectory version it started from, so anything
  appended while it was thinking threw away everything it had produced —
  including its tool calls. `ErrStalePrefix` says such output "must be
  recomputed from the new prefix"; nothing anywhere recomputed it. A version
  number cannot tell "the person said something else" from "the agent filled a
  silence", and only the first invalidates what the reasoner relied on.
  Staleness is now about evidence: a new observation, a repair obligation or a
  tool result supersedes a continuation; the agent's own speech does not.
- **The sidecar hand-off states the result instead of dictating it.** `omni`,
  `duplex` and `upstream` told the model "say this, preserving every fact and
  identifier exactly, and add nothing" — the design the cascade shipped and
  withdrew, because a phase told to say what another provider wrote performs it
  rather than speaking, and when it loses the referent it reads its own last
  turn back instead. The result is now handed over as something the model knows,
  to be told in its own words, which is the contract the cascade voice already
  runs under.
- **An observation deliberates.** Whether the background reasoner ran was
  decided by the voice declining to mark its turn complete — which put every
  capability the agent has behind one judgement by the only phase that cannot
  act on it, and the judgement is model-dependent in a way nothing measured.
  On sixteen FDB v3 tool-using turns the reasoner ran on four to seven of them;
  it now runs on all sixteen, correct calls went from two to ten, and turns
  with no call at all went from twelve to zero. The other three bindings never
  took this risk: `omni`, `duplex` and `upstream` have always deliberated at
  the endpoint.
- **The marker still means something, and something the voice can judge.** It
  says the voice has finished speaking, not that the work is finished. Whether
  anything remains to be done is decided by looking: a slow continuation with
  no tool call and nothing to add returns silently, so a turn that needed
  nothing costs one call and says nothing.

### Perception

- **"I heard nothing" is part of the perception contract.** `PerceptionRevision`
  had no documentation at all, so what an empty or punctuation-only transcript
  meant was left to each consumer to guess, and one guessed that it was
  something the user said. The rule now sits on the type, with the predicate
  beside it: every adapter reports no-words the same way and every consumer
  reads it the same way.
- **The five silence thresholds are written down together.** They live in three
  packages, each documented where the others are not, and one silently overrode
  another until it was measured. Operations now states the order they compose
  in and the longest a turn can stay open on silence alone.

- **Room tone no longer becomes a turn.** The acoustic gate opened on any block
  above its threshold, so anything percussive started an utterance, and a
  recogniser asked what was in it answered "." — which the only content check,
  `TrimSpace(text) == ""`, accepted as something the user said. On one FDB v3
  recording seven of nine observations were that. The gate now wants sustained
  voicing, and an observation wants a letter or a digit in it.
- **A pause inside a sentence is no longer the end of a turn.** The gate's
  silence threshold closed the utterance and that closing was taken as the
  endpoint, so a disfluent request arrived as five turns. The floor decides
  now, which is the question it already owned.
- **Turn projection ran for the first time.** A reasoning model asked for one
  enumerated word spent its budget on `<think>` and never answered; the empty
  answer scored as unknown, and unknown fell below the confidence threshold. It
  is told not to think, in whichever of the five spellings its endpoint takes.
- **The default recogniser refuses what it cannot honour.** `-asr-language`,
  `-asr-keyterms`, and `-asr-endpointing` were accepted for `qwen-asr` and
  dropped on the floor: the local service detects the language itself, has no
  vocabulary hints, and leaves the endpoint to the engine's own gate. A
  deployment could therefore believe it had configured recognition it had not.
  The provider layer now refuses each of them by name, so every composer gets
  the same answer; `serve` stops forwarding the Deepgram-only endpointing
  default to a recogniser that never read it, and the shared legacy
  `-language` hint, which also configures synthesis, no longer reaches a
  recogniser that detects the language for itself.

### Turn-taking

- **A policy model may report that it does not know how sure it was, and an
  unknown confidence is no longer read as a low one.** `confidenceOf` returned
  `0.5` for "no log probabilities came back", which compares like a number: the
  projection threshold is `0.7`, so every unmeasured answer was silently
  discarded. Combined with a reasoning model that never reached its choice at
  all, turn projection had never fired in either direction.
- **The two projection answers no longer share a threshold.** Ending a turn
  early cuts a person off mid-sentence; holding one open costs latency that
  `-projection-hold` already bounds. The code said so in a comment and then
  gated both on one value, which spends the cheap failure to avoid the
  expensive one.
- **A projection the floor never asks is a configuration error.** Turn
  projection reaches the conversation only through the floor, and the policy
  set could name a model projection while its floor consulted none: the set
  reported `model:…`, flipped the evidence capabilities, and projected nothing.
  `serve` has always rebuilt the floor when it installs one; every other
  composer had to remember to. `Policies.Validate` now compares the projection
  the floor consults with the one the set reports and refuses a disagreement,
  so the silent no-op cannot be composed.

### The release gate

- **The reference sidecars are run, not only shipped.** Qwen3-Omni, MiniCPM-o,
  and Moshi each accept `--mock` so their plumbing can be verified without a
  model, and nothing ran them: the Python suite covers the framing library and
  the Qwen adapter's parsing, and the Go suite drove a Go echo sidecar. A
  reference sidecar could stop speaking the protocol it ships for and every
  gate would stay green. `TestReferenceSidecarsPassConformanceInMockMode` now
  runs each as the engine would, at version 1 and at the highest version it
  declares, and both CI gate jobs install the numpy and Pillow it needs.
- **Efficiency is a gate in the matrix, not only a page.** The efficiency
  numbers were called release gates and nothing ran the command that produces
  them. `local.efficiency` runs it for ten seconds of simulated video and
  retains the JSON report, failing closed if the report is not written or does
  not reach the audio gate; it compares no thresholds, because the claim is
  that every number is measured and stated against its machine.
- **Three suites have acceptance targets.** The behavioral acceptance gate was
  blocked on every suite but the scenario aggregate, because no floor had been
  registered. Realtime-CU, the cascade Meeting Assistant, and FDB v1.5 now
  carry registered aggregate, per-case, safety, deadline, and latency targets
  derived from their retained, independently reopened complete runs and cited
  to them by artifact, revision, and executable digest. They are non-regression
  floors - Realtime-CU's aggregate is 8 of 16 and FDB v1.5's interruption
  yield median is 2.5 seconds, which is where the runtime is rather than where
  it should be - and a cross-check confirmed each floor accepts the run it came
  from. FD-Bench, FDB v3, and both τ-Voice conditions stay unavailable, each
  now saying exactly why there is nothing to derive a floor from.
- **A missing tool is a failure in a release run, everywhere.** The official
  client, portable client, and Python sidecar stages of `check.sh` already
  refused to report a claim they had not checked; the Go tests that need
  node, Chromium, ffmpeg, ffprobe, bubblewrap, python3, or git still skipped
  under `OPENREALTIME_RELEASE_GATE`, and `go test` printed ok for each. Every
  such test now goes through `internal/testgate`: an ordinary run skips and
  names the tool, a release run fails, and the helper's own subprocess test
  proves both. Provisioned inputs that no gate installs - the pinned FDB v3
  dataset, the local SenseVoice runtime - keep skipping, but say
  `NOT VERIFIED` in one wording so a recorded skip reads as an unchecked
  claim. The companion command gate also stops reading the variable as
  exactly `1` while the script reads it as non-empty.

- **The official-SDK check no longer races its own handshake.** It waited for
  `session.created` and then asserted `session.updated` had also arrived, which
  is true whenever the machine is idle and false under load — so the one test
  standing behind the compatibility claim failed four gate runs this week for
  reasons that had nothing to do with the server. It waits for both, and for
  the refusal that travels with them.

- **A turn-projection model can now say the person has *not* finished.** It was
  asked a two-sided question — continuing or finished — with a prompt tuned to
  spot exactly the mid-thought pause, and only the "finished" half reached the
  decision. The "continuing" half was computed and dropped, so silence
  endpointed the pause anyway and a disfluent sentence became several turns,
  each answered separately. A projected pause now holds the endpoint open, for
  at most `-projection-hold` past the silence threshold: a floor that can be
  talked out of ending is not a floor, and a model that keeps answering
  "continuing" would hold one forever. The prompt states both costs, because a
  model told only that cutting people off is bad will never say "finished".

### Cognition

- **A hallucinated tool name no longer ends the session.** An undeclared name
  from a provider with execution authority failed the whole continuation, and
  through it the conversation. Two of sixteen FDB v1.5 recordings died this way
  in one run — once on `google_calendar.list_events?` with the question mark
  attached, once on a sentence of instructions emitted as a function name.
  Neither could ever have executed. Both are now recorded as non-executable
  proposals, exactly as a fast provider's calls already were, and the
  dispatcher re-checks at the point of effect as it always did. A wrong name is
  what a tool error is for; the slow phase is already told a tool error is
  authoritative.
- **The completion marker means the request is satisfied, not that the
  sentence is.** The voice writes it to say deliberation is not needed, and a
  turn that promised to look something up reads as complete when the sentence
  ends. It is now told the test out loud: ask what the user still does not
  have, and if the answer is anything, do not write it — a promise with the
  marker after it is a promise nothing will keep.
- **Spoken identifiers are reassembled, not guessed.** A recogniser writes an
  order number the way it was said, so "A-B-C-one-two-three" arrives as
  "AB, C,1,2,3". The reasoner is told to remove only the separators the
  recogniser introduced and to write spoken digits as digits, rather than
  inventing a plausible identifier.
- **A holding line is said once.** The voice is told never to leave dead air,
  and on a fragmented turn it obeyed several times over — "one moment please",
  then "let me check", then asking again for an identifier the user had already
  given. It is now told that the second "one moment" is worse than a short
  pause, because it sounds like the agent has lost track.

### Operations

- **A session can no longer be held open forever by a client that stopped
  reading.** The write used the session context, which ends only when the
  session does, so a client whose receive window closed blocked the write with
  nothing left to end it: the writer stopped draining, the send buffer filled,
  the handler blocked, and the read loop blocked handing it the next event. The
  session was wedged for the life of the process, holding a binding runtime and
  its provider connections, while every health check reported a healthy server.
  `-write-timeout` bounds one send. Separately, nothing told the server its peer
  was gone — the library answers a client's pings, and a peer that vanishes
  without a FIN leaves a connection open on this side only, which no write
  discovers in a session where neither side is speaking, because the HTTP
  server's `IdleTimeout` stops applying at the upgrade. `-keepalive-interval`
  pings after an idle interval. Both causes are named in the log, because "the
  peer stopped reading" and "the peer stopped answering" reach an operator as
  the same symptom and have different fixes.
- **A stalled socket no longer stops a conversation with no error.** The same
  unbounded-write defect existed on three surfaces besides the gateway. The
  presentation effect socket wrote on the session context under one mutex, so
  a browser that stopped reading blocked not just its own write but every
  write to that session, and kept one of the host's bounded slots for the life
  of the process. The Deepgram listener bounded its dial and its drain and not
  the send between them — the one call on the recogniser's hot path, made every
  cadence while someone speaks, holding the listener's lock, on a context that
  comes from the session and has no deadline. The Gemini Live client had the
  same gap on the foreground voice. All three are bounded now, each by its own
  cadence rather than a shared number, and `WriteTimeout` on the two adapters
  is configurable. The shared Realtime client is the fourth: `Send` takes a
  context, which is the right shape for a library, but the callers that matter
  — the `upstream` binding forwarding a caller's audio to a remote endpoint,
  and the WebRTC adapter bridging a peer — hold a session-scoped socket and
  pass the session's context, which has no deadline. Every one of these fails
  the same way: not slowly, but silently and forever, because the write holds
  a lock that every later write needs.

  Six sockets in all, which is every WebSocket write in the tree that was not
  already bounded: the gateway's, the presentation effect socket, the
  presentation relay's own data path — where one stalled browser wedged both
  directions, because the goroutine that blocks writing is the goroutine that
  stops reading — the Deepgram listener, the Gemini Live client, the shared
  Realtime client, and the LiveKit agent's link to the endpoint. Each is
  bounded by what it carries: five seconds for a frame of audio to one
  provider, thirty for a whole Realtime event to a server.
- **A gateway can be told how many sessions it will take.** Admission checked
  only whether the gateway was closing, and past that point a session holds a
  runtime and its provider connections — so an unbounded gateway does not
  degrade under load, it exhausts the process and takes every established
  session with it. `-max-sessions` turns that into a 503 with `Retry-After`.
  It defaults to unbounded, because capping an existing deployment at a number
  chosen here would be a worse surprise than the exhaustion it prevents;
  `/metrics` now reports `sessions_in_flight` and `sessions_rejected` so the
  number can be chosen from evidence rather than guessed.
- **Connecting costs 34 ms less and 1.6 MiB less.** Every session compiled the
  pinned 370 KiB Realtime schema bundle into its own copy of 133 schemas before
  it could check its first event. Correct, identical for every session, and
  invisible to every test — the cost was connect latency and resident memory,
  which at a thousand concurrent sessions is a third of a CPU-second and
  1.6 GiB of duplicated immutable data. One process-wide bundle now serves them
  all.
- **Live providers share one warm connection pool.** Every adapter built its
  own `http.Client`, which inherits a two-connection idle pool per host: right
  for a program that talks to many hosts occasionally, wrong for a runtime that
  calls one recogniser several times a second across every session, where the
  third concurrent call pays a redial and a TLS handshake on the path a person
  is waiting on. There is still no shared request deadline, because a
  recogniser answering in tens of milliseconds and a reasoner answering in tens
  of seconds cannot share one that means anything.
- **The dependencies carried thirty-one advisories this code reached.** Both
  modules pinned a Go 1.25.0 toolchain and `golang.org/x` releases that predated
  their fixes; every advisory had been fixed upstream, some for months, and
  nothing in the repository was arranged to notice. Both now declare the exact
  patched toolchain, which is also what makes the release build reproducible
  rather than reproducible-per-machine. CI runs `govulncheck` on every change,
  Dependabot proposes the updates, and the container image is built on every
  change like the release binaries already were.
- **A policy model with no partial transcript measures as no effect.**
  Backchannel and turn projection answer a question about the partial
  transcript, and a batch recogniser asked for nothing before the endpoint
  produces none — so enabling them against one changed nothing, which reads as
  the policy models not working rather than as their evidence being absent.
  `-policy-models` and `-asr-partial-interval` are documented together now,
  because they are one decision. Making partials affordable is what made the
  policies work: on one τ-Voice simulation, agent interruptions fell from 107
  to 36 for about 200 ms of response latency.
- **A local recogniser that long conversations can afford.** `sensevoice`
  serves SenseVoiceSmall on the OpenAI transcription route, with the server and
  a preparation script in `deploy/sensevoice`. The property that matters is
  that it is non-autoregressive: it emits the whole transcript in one forward
  pass, so recognising a growing utterance costs what the audio costs rather
  than what the transcript costs. An autoregressive recogniser asked the same
  question repeatedly gets dearer every time, until it stops keeping up with
  the audio arriving and the advance bound fails the session — correctly, for a
  reason that looks like an engine defect. Measured here at a real-time factor
  near 0.008, about 26 ms an utterance.
- **A recogniser getting slower is visible before it stops.** `asrbuffer`
  measured `provider_elapsed_ns` and `provider_max_elapsed_ns` per utterance
  and `Buffer.ProviderRuntimeMetrics()` returned them "for process-level
  aggregation", but the aggregation went out with the legacy scaffolding and
  nothing called it, so the only observable signal was the failure. Buffers now
  fold into a process-level accumulator as they close and `/healthz` reports
  it. An utterance still open is read in place rather than waited for: the one
  most likely to be slow is the one that has not finished, and a total that
  only moved at the endpoint would go quiet during the stall it exists to
  report. The mean is reported next to the maximum because a maximum jumps once
  on one bad call and never comes down.
- **A session that ends mid-utterance releases its recogniser.** One recogniser
  exists per utterance and the endpoint retires it, but a session closing while
  the user was still speaking never reaches an endpoint — and hanging up
  mid-sentence is ordinary behaviour. The observer's own documentation already
  claimed this path; nothing called it, so the socket and the goroutine reading
  it outlived the session.

### Cognition

- **A question the voice can answer is answered once.** The rollout ran the
  background reasoner on every observation, so a turn needing no deliberation
  produced a second answer nobody asked for — and the step that read it back
  spoke it. Whether a turn needs deliberation is now the fast phase's
  judgement, handed on with a control marker that is stripped before any item
  is committed, so it reaches neither the trajectory nor the user.
- **Slow's result is background state, not an assistant turn.** Every item
  records whether its producer could be heard, and the projection into a
  provider discarded that, so a written result arrived as an ordinary assistant
  message — indistinguishable from what the agent had actually said. A voicing
  step told to "say the answer the reasoning continuation just produced" had no
  referent for it and recited the earlier spoken turn back instead, word for
  word, including the truncation from its own token limit. It is now projected
  as what it is, in every dialect.
- **The voicing step is gone.** Nothing is asked to recite what another
  provider wrote. The voice reads the same trajectory and answers in its own
  words, which is the only kind of spoken turn there is.
- **The fast provider is told what the agent can do and given nothing to
  execute.** It was handed the full tool definitions while its authority was to
  propose, so it spent a ninety-six token budget emitting JSON that could not
  run — and left the dead air its own instruction forbids. It gets the
  capability list and the escalation marker instead. A call it emits anyway is
  still recorded as a non-executable proposal rather than failing the turn.

### The event loop

- **Every completion re-enters the loop.** Locally dispatched tool results were
  appended straight to the trajectory, and a finished slow chain was acted on
  where it happened, so neither passed the gate that decides when the agent may
  be heard. Both now travel as events, exactly as a client-executed result
  already did. A signal event opens a safe point without appending anything,
  because what it refers to is already in the log.
- **Deliberation is never deferred for silence.** The gate holds what will be
  heard; it must not hold what will only be thought, or the background reasoner
  could not reason while the voice is talking, which is what it is for.
- **A superseded turn is a cancellation, not a failure.** Two of the three
  interrupt paths marked themselves as interruptions and the third did not, so
  a turn replaced by newer evidence was reported to the client as an error.

### Compatibility

- **A response is one thing the agent did, not the whole turn.** The
  compatibility document claimed a turn had to be a single response, on the
  grounds that a client stops reading a finished one. That is not what a client
  does — audio arrives on the audio channel and function calls are read as they
  arrive — and the claim would require holding a response open across an
  unbounded deliberation. Corrected, with the test that encoded it.

### Removed

- `AppendBatchAfter`, `AppendToolResults`, `ErrSlowMaySpeak`,
  `ErrFastMayExecute`, and the `ValidateArrangement` the last two named in
  their documentation: none had a caller, and the last three described a check
  that does not exist. The arrangement is enforced at engine construction.

### Realtime endpoints

- **`providers -role upstream -probe NAME` contacts an endpoint for real**,
  using the same dial path the binding uses, and prints every server event it
  sent back. A fake built from a vendor's documentation proves only that this
  code does what the documentation was read to say; the endpoint itself is the
  only thing that can prove the reading was right.
- **Each entry records how far it has been checked** — `live-turn`,
  `reachable`, or `documented` — and the listing prints it, so the table says
  what is evidence and what is a careful reading rather than leaving that in a
  paragraph.

- **Five remote realtime endpoints behind the background reasoner**, resolved
  through the catalogue: OpenAI, xAI, Azure OpenAI, Alibaba's
  Qwen-Omni-Realtime, and Google's Gemini Live. `-upstream-provider` selects
  one; `openrealtime providers -role upstream` lists them.
- **A rename table for endpoints on the pre-GA event names.** OpenAI renamed
  its audio events at general availability and several endpoints implement the
  earlier spelling. That is now a catalogue entry rather than an adapter.
- **The hand-off is a declared property of the endpoint.** Giving the reasoner's
  answer to the remote to say is what this binding is for, and the base
  protocol is not as portable as it looks: Qwen-Omni-Realtime reserves
  conversation items for tool results and takes no per-response instructions,
  so its answer travels in the session instruction and is taken back out
  afterwards.
- **Gemini Live is translated rather than forked.** BidiGenerateContent is not
  a Realtime dialect - no event type field, no conversation items, no session
  updates after the handshake, and 16 kHz audio in. `adapters/geminilive`
  speaks it on one side and the Realtime protocol on the other, so the binding
  keeps one runtime and one mirror. Verified against Google's live API, not
  only against a fake.
- Deepgram Voice Agent and ElevenLabs Agents are deliberately **not** here:
  they are agent platforms that run their own loop, and this binding's reasoner
  behind one would be two orchestrators on one conversation.

### Providers

- **One catalogue for every model provider.** The new `providers` package is
  the single place that knows how to reach a language model, a recogniser, or
  a synthesiser, and the server resolves all four roles through it. Thirty
  language providers, ten recognisers, and nine synthesisers; adding one is a
  table entry plus, when the wire format is genuinely its own, an adapter.
  See [providers](docs/providers.md).
- **`openrealtime providers`** lists the catalogue for any role and shows
  which credentials are present. `-probe NAME` asks a provider what it
  actually serves, because a default model compiled into this repository is
  the one part of an entry that goes stale.
- **An Anthropic adapter** over the native Messages API. It is not a dialect
  of Chat Completions, and three of its constraints are load-bearing: a turn
  must end with a user message, every tool call must be answered in the very
  next one, and thinking blocks must be replayed with their signatures.
- **Provider dialects for OpenAI-compatible endpoints.** The reasoning switch
  is spelled five different ways across vendors and the output-token limit two;
  both are now declared per provider rather than assumed. An effort level an
  endpoint has no word for is refused at construction instead of being answered
  at a neighbouring one.
- **Streaming recognition from Deepgram**, dialled at the caller's own sample
  rate so nothing is resampled before it is recognised, and **batch
  recognition** from OpenAI, Groq, ElevenLabs, Fireworks, SiliconFlow,
  Mistral, and any local whisper server through one adapter. A batch endpoint
  recognises once, at the endpoint of the utterance, and says so rather than
  pretending to stream.
- **Speech from Deepgram, ElevenLabs, and Cartesia**, sharing one adapter and
  one streaming reader with the endpoints that speak OpenAI's speech route.

</details>

## v1.0.0

The first release of the current architecture. The engine was rebuilt around
four subsystems over a session core — perception, cognition, action, and an
interaction control plane — and everything below is what that made possible.

### The engine

- **Four bindings, one runtime.** A cascade of recogniser, language model, and
  synthesiser; a speech-to-speech Omni model; a full-duplex interaction model;
  and a remote Realtime endpoint. The binding seam is what varies; the
  trajectory, the event loop, and the interaction policies do not.
- **A background reasoner that shares one trajectory** with the foreground
  model, reasoning and calling tools while the voice keeps talking. Two
  boundaries hold it in place: fast cognition cannot call tools, and slow
  cognition cannot speak.
- **An interaction control plane** with named, swappable policies for
  triggering, preparation, rollout, the floor, and commitment — the parts of
  feeling good in a conversation that no model in the stack knows anything
  about.
- **Duplex state derived from playout**, not from generation. Agent-speaking
  means audio reaching a person, which is the only definition barge-in can be
  built on.
- **An event loop with one invariant**: commit is unconditional, acting is
  conditional, and every deferral has a wake-up.

### The protocol

- **OpenRealtime Protocol v1**, a strict superset of OpenAI Realtime:
  three events and two object extensions, zero changes to the base surface. A
  client that never mentions it gets an ordinary Realtime session, and the
  server never volunteers the key.
- **Realtime video and computer use** over the same session, through the
  extension.
- **Wire validation on every session by default**, in production rather than
  only in tests.
- **A sidecar protocol** for model implementations that are not Go, with a
  conformance suite and reference sidecars for Qwen3-Omni, MiniCPM-o, and
  Moshi.

### Transports and integrations

- **WebRTC termination in-process** via Pion, with Opus inbound and PCMU
  outbound.
- **LiveKit** as a separate module, so a server build does not inherit its
  dependency tree.
- **A browser example** that connects to a local server with no build step.

### Safety

- **Observer authority and fenced observed content.** What a screen or a
  document says is evidence, never instruction, and the injection-authority
  test is a release gate rather than a test that happens to exist.
- **Declared targets and confirmation requirements** on the `computer.*`
  namespace, with a uniform commit boundary and an action audit.

### Measurement

- **A harness that drives a running server over the protocol**, not an
  in-process session, with fail-closed reporting: no incomplete cell is
  reportable, every cell declares its revision and executable hash, latency
  claims carry distributions, and negative results publish.
- **Five suites**: FDB v1.5, FDB v3, and FD-Bench run here; τ-Voice and
  DynaCU-Bench stay in their own environments and OpenRealtime ships a runner
  for each. Both runners point the published environment at a running server
  and patch nothing in it: those benchmarks speak the OpenAI Realtime protocol
  over a configurable base URL, and this is a strict superset of it, so the
  endpoint is an argument. What they measure is therefore the protocol claim
  rather than an adapter written to agree with us.
- **Dataset preparation** that verifies every archive against a pinned digest
  and every partition against a pinned population.

### Release engineering

- **One verification gate**, `scripts/check.sh`, which is what CI runs.
- **Reproducible cross-platform builds** with published checksums, verified by
  building twice and comparing rather than asserted in prose.
- **A distroless container** that runs as a non-root user and contains the
  server binary and the certificate roots and nothing else.

### Fixed before release

An audit against the plan found one recurring defect: policies that were
constructed, validated, named in the health report, and exposed as flags — and
never consulted at runtime. A measured factor that is a silent no-op cannot
measure anything, so each of these is now wired to the live path with a test
that fails when it is unwired.

- **The repair lifecycle.** Nothing created a repair obligation, so
  `RepairInstruction` never fired and audible repair never happened. Content
  the user heard and a later observation invalidated is now recorded in the
  ledger, raised into the trajectory at the next safe point, put to the slow
  provider as an instruction, and resolved against the correction.
- **Turn projection.** The floor policy was never consulted, so
  `-policy-models=turn-projection` changed nothing. A projected endpoint now
  closes the turn before silence confirms it, and only a projected one does —
  an ordinary endpoint stays the acoustic gate's.
- **Backchannel.** The policy was never consulted, so
  `-policy-models=backchannel` changed nothing. A chosen continuer is now
  spoken, off the audio path, carrying no assistant item, and does not trigger
  barge-in against itself.
- **Preparation.** `Prepare()` was called and its decision discarded. A
  continuation is now genuinely generated before the endpoint against an
  uncommitted observation, committed to nothing, and adopted only if the
  endpoint says the same thing. It is opt-in (`-preparation continuous`),
  because it is the one policy that spends tokens rather than reorders work.
- **Computer use could not press anything.** Every action in the namespace that
  changes something declares `confirm: policy`; with no policy supplied that
  reads as `always`, and `always` with no confirmer denies. The declared target
  is now the policy, `-computer-confirm always` is refused at startup rather
  than denying silently at dispatch, and a `Confirmer` can be supplied.
- **Observers were per deployment, not per session.** They are now selected in
  the session extension (`observers`, answered with `available_observers`), and
  factor F3's video-only level genuinely runs no recogniser instead of being
  audio+video under another name.
- **The declared video rate cap was not enforced**, and the video observer's
  sampling interval keyed on client-supplied capture times — so a client that
  sent no timestamps had no rate limit at all. Both now hold server-side.
- **The admission governor was never constructed.** Policy models, narration,
  and speculative preparation now compete under one budget when
  `-compute-capacity` is set.
- **Deferred routine work could ride out on a parallel batch** and bypass the
  deferral gate. A merged batch is parallel only when every part of it is.
- **Two policies that cancel out are refused rather than silently reconciled**:
  `stable-partial` observation with a deferral that waits for the endpoint held
  every partial until the endpoint and bought nothing.

Two defects in the measurement harness itself, which are the more serious kind
because they would have produced published numbers that were wrong:

- **FDB v3 scored an unreassembled identifier as correct.** Value comparison
  stripped whitespace, so an agent that heard the caller spell it out and sent
  `order_id="B O B 1 2"` matched the expected `BOB12`. Reassembling a spelled
  identifier is the distinct failure this suite exists to separate from not
  knowing which tool to call, and the scorer was reporting it as absent.
  Punctuation is still normalised away; a word boundary is now a word boundary.
- **FD-Bench computed the premature/overrun distinction and did not report
  it.** An answer begun mid-turn and an answer that ran into the next turn have
  different causes and different fixes; both are now in the metrics, with the
  overlap duration.

### What running the benchmarks unmodified found

DynaCU-Bench drives 150 real browser tasks with an unmodified official-API
client, which exercises paths no test written here had thought to. Five
compatibility defects came out of pointing it at the server, and each is now a
capability rather than a workaround:

- **A response was not a turn.** Every output kind was rendered as its own
  response, so an agent that spoke and then called a tool produced two of them
  and a client that stops reading at the first `response.done` — which the
  protocol says it may — never saw the calls. One `response.create` now
  produces one response carrying every output item, closing when the rollout
  has finished *and* every utterance it started has finished playing.
- **Text output was refused.** `output_modalities: ["text"]` failed the session
  update, so the whole class of clients the video and computer-use extension
  exists for could not open a session at all.
- **Turn detection could only be the server's.** A client sending
  `turn_detection: null` had it ignored and `input_audio_buffer.commit`
  refused, so a client driving its own turns could not work here.
- **An image attached to a turn was dropped**, and the cognition engine had no
  media resolver, so even retained media was invisible to every provider — an
  agent handed a screen acted on one it had never seen.
- **Vision was not a declared property.** A text-only fast model was handed the
  screenshots and returned "is not a multimodal model" on every step.

### What is not claimed

The measurement program in `docs/measurement.md` runs continuously after
launch, and its claims appear as each cell completes. Until a cell is complete
there is no score for it, which is a deliberate trade: the project ships before
its most interesting claims are provable. Human preference and perceived
naturalness are not measurable by any of this and need a separate study.
