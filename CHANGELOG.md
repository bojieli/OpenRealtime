# Changelog

## Unreleased

- The strict launch profile is a Linux-only path, and nothing said so. A profile
  file is read back through a hardened open - refusing symlinks, hard links,
  special files, and any identity change between lookup and read - implemented
  for Linux and stubbed everywhere else, so on macOS `serve` stops with "secure
  launch-profile file opening is unsupported on this platform" before it
  listens. That covers the default `companion` room, `-client macos`, and any
  `-launch-profile` an operator authors, which is to say the README's first
  command and the quickstart's native-client command both failed on the
  platform whose client they were introducing. The bound is now stated in the
  README, the quickstart, and the room guide, next to the commands it governs,
  and the macOS commands name the explicit cascade composition that does work
  there. The limitation itself is unchanged; it is only no longer silent.

- Defaulting the companion room to the twelve-scenario pipeline made the strict
  launch profile supersede provider selection, and nothing that depended on the
  older flag form was updated with it. `serve` refuses to start rather than
  ignore a flag the profile overrides, so `macos/verify-hosted-companion.sh`
  died at startup with "-launch-profile supersedes flags -slow-model,
  -slow-provider", and seven documented commands in the local-stack guide were
  refused the same way. Both landed the same day as the macOS compile break, so
  the compile failure had masked the script from its first run.

  The script's two flags named a stand-in reasoner before the profile existed;
  the profile encodes that now, so they are removed rather than translated, and
  placeholder provider credentials let it reach readiness on a machine with no
  provider account - it drives supervision and routing and never reaches one.

  The guide's commands name `-binding cascade`, which is what says "compose the
  cascade this page describes, from these flags" rather than the room pipeline.
  That is a one-token addition that keeps the documented flag form working, and
  the guide now explains the rule once instead of leaving each command to fail
  at the reader.

- Repository hygiene before publication: a vim swap file for a docs page had
  been committed and carried the author's home directory and hostname in the
  tree; `LICENSES.md` granted licenses to four paths that no longer exist
  (`PLAN.md`, `tests/fixtures/`, `tests/golden/`, `benchmarks/*/reference/`)
  while the fixtures that do exist were named nowhere; and one test's example
  HTTPS proxy used a real routable address instead of the documentation range
  the rest of the repository already uses. Issue and pull request templates now
  ask for what `CONTRIBUTING.md` says a report needs.

- CI had been red on `main` for three weeks - thirty-eight of the last forty
  runs - and not one of the failures was a defect in the code the jobs were
  checking. `./scripts/check.sh` passed locally, complete and with nothing
  skipped, throughout.

  Most of it was one mistake. `actions/setup-go` pins `GOTOOLCHAIN=local` so
  that its `go-version` input is the only toolchain a job may use, and this
  repository deliberately does the opposite: each module names the exact
  `toolchain` it is built with, the root module `go1.25.14` and
  `integrations/livekit` `go1.26.8`, and `check.sh` runs both with one `go`
  binary. Under `GOTOOLCHAIN=local` the LiveKit module could not be loaded at
  all - `go.mod requires go >= 1.26 (running go 1.25.14)` - so its vet, its
  tests, and its race tests were reported as three failures without one of them
  executing, in the same red as tests that genuinely fail. The gate,
  compatibility, and release-matrix jobs now restore `auto` after setup-go,
  which hands each module the toolchain its own `go.mod` pins and is what a
  contributor running the gate locally already gets.

  The same pin had silently disabled the vulnerability scan outright.
  govulncheck now requires Go 1.26 to build, so `go install` inside the 1.25
  half failed in a third of a second, before a single package was loaded, and
  the job reported "could not look" in the same red as "found advisories" -
  the one confusion it exists to prevent. The scanner is now built once with
  the newer toolchain and used for both scans; each module is still scanned
  under its own pinned toolchain, so the standard library each result describes
  is still the one that module's build uses. Both modules currently report zero
  reachable advisories.

- The native macOS client did not compile on the runner that builds it. The
  package's deployment target is macOS 14 and stays there - `RoomRecorder` is
  marked `@available(macOS 15.0, *)` and every call site guards with `if
  #available`, so a macOS 14 machine runs the app and simply has no recorder -
  but `@available` is a runtime check, and the macOS 14 SDK has no
  `SCRecordingOutput` symbol to compile against. Every run since the recorder
  landed failed with twenty "cannot find type" errors against correct code. The
  job now runs on `macos-15`, which is the SDK `macos/README.md` has asked of a
  contributor all along: macOS 14+, Xcode 16+.

- The public companion command's end-to-end gate had never passed since it was
  written. It polled `/metrics` without the bearer token the companion runs
  with, collected 401s until its deadline, and then reported that the browser
  and native sessions had not completed - naming the wrong half of the system,
  since by then both had run. It also required a Deepgram and a Gemini
  credential to reach readiness, on a runner that holds neither. The poll now
  carries the credential; the credentials are supplied as placeholders by the
  test itself, which is honest because this test never reaches a provider - it
  asserts on supervision, routing, and session accounting, recognises no audio
  and requests no reasoning - and which also shadows a real key that happens to
  be exported, so no developer's account is dialled by a test run. Its failure
  message now reports the counts it saw rather than only the counts it wanted.

- Two tests failed under CI's capacity rather than for anything they check. The
  mounted-graph test read the trace the instant the egress envelope arrived,
  racing the runtime goroutines still recording that crossing, and saw three
  events where four were due; it now waits for the count, with the same
  assertion after the wait, so a fourth record that never arrives still fails.
  The Chromium inspection test held a 30-second bound where every other
  Chromium launch in its package holds 90, and died at 30.14s with the deadline
  killing the browser mid-render; it now matches its siblings. Neither change
  alters what is asserted.

- Two opt-in pre-ASR audio filters, `profile filtered-room` and `profile
  target-room`, place a waveform transform ahead of both energy admission and
  ASR. The ordering is the whole point, and a parallel evidence lane cannot
  provide it: the filter completes before admission sees a frame, so neither an
  acoustic interruption nor a partial transcript can be caused by the
  unfiltered version of that frame, and there is no raw-audio path around the
  filter. The filter enters as a locked graph node with its own lock entry
  rather than as a setting, so a profile that names no filter URL still builds
  a byte-exact topology, values document, and lock - `filtered-room` and
  `target-room` are the only graphs that change.

  `filtered-room` selects RNNoise, which suppresses stationary noise and does
  not remove an overlapping speaker. `target-room` selects target-speaker
  extraction from an initial three-second reference and is **experimental, not
  validated for live use**: the end-to-end diagnostics recorded in
  `tools/targetvoice/E2E_RESULTS.md` still show competing-speech transcription
  leakage, and a playback diagnostic still observed one false interruption, so
  the extractor is not yet a reliable target-presence gate.

- The LiveKit half of the vulnerability scan could not run, and had not run for
  days. `integrations/livekit` declares `go 1.26` because its LiveKit
  dependencies now require it, while the job installed one govulncheck with Go
  1.25 and used it for both modules. govulncheck can only load packages up to
  the language version of the Go it was built with, so the second scan died
  with a loader error - `package requires newer Go version go1.26` - rather
  than reporting anything about advisories. The root-module scan passed the
  whole time, so the job's failure said nothing about what it had or had not
  looked at. Each module is now scanned by a govulncheck built with that
  module's own Go, and the root module keeps the 1.25 toolchain the released
  binaries are actually built with.

- The verification gate had never passed in CI, for two unrelated reasons.

  The larger one was not load, though it looked like it: the media review runs
  ffmpeg under bubblewrap with `--unshare-all`, bubblewrap brings up loopback
  inside the new network namespace, and Ubuntu 24.04 restricts unprivileged
  user namespaces by default — so every ffmpeg test failed with `bwrap:
  loopback: Failed RTM_NEWADDR: Operation not permitted`. A runner policy
  reading as a broken sandbox. CI now relaxes that sysctl where it installs
  bubblewrap; the sandbox flags, which are a security property rather than a CI
  detail, are unchanged.

  The rest was capacity. `check.sh` documents `OPENREALTIME_TEST_PARALLEL`
  because these tests start real servers and hold real deadlines, and one
  package per CPU on a small machine makes a handful fail while every one of
  them passes alone; CI had never set it. The gate and compatibility jobs now
  bound it, and the compatibility job's deadline is no longer shorter than the
  work it was asked to do — it was being killed at twenty minutes rather than
  finishing.

- The routable-listener refusal is made before providers are configured. It ran
  after, so a deployment missing both a bearer token and a model credential was
  told about the credential and never about the open endpoint — and the test
  covering it could only observe the refusal on a machine where the providers
  happened to be configured, which is why it passed locally and failed in CI.

## v0.1.0 — 2026-09-06

The first release. It is numbered 0.1.0 rather than 1.0.0 deliberately: the
graph runtime is still replacing the binding-based launch paths, `serve` still
carries flags that a launch profile is meant to own, and the
[implementation tracker](docs/composable-agent-graph.md#living-implementation-tracker)
names that work. A 1.0.0 says none of that is true.

A `v1.0.0` tag was cut on 2026-08-17 and pushed. It named a tree the project
then moved 1,450 commits past while every binary built from it went on
printing `1.0.0`, because nothing checked the version constant against the
`VERSION` file or against the tag. That tag is superseded; this is the release
line. What follows is everything in it, including the work that tag contained.

### Highlights

- **The release is numbered, published, and checked against itself.** `VERSION`,
  the constant the binary prints, and the tag are now compared before anything
  is published, and a disagreement fails the release rather than shipping an
  artifact that misreports what it is. A tag builds the reproducible binaries
  and their checksums into a GitHub Release and pushes a multi-platform image
  to GHCR; until now both were built on every change and then discarded, so the
  only install path anyone had was a clone and a Go toolchain. The container
  cross-compiles instead of emulating a toolchain, so `linux/arm64` costs a
  cross-build rather than an emulated one.

- **A routable Realtime listener with no bearer token is refused.**
  `serve -listen 0.0.0.0:8765` with the token variable unset used to start,
  print a normal banner, and complete anonymous sessions against whatever
  provider credentials the process held. The WebRTC adapter had enforced
  exactly this rule for its own listener since it was written; the endpoint
  clients actually connect to did not, and the operations guide asked the
  reader to notice instead. Loopback still starts without a token, because that
  is the quickstart - and says so in the log, because something in front of it
  can publish a loopback origin without the server ever seeing that happen.

- **The health inventory and the metrics counters need the token when one is
  configured.** `/healthz` names the binding, the model identities, the live
  component digests, and the server-profile fingerprint, and `/metrics` reports
  session and media volume; both answered anyone who could reach the port. The
  status word and status code stay public, because an external health check
  cannot present a credential and a 401 would take a healthy server out of
  rotation. Everything else does not. A deployment with no token keeps the
  whole payload: there is no credential to present.

- **Cloudflare Tunnel is the documented way to publish it.** The server has no
  TLS listener and does not want one - a certificate to renew and reload
  without dropping live sessions is the wrong problem for a process whose job
  is holding connections open. `cloudflared` dials out to a loopback origin, so
  there is no inbound port and no certificate on the box.
  [deploy/README.md](deploy/README.md) has the configuration, including the two
  things that do not travel through an HTTP tunnel and the reason a tunnelled
  deployment still needs the bearer token.

- **The portable JavaScript client reducer is a package.** It was a directory
  of `.mjs` files with no manifest, which made the shared client contract
  something to vendor by hand rather than install.


### Highlights

- The audio-free text/file graph now accepts session instruction updates and
  explicit response creation. `policy.ObservationInvocation` owns the selected
  settings and automatic-versus-explicit activation, binds each model call to
  the committed conversation prefix, and preserves bounded stream cancellation.
  It accepts ordinary observation commits without inventing a conversational
  semantic decision. The graph remains a component reference; its production
  gateway adapter is still unfinished.
  The shared session policy also keeps its context version from moving backward
  when an older observation commit arrives late, preventing a subsequent manual
  response from using stale conversation history.

- Canceled speech recognition can no longer publish late provider text into
  conversation history or reopen an utterance from delayed audio and endpoint
  messages. ASR checks cancellation before publishing revisions, keeps bounded
  session-scoped stream cancellation, and leaves another session's active
  recognizer alone. The final-observation gate also rejects new causes from
  canceled streams, including cancellation reported by an interrupted observe
  or flush operation. Fresh utterances continue normally.

- An FD-Bench turn answered without a pause after the previous answer is no
  longer recorded as unanswered. A reply was recognised by a gap of at least
  one packet between audio segments, so an agent that finishes one answer and
  begins the next without pausing produced no gap and no reply: measured across
  eight conversations, one turn of thirty-six was counted missed while
  twenty-two deltas of a genuinely new response arrived 148 ms after it ended.
  A change of response now establishes an onset as well. A response that had
  already begun before the turn ended is still an overrun rather than a fresh
  answer, because its audio either side of the boundary carries the same
  identity.

- Canceling a content or generation stream now suppresses its later revisions
  through ingress, semantic admission, and static/session-configured generation
  policies. These paths previously consumed cancellation on the first match,
  or forgot it after canceling pending work, allowing withdrawn text, images,
  files, or semantic decisions to restart. Stream cancellation stays within
  the existing bounded memory. Ingress cancellation now distinguishes content
  and stream IDs and respects the exact session; explicit stream addresses
  also take precedence over unrelated envelope run IDs. Fresh streams and
  cancellation of one specific content request or generation retain their
  separate scopes.

- FD-Bench measures where each turn's speech actually stops and reports how
  many of the turns it counted as spoken over had the agent starting after the
  person had already stopped. The released turn boundaries enclose whatever
  silence the synthesiser left at the end of the clip, and how much that is
  follows the synthesiser rather than the condition: sampling six conversations
  in each of the twenty-one conditions, the audio goes quiet before the
  annotated end by a p90 of 20 to 200 ms across the seven F5-TTS conditions and
  1,180 to 1,420 ms across the three ChatTTS ones. An agent that endpoints on
  real silence and answers quickly lands inside the annotation without having
  spoken over anybody, and is charged an order of magnitude more often on some
  conditions than others - underneath the comparisons across conditions the
  suite exists to invite. The scoring is deliberately unchanged: what is added
  is the number that would justify changing it, so a run carries its own
  evidence. Where the measurement cannot read a turn - the 0 dB background
  conditions, where the noise never stops - it reports the annotation, so
  nothing there moves.


- A failure to yield is measured against the response that was interrupted,
  not against the clock. The suite followed the agent's audio after an overlap
  until a four-hundred-millisecond gap, which cannot tell the interrupted
  answer from the answer to the question that interrupted it, and on a fast
  agent the two are milliseconds apart: one attempt's interrupted response ran
  9.4 seconds past the overlap, the next response's first delta arrived 1.5 ms
  after its last, and the recorded failure to yield was 13.3 seconds. Every
  audio delta carries the response it belongs to, so the boundary is now that.
  The gap rule is kept underneath, for an endpoint that does not identify its
  responses and as a second boundary within one response.

- A truncation that arrives just after an answer ended now reaches the runtime,
  and one naming an item the session never played is refused instead of
  confirmed. The client is the only party that knows where playback actually
  stopped, and `conversation.item.truncate` is the only way it can say so. The
  utterance it names has usually just ended, because the server finished
  sending while the listener was already talking over it, and the session
  forgot the item the moment it ended: the message was answered with a
  confirmation that nothing had acted on, and the server went on recording the
  whole answer as heard. Sessions now keep the last thirty-two ended
  utterances, which is a race window rather than a history.

- The full-duplex suite times its windows from the first sound of an event
  rather than from where the event clip was placed. The recordings carry two
  timestamps saying when the event happens, and the clip behind them begins
  with whatever silence the speaker left: nothing worth counting for
  backchannels, side speech, and background speech, a median of 260 ms for
  interruptions, and 1,460 ms in the worst case. Interruption is the one
  category scored against a one-second deadline, so a third of it was charging
  the agent for time in which there was nothing to react to. On thirty
  recordings run twice each, 32 of 50 attempts yielded inside the window when
  timed from the annotation and all 50 did when timed from the first sound,
  with the slowest falling from 1,709 ms to 878 ms. Every task now reports
  `event_audible_after_ms` and `yield_latency_from_annotation_ms`, so a run
  scored before this can be reconciled with one scored after. The three
  categories that ask the agent to keep speaking have almost no lead, and
  their windows move later, not earlier.


- Production computer-use regressions now cover classifications delivered after
  cancellation and replacement, duplicate classifications while cancellation
  waits, and old acknowledgements replayed during a different cancellation.
  They check that each task keeps its own completion authority and that a
  fresh task can still execute and settle afterward.

- Element-graph traces reach the clients that ask for them. Three graph-native
  bindings and the word-timing reporter were labelling their debug entries with
  category names no client could select, so the gateway's category filter
  discarded every one of them before the wire: the emitters looked
  implemented, the client saw nothing, and neither end could tell that apart
  from a subsystem with nothing to say. Graph execution now has its own
  advertised category, word-timing failures are reported under speech
  synthesis, and a test reads the emitters rather than trusting them.

- `conversation.item.created` and `input_audio_buffer.committed` now name the
  item they follow. `previous_item_id` was null on every event, which the wire
  reserves for an item that has no predecessor, so a client reconstructing the
  conversation was told that every item was the first one - and the events
  carry no other ordering to repair it from. A commit and the creation that
  follows it report the same predecessor, so a client that inserts on either
  builds the same conversation.

- Speech activity now reports its position from the start of the session's
  audio, as the wire defines it. The acoustic element keeps one gate per
  utterance and each gate counted from its own first sample, so every
  utterance after the first was reported at the position of the one before
  it, and the error grew for as long as the session lasted. Probed with two
  tone bursts written at known offsets, the second came back three seconds
  early - exactly the length of the first utterance - and now comes back
  where it was written. A client that trims its own recording at
  `audio_start_ms`, or cuts playback where the person began speaking, gets
  the offset it asked for. The values reach only the wire, so no decision
  changes. The cascade and sidecar bindings keep one gate per session and
  were already correct.

- The full-duplex suites can keep the timed record each score was derived
  from, with `-transcripts` on `bench fdb` and `bench fdbench`. A score is one word per recording and the
  interruption category is decided inside a window of one second, so the
  difference between yielding eighty milliseconds late and never yielding
  arrived as the same word. Two earlier explanations for that window were
  each tested by rebuilding the server and re-running thirty recordings
  three times, about forty minutes an answer; the same question is now
  answered from one run, offline. FD-Bench keeps each conversation's turn
  boundaries beside its timing, because a premature start and a missed turn are
  the same word in a score and both are claims about where a turn ended.
  Benchmark sessions also retain `audio_start_ms` and `audio_end_ms` alongside
  arrival time, which is what separates a slow detector from a slow notice.

- Concurrent graph shutdown callers now wait for the same completed cleanup
  even when the graph was mounted but never run. Each caller can cancel its
  own wait while resource retirement continues, and subsequent callers receive
  the final cleanup error. Buffered terminal output remains readable.

- Standing-instruction extraction presents a split spoken request once.
  Earlier endpoint clauses and their superseded recognition hypotheses no
  longer appear both in recent conversation and in the reconstructed request.
  Canonical item and stream identities preserve unrelated history, including
  older turns with identical words and other speakers.

- Conversation policy now distinguishes played words from prepared, queued,
  and canceled speech. After interruption, it sees the heard prefix, any word
  cut short, and the unplayed remainder separately. A completed model response
  no longer tells the next policy decision that the user heard every draft
  word. Completed speech and unmeasured text responses keep their existing
  representation.

- Scenario conversation completion now waits for the producing response's
  model and segmentation terminals and applies its output state in semantic
  policy before notifying the client. A request immediately after completed
  speech no longer sees the old response as still speaking. Queued segments
  and other active responses retain their speech-control behavior.

- Computer-use cancellation keeps settlement complete after its terminal
  acknowledgement. Replaying an earlier pending update can no longer strand
  cancellation while another component is stopping, and later publications
  preserve the original acknowledgement used to authorize downstream cleanup.

- Benchmark sessions now record passive observation and debug traffic without
  extending conversation deadlines. Continuing screen updates can no longer
  turn a recovered, settled computer-use task into a false session timeout.
  Response progress and outstanding tools still retain their working interval.

- Continuation commits now check for superseding evidence and append output
  under one trajectory lock. A concurrent user correction can no longer enter
  between those operations and leave a stale answer or tool call committed
  after it. Unrelated assistant speech still allows reasoning to complete.

- Computer-use settlement remembers which canonical result already released
  continuation. A delayed copy of its visual consequence can no longer request
  a second completion decision and block settlement of the next action. New
  results under the same user intent still support multi-step work.

- Completed playback now publishes its played-word state through the scenario
  graph before releasing the turn. An explicit response request after speech
  can evaluate the current conversation immediately; it no longer waits for
  a later user observation to publish the playback state. Partial playback,
  concurrent observations, and canceled append waits retain their exact
  history and ordering.

- The scenario conversation profile preserves sanitized speech history when a
  newer observation makes the original model result stale. Exact original-prefix
  verification and playback receipts keep already-audible words available to
  later continuations. Stale reasoning, native state, and tool proposals stay
  outside the separate history transaction; its `speech_retained` outcome does
  not grant action authority.

- Event-count scorer version 10 requires independent recognition of exactly
  the expected number in captured agent audio and silence outside the authored
  response windows. Reported digits cannot certify garbled synthesis. Results,
  media reviews, and exact replay retain each hearing, activity measurement,
  and recognizer error. Scenario continuation instructions now request
  punctuated number words for counts; retained synthesis controls support that
  input form without establishing general speech-quality acceptance.
- A confidently verified standing trigger can recover an uncertain final
  `answer` just as it can recover `listen`. The ordinary confidence threshold
  previously suppressed the first animal count even after the independent
  activation guard had grounded it. A production-graph regression verifies
  actual count audio and preserves silence for uncertain partials, unmet
  conditions, and uncertain activation evidence.
- Spoken stops and acoustic interruption now revoke a prepared speech stream
  even after text generation and segmentation finish. The overlap controller
  retains the emitted segment count until exact speech terminals arrive, so
  queued sentences remain cancellable between playback segments. Duplicate and
  reordered terminal receipts cannot retire unplayed speech or revive completed
  work. A mounted scenario-graph regression verifies quiet after stopping and
  successful speech on a later request.
- Interrupted-count scorer version 9 requires at least three observed numbers
  before and after interruption, an ordered prefix within the requested range,
  and captured speech near the interruption. A lone “One” followed by “Two”
  cannot pass. Results and reviews retain the observed counts and recent audio
  activity. Scenario profiles now distinguish a complete spoken numeric range
  from one count per new event; silence remains explicit when no event occurred.
  The pre-resumption count includes speech heard while the agent is stopping;
  captured audio must then stay quiet until the request to resume. Intermediate
  version-8 diagnostics retain their original, shorter recognition windows.
- Scenario recordings now retain versioned replay inputs. The offline
  `review replay-scenario` command reconstructs the authored timeline, acoustic
  checks, menu outcomes, counting decisions, and latency metrics from sealed
  media and observations. Behavioral acceptance uses the same verifier and
  rejects missing replay evidence or scores that disagree with reconstruction.
  Historical integrity verification remains available; replay does not prove
  recognizer accuracy, provider authenticity, or release acceptance.
- Acknowledgement scorer version 7 requires the purchase-confirmation detail
  from the supplied refund policy. An unrelated email about the return label
  cannot satisfy it. Historical recordings retain their original scores;
  separately attributed rescoring exposes previously passing omissions.
- The scenario conversation graph keeps short comma introductions such as
  `First,` with the following phrase before synthesis. Its separate
  `minimum_clause_runes` setting preserves short complete sentences and
  immediate flushing at source completion. Existing profiles that omit the
  setting retain their segmentation behavior.
- Listener-backchannel validation now treats speech-recognition punctuation
  as insufficient evidence of a question. The validator uses lexical content
  and conversational context, preserving genuine requests and floor-taking
  speech while accepting pure continuers such as ASR-rendered `Right?`.
- A new graph-native diagnostic, `acknowledgements during observed speech`,
  sends its cues only after recorded agent audio establishes an opportunity.
  Scorer version 6 checks actual input positions, full transmission, source
  PCM identity, and independently recomputed activity. Silence and partial
  cues fail explicitly. The default twelve-case release population is intact;
  the extension cannot earn full-suite credit.
- Scorer version 5 rejects acknowledgement holds when an overlapping response
  is cancelled, fails, or ends incomplete before the measured window closes,
  even when replacement audio continues. Missing/conflicting terminal evidence
  or unattributed active audio cannot earn hold credit. Completed segments and
  cancellations after the hold window remain valid within the check's scope.
  Historical recordings and scores remain unchanged.
- Acknowledgement scenarios now supply a concrete refund policy and require
  key explanation details, preventing a sustained non-answer from passing.
  Scorer version 4 retains response identities, serialized audio positions,
  and terminal status beside the waveform measurements. Reviews distinguish
  reported completion, cancellation, incomplete output, and missing evidence
  without inferring the cause of silence. Historical scores remain intact.
- Focused graph-native scenario diagnostics now select exact cases with
  repeated `-case` flags in both profile creation and execution. The frozen
  populations must match. `scenario -list` lists canonical names without
  services. Subsets retain audio, visual inputs, results, and source receipts
  while remaining ineligible for the full-suite acceptance gate.
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

- **A LiveKit room's video reaches the engine too.** The agent subscribed to
  audio only, so a stock meeting client sharing a screen was audio-only to it
  and video arrived only from a custom participant sending protocol frames as
  data packets. Behind a `-video` flag it now subscribes to room video,
  declares the extension in its own `session.update`, and bridges VP8 key
  frames as the same two events, asking the publisher for one each second with
  a picture-loss indication. It writes the bridge itself rather than importing
  the server's, for the reason it writes its own protocol client. The flag is
  opt-in: without it the wire is byte-for-byte unchanged.
- **A VP8 video track reaches the engine.** The in-process WebRTC adapter
  ignored inbound video tracks, so a stock client that published a screen
  share was audio-only. It now bridges the track's key frames as the same
  source and frame events a client sends, only after the client negotiated
  `video.input`, scaled and encoded within the limits the server answered
  with, and at no more than the negotiated frame-rate cap. Key frames only:
  there is no pure-Go inter-frame decoder and libvpx would cost the static
  binary, so the adapter requests a key frame a second with an RTCP
  picture-loss indication, which is the cadence the observer wanted anyway.
  Other video codecs are logged and drained. The decoder and scaler come from
  `golang.org/x/image`.
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

- **Interruption yielding is better, and the remaining floor is not what it
  looked like.** Repeating the first thirty interruption recordings three
  times puts the repaired tree at 46/74 applicable where single samples said
  1/26 in August and 10/26 in September, so the improvement replicates. Six
  recordings still fail every attempt. Two explanations for their one-second
  yield were tested and both were wrong: a streaming recogniser is 164 ms
  *worse* at the median rather than better, and cutting the policy's decision
  deadline from 1,000 ms to 300 ms moves the latency cluster by 9 ms.
  Something else holds a floor near one second, which is also FDB's yield
  window, so the category is decided by tens of milliseconds either side of it.

- **The first complete FDB v1.5 run since the interruption work.** Retained
  under `artifacts/fdb-candidate-full498-20260905-02-*` and not reportable:
  one recording reached no evaluation when the local policy socket broke under
  host contention. Against the 2026-08-31 campaign on the same profile and the
  same applicability basis, interruption yield went from 15/156 to 80/161 and
  its median from 2,412 ms to 1,002 ms, while holding through background
  speech fell from 89/89 to 61/73 and through side speech from 93/95 to 76/82.
  An agent readier to stop for an interruption is readier to stop for a voice
  that was not talking to it, and the aggregate rose anyway because
  interruption is the largest category, which is what per-case targets exist
  to catch.

- **One transient provider error no longer costs a whole campaign.** A
  session that reached no evaluation is retained as an incomplete attempt
  rather than scored, which is right, but resume then met the suite's own
  recovery validator, which refuses a retained failure and aborted the entire
  resume. A four-hour FDB v1.5 run that lost a single recording to a broken
  pipe from the local policy provider could only be redone from the start.
  Resume now retires an incomplete attempt into the same `interruptions/`
  namespace an uncommitted attempt takes, where it stays inside the sealed
  bundle, and runs that case again. A behavioural failure is never re-rolled:
  only an attempt the harness itself recorded as having reached no evaluation.

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

- **The speaker-identity adapter has a contract test.** It was the one adapter
  with no test at all, so a service that renamed the sample-rate header or the
  embedding field would have failed only in a live session. Six tests now pin
  the request shape, the no-embedding answer for an utterance too short to
  identify, refusal of empty audio before any request, the reported status of
  a failed service, rejection of a non-JSON body, and the request timeout.
- **Local default ports agree with the documented stack.** The whisper and
  generic transcription catalogue entries defaulted to :8001, which the
  streaming Qwen3-ASR service owns, and the OpenAI-compatible speech adapter
  defaulted to :8080, the language-model port, while the catalogue said
  :8081. A test now pins every local default to the ports the quickstart and
  local-stack guide document.
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

- **The agent stopped speaking for voices that were not talking to it.** When
  the agent's voice lifecycle reached the interaction policy, keep-speaking and
  stop-speaking became the acts available during speech - and the instruction
  defined them without ever saying how to choose. Every other act it governs is
  carried by a worked example; these two were the only ones without. A complete
  FDB v1.5 campaign is what that cost: sixteen recordings that had held through
  background speech or speech addressed to somebody else now stopped for it,
  and thirty-five more never reached the overlap at all. The instruction now
  says that stopping is not the safe answer, that only words directed at the
  agent and changing what it should do take its floor, and carries a worked
  example of each act with the agent mid-sentence.
- **A repeated attempt now carries its own execution evidence.** The scope
  named the recording rather than the attempt, so on the first stability run
  every attempt after the first looked like evidence for a different task and
  the run refused itself. The refusal was right and the scope was wrong.
- **FDB v1.5 can repeat a recording.** It never could, so every per-recording
  claim it has made was sampled once - and two runs of the same forty against
  the same executable disagree on five of them while reporting the same number
  of passes. `-repeat N` runs each recording N times and the report says how
  many agreed with themselves, naming the ones that did not. A single run keeps
  its exact identifiers and output. The scenario suite answered this question
  with fifteen repeats years of evidence ago; this suite had never asked it.
- **A finished answer is not a failure to hold.** The scorer asked whether the
  agent had spoken in the half second before an overlap, and called that
  speaking when the event began. For a one-line command whose answer is over in
  a second those are different questions: four of forty background-speech
  recordings were judged applicable on audio that had stopped hundreds of
  milliseconds earlier, then failed the hold they had nothing left to hold.
  Applicability now asks whether the audio actually reached the event, within a
  hundred milliseconds of jitter tolerance, and the half-second total stays as
  the reported metric. Rerunning the forty took the spurious failures from four
  to two; both survivors had audio still arriving at the event and none through
  the hold window, which is the real defect.
- **One sample of audio is not the agent speaking.** The FDB scorer's two audio
  tests asked for more than zero milliseconds, which a single 24 kHz sample -
  0.0417 ms - satisfies. A recording whose answer ended one sample inside the
  lookback window was judged applicable, asked to hold through an overlap it
  had already finished, and failed for it. Both tests now want one packet of
  audio, which is the least the pipeline can deliver and the least anyone
  could hear.

- **A final transcript that lands mid-speech no longer kills the session.**
  Since the semantic policy learned whether the agent is speaking, its
  executable acts during speech have been the two speech controls, but the
  committed-observation path still allowed only silence and answer, the
  free-floor acts. The intersection was empty, and every profile without a
  transcript-event policy failed the whole session with "no executable act"
  on exactly the FDB interruption case: the first fresh FDB v1.5 campaign of
  the day lost two of its first twenty-five recordings to it and was stopped.
  Keep-speaking and stop-speaking are now offered while the agent is audibly
  speaking, and a regression test drives a mid-speech final through the
  no-policy path to a disposition instead of a failure.

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
- **The Opus encoder is compiled and tested somewhere.** The cgo encoder
  behind the `opus` build tag was reachable from no gate, so nothing would
  have noticed it stop building. `local.go.webrtc.opus` compiles and tests
  it; a new `pkg_config` prerequisite kind asks pkg-config for libopus and
  libopusfile, so a host without them plans as blocked rather than failing to
  build, and both CI gate jobs install the two development packages.
- **DynaCU's Python environment is what the gate expects.** The prepare
  script accepted the system interpreter while the matrix required a virtual
  environment inside the pinned checkout, so the optional gate always
  reported the environment missing on a host that could run it. The
  environment is now created over the system packages; the gate is blocked
  only on the endpoint it must be pointed at.
- **Sidecar conformance is a dedicated gate.** The broad Go sweep tolerates
  skips by design, so the one place the bundled Python element and the three
  reference sidecars were exercised could be skipped without anyone noticing.
  `local.conformance.sidecar` runs both tests as real subprocesses with the
  release gate set and forbids a skip.
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
- **A settlement test stops depending on how busy the machine is.** The
  probe-collision test hashed the issue time into the probe identity and gave
  each mount an advancing clock, so whether the second mount reproduced the
  predictor's probe depended on how many clock reads its startup happened to
  make first. Under a loaded race run that turned a refused collision into an
  ordinary held probe and failed the gate. Both mounts now read a fixed clock,
  which is the collision the test exists to provoke.
- **The gate can stop oversubscribing a shared machine.** `go test ./...` runs
  one package per CPU, which serialises this suite's end-to-end tests on a CI
  runner and does the opposite on a thirty-two core box already carrying other
  people's work: thirty-two packages at once, each starting real servers and
  holding real deadlines, and a different handful fails every run while every
  one of them passes alone. Two full gate runs at load average 60 failed on
  five different tests between them; one run with `OPENREALTIME_TEST_PARALLEL=4`
  passed every stage. The variable bounds how many packages run at once and
  changes nothing about what is checked. Unset stays Go's default.
- **Three gate failures were assertions about the machine, not the system.**
  Each pinned a number tighter than the property it was testing, and each
  failed the verification gate at load average 70 while passing alone. Two
  speech-cue tests bounded where a cue lands to a 300 ms slice inside its own
  authored window, when what proves the sent-audio clock is the equality
  beside it - the tone present at exactly that position in the retained audio,
  and the score's trigger matching it - which holds wherever in the window the
  cue lands. The third gave a fallback from a ten-millisecond reflex timeout a
  one-second budget, when the claim is that it does not wait on the
  thirty-second provider deadline. The windows are now the authored ones and
  the budget is five seconds; every discrimination they were making survives.
- **The Go sweep now enforces the bound it claims.** The race gate is allowed
  sixty minutes, but `go test` applies its own ten-minute default *per
  package*, and `cmd/openrealtime` spawns real servers in most of its tests:
  at load average 70 the package reached that bound while doing real work and
  panicked with a stack naming whichever three-second test was running, which
  reads exactly like a hang. Both gates and `check.sh` now state twenty
  minutes, which the largest package cannot reach by being slow, so a timeout
  there still means something is stuck.
- **Two gates stopped depending on how loaded the machine is.** The matrix's
  race sweep failed twice on this host at load average 70, each time on a
  different test, each passing alone. One asserted a gauge in the statement
  after waiting for a counter, so it was reading the window between a session
  failing and releasing its slot; it now waits for the gauge itself. The other
  was the benchmark driver: the conversation horizon cancels the video sender
  too, so a frame mid-write when it expired surfaced the raw deadline instead
  of the typed conversation timeout, and which worker noticed the horizon
  first decided whether a suite recorded a scored negative or an
  infrastructure failure. The driver now reports the horizon as the horizon.
  The retained test pins the ordinary path; the in-write case that failed the
  matrix needs an injection point the driver deliberately does not have.
- **The release matrix runs in CI.** Nothing ran the matrix: the gate job
  runs `check.sh`, which tolerates skips by design, so the matrix's dedicated
  gates could rot with every check green. A `release-matrix` job now runs the
  complete local scope with skips forbidden on every change and keeps the
  create-only report as an artifact. Its first local run found five things:
  formatting inside sealed evidence, a timing-sensitive race-sweep test, and
  three cancellation fuzz gates that lose the context cause under the fuzz
  engine.
- **The cancellation fuzz gates no longer fail on a fuzz-engine behaviour.**
  Under `go test -fuzz` a blocked waiter can be handed `context.Canceled`
  from a context whose cause reads correctly a microsecond later; a
  twenty-line target reproduces it in a second, and the same target passes
  thousands of times outside the engine. The three gates accept the bare
  cancellation only while the engine drives them; the runtime's seeds still
  prove the cause survives in the ordinary sweep.
- **Retained evidence is not source the gofmt gate may fail on.** Benchmark
  evidence directories under `artifacts/` carry the support programs that
  produced them, copied in verbatim and sealed by receipts. The gofmt gate
  walked them and failed the local matrix on formatting inside sealed
  evidence, which could only be fixed by breaking a receipt. Both the matrix
  gate and `check.sh` now exclude `artifacts/` the way they exclude
  `.runtime/`; Go tooling already ignores those dot-directories.
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

## Earlier work in this release

Written while the tree was tagged `v1.0.0`, and part of v0.1.0 above.

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
