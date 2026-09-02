# Composable Presentation and Observability

- **Status:** accepted design with a shipped composable companion; signed native and complete release evidence remain pending
- **Scope:** the OpenRealtime presentation host, browser client, macOS client,
  observability APIs, client plugin lifecycle, security boundaries, and end-to-end gates
- **Depends on:** [Composable Real-Time Agent Element Graph](composable-agent-graph.md)

This document extends the element-graph design to the software around the
agent. The browser application, its local server, the macOS application,
inspection, editor, media capture, tool hosts, and view components are not
privileged parts of the realtime server. They are replaceable plugins composed
against versioned services. Both shipped clients connect to the same
OpenRealtime server and consume the same protocol and observability contracts.

The design follows the DeepSeek Harness/Cordis discipline: product behavior is
mounted as a plugin; dependencies are named services; registrations and
runtime effects are scoped to the plugin that created them; removing a
provider disposes dependents; and profiles are patchable plugin trees rather
than new application species. A tiny loader, schema validator, and lifecycle
context are mechanics, not a place for product behavior or a hidden default UI.
This is an architectural contract, not a claim that OpenRealtime plugins are
currently loadable by `dsh`: no pinned DeepSeek Harness runtime, compatibility
adapter, or cross-runtime conformance corpus exists yet.

Nothing in this document weakens the OpenAI Realtime compatibility boundary.
OpenRealtime remains an additive extension. A client that knows only the
OpenAI protocol can still use the realtime endpoint. Graph inspection,
multimodal extensions, and client composition are explicitly negotiated and
versioned under the OpenRealtime namespace.

## 1. Presentation boundary

The shipped browser and macOS clients are compositions of one client platform,
not application forks. Browser modules are selected by an immutable manifest;
the native app selects exact Swift implementations of the same logical
contracts. Both consume language-neutral reducer fixtures and the same public
Realtime and negotiated management APIs.

The old standalone browser/demo applications were reference prototypes, not
production inputs. Their relevant transport and lifecycle behavior is covered
by the composable profiles and end-to-end gates; no loader, alias, or inferred
same-origin endpoint adapter remains in the normal client boundary. Portable
Swift tests are cross-platform evidence only. They do not establish that a
signed macOS app, device permission, or native media path ran.

## 2. Architectural decisions

### 2.1 No UI in the realtime server

The gateway owns protocol admission and rendering. It does not own HTML,
Swift views, a browser asset tree, filesystem tools, browser automation, or a
presentation manifest. In particular, the target `gateway.Config` has no demo
handler and the normal `serve` command has no UI switch.

A browser deployment consists of two independently deployable compositions:

1. an OpenRealtime server profile, which may be headless; and
2. an optional presentation-host profile, which serves client plugins and may
   proxy authenticated protocol or local-effect connections.

The host can run beside the gateway in one operating-system process for a
developer profile, but this is a deployment choice expressed by a profile. It
does not grant the host an untyped path into session internals. The host still
uses the public realtime and management APIs exactly as a separately deployed
host would.

### 2.2 One plugin discipline on both sides

Every product capability is a plugin in one of three realms:

| Realm | Examples | Isolation boundary |
| --- | --- | --- |
| server | realtime gateway, graph runtime, inspection, metrics, authoring, transport adapters | process or sidecar placement selected by deployment |
| presentation host | static module store, credential relay, local tool provider, browser-control provider, artifact store | operator-controlled HTTP process, loopback by default when it can perform local effects |
| client | protocol reducer, WebSocket/WebRTC transport, microphone, camera, screen, playout, conversation view, graph inspector, editor | browser worker/window or macOS application scope |

A realm has a service context and scoped child contexts. Plugins communicate
through declared service interfaces and typed events, never by importing
another plugin's concrete implementation. Cross-realm calls use the versioned
wire API rather than pretending an in-process object exists remotely.

### 2.3 Reuse Graph IR semantics without mixing trust planes

Client and presentation compositions use the same descriptor, lock,
dependency, values, deployment, and lifecycle vocabulary as agent graphs. A
compiled client graph is still immutable and fingerprinted, and its selected
implementations are attested at runtime. It is a different graph *profile* and
runtime realm, not a second ad hoc plugin system.

The semantic client graph contains services and event flow. View layout is a
separate, non-semantic artifact keyed by stable slot and instance IDs. Moving a
panel cannot change effect authority, media routing, or protocol behavior.
Browser and macOS implementations may satisfy the same descriptor with
platform-specific code; the deployment lock records which implementation ran.

### 2.4 Profiles and patchable bundles

A profile is an ordered, inspectable plugin tree. A bundle contributes
descriptor-locked rows and assets. An operator overlay can replace a row by
stable ID, insert a provider, or remove a UI feature without rebuilding the
gateway.

Initial shipped profiles are compositions, not kernel enums:

- `server-headless`: graph runtime, realtime gateway, selected transports,
  health, and bounded telemetry;
- `browser-minimal`: presentation host, session relay, client loader,
  protocol reducer, one transport, text input, and transcript view;
- `browser-developer`: the minimal profile plus audio/video, tool providers,
  graph inspection, trace timeline, artifacts, and authoring plugins;
- `macos-developer`: native transport, reducer, audio/video capture and
  playout, tool/confirmation host, inspection, and native view plugins; and
- `client-headless`: the shared reducer and scripted sources/sinks used by
  conformance, simulation, and benchmarks with no view provider.

Names are distribution defaults only. Runtime code resolves services and
descriptors; it does not switch on these names.

The public developer workflow is `openrealtime companion`. It supervises a
clean server, a separate loopback WebRTC adapter, and the descriptor-locked
presentation host. The default opens `browser-developer-webrtc`; the
`-client browser|macos|both|none` policy changes only client launch. The host
mounts separate WebRTC and WebSocket relay plugins so browser and native
clients can traverse one unchanged long-lived server. A custom presentation
address produces an exact temporary native endpoint directory that is removed
on shutdown. The gateway never mounts presentation assets or routes as a side
effect of this convenience command.

The Go `server.Bundle` now compiles the headless HTTP boundary as a stable
router plus separate realtime, observability, session-inspection, and canonical
management-session API entries. Every entry carries the exact selected runtime
artifact in server-realm live evidence. Historical route aliases and the fixed
gateway facade have been deleted; the gateway exports payload handlers and the
compiled server profile owns every public route.

## 3. Plugin contract

### 3.1 Immutable descriptor

Each plugin revision publishes a canonical descriptor containing at least:

```text
identity          symbolic id, semantic revision, content digest
realm/platform    server, host, browser, macOS, or portable implementation
provides          typed service interfaces and event schemas
requires          required service revisions/capabilities
optional          optional services observed reactively
configuration     separately identified values-schema reference
permissions       network, media, storage, filesystem, process, device, effect
authority_ceiling maximum effect class and target scope the plugin may request
assets            content-addressed module/resource digests
state             optional state schema and migration declarations
lifecycle         mount, ready, quiesce, snapshot, restore, dispose contracts
health            readiness and bounded diagnostic vocabulary
```

Descriptors contain no secret values. A semantic contract change produces a
new immutable revision. Implementations register against a descriptor and are
resolved through a reviewable lock plus deployment bindings, just like agent
elements.

### 3.2 Services, dependencies, and scoped effects

A plugin mounts only after every required service is ready. A missing required
service leaves the plugin pending with a visible reason. If a provider
disappears, dependent plugins quiesce and dispose in reverse dependency order;
they may remount when a compatible provider returns.

Every registration, listener, worker, timer, media track, socket, temporary
file, device lease, and authority grant is owned by a scope. Disposal is
idempotent and bounded. Unload completes only after the scope reports no live
effects. Irreversible external actions are ledgered facts and are never called
reversible merely because their registration can be removed.

Multiple client sessions receive child scopes. Closing one session cannot stop
another session's capture, consume its events, retain its tokens, or dispose a
shared provider prematurely. A plugin that needs an isolated service declares
the isolation boundary in the profile rather than creating a hidden singleton.

### 3.3 Client slots are services, not authority

A view plugin registers a renderer in a typed slot such as `root`,
`conversation.activity`, `session.controls`, `inspection.graph`, or
`artifact.viewer`. A slot declaration defines cardinality, ordering, child
slots, and the immutable state projection provided to the renderer.

Renderers receive data and callbacks through their declared face. They do not
reach into the protocol connection, tool registry, or graph runtime. Streaming
business state lives in framework-neutral services with immutable snapshots;
the DOM/AppKit/SwiftUI layer is a projection, so replacing a view toolkit does
not rewrite the session state machine.

Registration in a confirmation slot does not grant effect authority. A local
effect provider independently validates the signed proposal, target,
confirmation receipt, authority decision, and idempotency key immediately
before execution.

### 3.4 Client module loading

The browser host serves a fingerprinted boot manifest and content-addressed
plugin modules. The small browser bootstrap verifies the manifest and assets,
constructs the client context, activates required infrastructure plugins, and
then mounts the selected view renderer. Cross-plugin cooperation occurs only
through services/events; importing a sibling plugin's private state is a build
and conformance error.

The macOS app uses the same logical manifest and descriptor catalog but
resolves native implementations from a signed local bundle. A manifest cannot
make the native app load JavaScript or acquire a permission absent from the
installed app entitlement and operator policy.

## 4. Clean server and host APIs

### 4.1 Realtime data plane

The existing OpenAI-compatible WebSocket endpoint remains the common session
entrance. WebRTC is a transport adapter over that entrance, not a second
session implementation. Browser and native clients may select different
transport plugins and still reach the same server-side session graph.

| Contract | Purpose |
| --- | --- |
| `GET /v1/realtime` | OpenAI-compatible WebSocket upgrade and OpenRealtime-negotiated events |
| `POST /v1/realtime/calls` | WebRTC SDP/media plus the same protocol event stream over a data channel through the GA OpenAI-compatible call boundary |
| `openrealtime.*` events | additive multimodal, interaction, authority, and inspection negotiation |

The transport interface exposes connection state, negotiated capabilities,
ordered protocol events, close reasons, media-track attachment, data-channel
limits, and backpressure. Session state does not branch on transport names.

Browser/mobile profiles prefer WebRTC when media is carried because the
browser media stack supplies pacing, jitter handling, and echo processing.
WebSocket remains a first-class plugin for compatibility, debugging, text,
and explicit PCM/event transport.

### 4.2 OpenRealtime management plane

Observability is a versioned data/control API, not code embedded in a canvas.
The target API exposes these resource families under an OpenRealtime-specific
namespace. The historical session-live alias is not part of the target and is
deleted; negotiated inspection capabilities point directly at the canonical
management API:

```text
GET  graph descriptor and exact canonical Graph IR by fingerprint
GET  descriptor/configuration schema catalogs by immutable identity
GET  one live session snapshot
GET  bounded live session deltas or events with resume cursor
GET  a bounded causal trace or a deterministic trace export
POST validate/compile/render a graph authoring document
POST apply a mediated source edit or graph reconciliation candidate
```

Every schema has an explicit version. Snapshots and streams identify the exact
Graph IR, values, deployment, resolved implementations, plugin profile, and
client composition. Cursors are bounded and resumable; a slow inspector cannot
backpressure the realtime graph. Payloads are omitted by default, and
redaction, retention, rate limits, and dropped-record counters are observable.

Management access uses narrow capabilities. A session may negotiate a
short-lived, session-scoped read token. Authoring and reconciliation require a
separate operator capability and never reuse the session bearer token.
Capabilities stay in headers or protected channels, never URLs, Graph IR,
client manifests, logs, or saved screenshots.

The UI-independent mediated source boundary is now implemented at
`POST /openrealtime/v1/authoring/write` as a separately selected management
plugin. A deployment must explicitly provide one opaque-identity-bound rooted
publisher and issue distinct create or update capability grants; merely
supplying an authoring document, mounting the pure analyzer, or opening an LSP
document grants no file access. Creates are atomic and no-replace, updates
require the exact predecessor content digest and atomically exchange a synced
same-parent stage, and the response is a content-bound payload-free audit
receipt. Linux and Darwin use their native no-replace/exchange primitives;
other server platforms refuse this publisher rather than weakening the
contract. The browser save workflow remains unmounted until a client profile
explicitly selects this effect and its operator-root grant.

### 4.3 Presentation-host API

The optional host exposes only services selected by its profile:

```text
GET  /client/v1/manifest                 locked client graph and capability declaration
GET  /client/v1/modules/{digest}         immutable browser plugin asset
GET  /client/v1/realtime                 optional credential-holding WebSocket relay
POST /client/v1/realtime/calls           optional credential-holding WebRTC relay
GET  /client/v1/effects                  optional local tool/effect channel
GET  /client/v1/artifacts/{id}           sandboxed artifact resource
GET  /client/v1/downloads/{id}           attachment-only generated file
```

Routes exist only when their provider plugin is mounted. The manifest reports
the same fact; the browser never guesses available tools or targets from UI
code. A direct-to-server profile can omit both relays and use an ephemeral
credential provider. An effect-capable host is loopback-only by default and
declares the exact filesystem root, browser/device target, tools, and
confirmation policy before the client can advertise them to a session.

## 5. Shared client state machine

Browser JavaScript, macOS Swift, and the Go headless driver share one generated
protocol vocabulary and one language-neutral conformance corpus. Each runtime
implements the same small services:

- connection and reconnect state machine;
- ordered inbound/outbound protocol event log;
- negotiated session/capability projection;
- conversation item and streaming response reducer;
- interruption, truncate, cancel, and playout accounting;
- media source/sink registration and backpressure;
- tool proposal, confirmation, result, and error lifecycle;
- inspection token, snapshot, delta, trace, and graph-identity projection; and
- artifact/download references without executing model-authored content in the
  application origin.

The corpus contains event sequences, virtual times, expected snapshots,
outbound commands, errors, and terminal cleanup. Code generation supplies
event/schema types where practical; golden vectors establish parity where
platform APIs require separate implementations.

## 6. Security model

- The realtime server never serves a UI merely because it is running.
- The browser never receives a long-lived upstream/server credential.
- Client manifests and modules are content-addressed and covered by a strict
  content security policy; hot replacement is an explicit development profile.
- Render plugins cannot register effect providers or exceed the profile's
  authority ceiling.
- Local effect providers bind to loopback unless an authenticated remote
  deployment explicitly supplies an equivalent trust boundary.
- Tool declarations come from the enforcing provider. The client cannot widen
  schemas, confirmation requirements, target fences, or mutability.
- Artifacts execute on a separate origin/policy boundary with sandboxing;
  downloads use attachment disposition.
- Camera, microphone, screen, filesystem, browser, process, and accessibility
  permissions are separate provider capabilities and are visible in the live
  client graph.
- Observability is payload-free by default, bounded, redacted, and independently
  authorized. Turning it off cannot change session semantics.

## 7. Replacement and deletion plan

Replacement proceeds in independently reviewable slices. Old implementations
are consulted only as behavior references; no adapter, dual-run path, or
historical loader is added to the new runtime. Once a replacement passes its
declared end-to-end matrix, the obsolete path is deleted.

### Stage P0: contracts and regression oracle

- Freeze the relevant browser and macOS event behavior as shared conformance
  vectors, then delete the obsolete standalone implementations.
- Inventory routes, assets, tools, permissions, transport behavior, UI state,
  cleanup, and current browser end-to-end assertions.
- Add client-visible performance measurements and record a clean baseline.

### Stage P1: plugin lifecycle and catalogs

- Extend the graph descriptor/runtime with client and host realms, permission
  ceilings, scoped service registration, dependency loss, and asset identities.
- Compile locked client profiles and expose their static/runtime identities.
- Add mount/unmount, missing dependency, replacement, rollback, race, and leak
  tests before moving product capabilities.

### Stage P2: shared protocol/client core

- Publish generated protocol schemas plus the language-neutral reducer corpus.
- Extract transport, session reducer, media, tools, inspection, and artifact
  interfaces from DOM/SwiftUI code.
- Make Go headless, browser, and Swift implementations pass identical vectors.

### Stage P3: standalone presentation host and browser client

- Implement manifest/module, credential-relay, effect, artifact, and download
  providers as separate host plugins.
- Build the browser client from transport, media, protocol, tool, inspection,
  editor, and view plugins selected by the manifest.
- Replace the standalone browser applications with profiles over those
  plugins, then delete duplicated modules once golden and real-Chromium parity
  holds.
- [x] Remove `serve -demo`, `gateway.Config.Demo`, and `/demo` from the gateway;
  presentation assets and browser routes live only in the separate host.

### Stage P4: macOS client

- Replace the monolithic `RealtimeClient`/view coupling with the same service
  boundaries and generated conformance corpus.
- Register native WebSocket/WebRTC, audio playout/capture, camera/screen,
  browser/computer-use, confirmation/effect, inspection, and view providers.
- Connect to the exact same realtime and management server APIs as the browser;
  platform adapters may differ, protocol behavior may not.
- Run actual Swift unit/integration tests and a signed app smoke/E2E test on a
  macOS runner. Source-string tests remain a cheap lint, not release evidence.

### Stage P5: inspection, authoring, and reconciliation

- Implement graph/descriptor/schema/snapshot/delta/trace services without any
  UI dependency.
- Mount canvas, timeline, queue, authority, trace, and editor views as plugins
  in both supported presentation profiles where the platform has a renderer.
- Exercise client/host plugin hot replacement, safe-point server graph changes,
  state migration/refusal, rollback, and leak detection end to end.

### Stage P6: obsolete-path deletion

- Replace documentation, examples, production launch paths, test launchers,
  and benchmark harnesses with profiles and the clean APIs.
- Reject unpinned client modules and hidden UI/effect registration.
- Remove duplicate routes, constructors, event reducers, presentation-specific
  gateway fields, and client-name switches before accepting a production
  candidate.

## 8. End-to-end and release gates

No presentation stage is complete from unit tests or source inspection alone.
The release matrix runs a real gateway and presentation host, real browser or
native client, and real protocol/media connections.

| Contract | Browser | macOS | Headless/control |
| --- | --- | --- | --- |
| manifest, digest, dependency, permission validation | real browser boot | native catalog boot | invalid/golden fixtures |
| text/session/event reducer parity | Chromium | Swift test/app | Go vectors |
| WebSocket session | required | required | required |
| WebRTC media/data channel | required | required when native provider ships | adapter/probe |
| microphone capture and audio playout | virtual/fixture plus device smoke | virtual/device smoke | deterministic PCM fixtures |
| image, camera, screen, and adaptive frame flow | required | required | deterministic frame fixtures |
| interruption, truncate, cancel, manual turns | required | required | virtual-clock vectors |
| tools, confirmation, authority, target fence, idempotency | required | required | adversarial vectors |
| browser/computer-use and visual feedback | developer profile | native provider | evaluator control |
| artifacts and downloads | sandbox/CSP checks | native safe viewer/export | schema/storage checks |
| graph snapshot, delta resume, trace correlation, redaction | required | required | API conformance |
| disconnect/reconnect/provider loss | required | required | fault injection |
| mount/unmount/replacement/rollback and resource leaks | browser/host scopes | native scopes | race/leak harness |

The same scripted fixture session must be runnable first through the browser
and then through the macOS client against one unchanged server process. Both
runs must attest the same server Graph IR and compatible negotiated
capabilities, while separately attesting their client graph and platform
implementation identities.

Presentation performance is part of the benchmark evidence. At minimum record:

- endpoint-to-first-played-audio and server-output-to-playout delay;
- audio underflow/overrun, playout gaps, and captured frame drops;
- video capture-to-admission age and client/transport queue occupancy;
- reconnect detection, recovery, and lost/duplicated event counts;
- reducer-to-view snapshot latency, long tasks, and bounded memory growth;
- snapshot/delta/trace rendering latency; and
- server and client overhead with observability disabled, sampled, and at the
  release profile.

### 8.1 Performance-evidence service contract

Instrumentation is an optional client plugin service, not a view concern. The
contract is `presentation.client.performance_evidence`, with schema identity
`openrealtime/presentation/client-performance-evidence/v1`. A selected
provider exposes a scoped payload-free recorder and immutable aggregate
snapshots. The runtime binds every recorder to the selected manifest entry,
implementation artifact, configuration digest, and mount generation; calling
plugins cannot assert those identities themselves.

The recording API is deliberately small:

```text
record duration(metric enum, integer nanoseconds, fixed dimensions)
add counter(metric enum, integer delta, fixed dimensions)
set gauge(metric enum, integer value, fixed dimensions)
set coverage(metric enum, measured|simulated|unsupported, reason enum)
snapshot immutable bounded aggregate
```

Metric names, units, dimensions, coverage, provenance, and unsupported reasons
are closed enums. Arbitrary labels and error strings are forbidden. A probe
that spans multiple services owns bounded local correlation tokens, completes
the duration, and records only the aggregate; tokens never enter evidence. A
view or exporter receives a declared read-only dependency. Upload, storage,
network access, and device access belong to separate explicit sink or sensor
plugins and are never permissions of the recorder.

`disabled` omits both provider and probes entirely; a no-op recorder would
contaminate the disabled-overhead measurement. `sampled` and `release` select
exact collector/probe implementations and identity-bound deterministic
sampling configuration. Provider loss invalidates its scoped recorders, and a
remount starts a new generation so snapshots cannot silently combine two
implementations.

Every canonical evidence envelope identifies:

- evidence class: exactly `deterministic_fixture`, `browser_runtime`, or
  `macos_native`;
- the evidence contract/digest and snapshot fingerprint;
- the clock domain, resolution, provenance, and any cross-host error bound;
- client platform plus exact profile, lock, plan, manifest, collector,
  implementation artifact, configuration, generation, and observability mode;
- the runtime-injected identity of each contributing selected source entry;
- fixture identity when applicable, fixed aggregate series, explicit coverage,
  and rejected/dropped-record counters.

The canonical client manifest remains the content-addressed authority for the
complete client composition. Server Graph IR and capability identity come from
the existing execution/live evidence, not from a client collector. A
browser-then-macOS run compares the unchanged server Graph IR fingerprint but
retains distinct client/platform identities.

Evidence stores fixed integer histograms (`count`, `sum`, `min`, `max`, fixed
buckets), counters, and gauges rather than raw samples. Initial hard ceilings
are 128 sources, 256 series, four fixed dimensions per series, 256 pending
probe correlations, 4,096 observations per series with saturation, and a
256-KiB canonical snapshot. Durations are non-negative and capped at 24 hours;
backward clocks, overflow, unknown enums, identity drift, and bound violations
increment typed rejection counters. Evidence never retains event payloads,
text, transcripts, tool arguments, credentials, URLs/hosts, session/response/
item IDs, paths, device names, or arbitrary diagnostics.

Metric boundaries are normative. They include endpoint-to-first-rendered-audio
and server-output-to-rendered-audio; audio gap, underflow, overrun, and capture
drop counts; video capture-to-server-admission; client message/byte queues;
reconnect detect/recover and sequence-proven lost/duplicate events;
reducer-publish-to-view-commit; long task/main-loop stall; memory baseline,
current, high-water, and growth; and live/delta/trace render durations.
Received RTP bytes or decoded audio are not "played audio". An audio buffer
scheduled or completed without a first-sample render boundary is named as such.
Timestamps from different hosts are never subtracted unless the evidence states
a valid synchronization/error bound. An acknowledgement measures
capture-to-admission-ack, not one-way admission. Missing support is
`unsupported`, never a numeric zero.

A single virtual integer-nanosecond corpus must produce byte-identical
snapshots in Go, JavaScript, and Swift and cover every metric plus unknown
enum, backward/overflow time, unmatched/duplicate span, bounds, redaction,
sampling, provider-loss/remount, immutability, and identity-mismatch failures.
Full-workload benchmarks compare otherwise identical disabled, sampled, and
release profiles. Browser runtime evidence distinguishes fake from physical
devices. Portable Swift on Linux can produce only deterministic-fixture
evidence; only a signed Darwin run with executable/signing and sensor
attestation can emit `macos_native`.

Any material correctness, safety, deadline, latency, queue, media-quality, or
resource regression blocks release. Preserve the failing artifact, diagnose it
with the exact client/server graphs and causal trace, rerun the affected slice,
then rerun the complete relevant suite. Presentation benchmarks do not replace
the project-wide scenario, Meeting Assistant, Realtime-CU, FDB, FD-Bench, or
tau2/τ-Voice matrix defined by the parent plan.

## 9. Definition of done

- [x] The realtime gateway has no presentation-specific handler, asset, route,
  command flag, or client-name branch.
- [ ] Browser hosting, relays, local effects, artifacts, and every client
  capability are replaceable descriptor-locked plugins with tested scoped
  disposal and permission ceilings. Locked minimal, observer, developer
  WebSocket, and developer WebRTC browser profiles boot in real Chromium, and
  shuffled race gates cover the shared host, reducer, and realtime client.
  The shipped effects and artifact-reference providers now pass an atomic
  real-Chromium replacement, signed-catalog renegotiation, real artifact
  execution, and zero-active-socket plus zero-scoped-effect final cleanup. The
  shipped WebSocket, WebRTC, and management relays also pass
  pre-mount refusal and active-session/request replacement with exact
  zero-worker retirement under a stable router/export. The immutable module
  store, client manifest, and browser shell now pass an atomic three-row swap
  with byte-identical routes, stable router/export identity, and exact
  zero-ownership retirement. The shipped loopback listener also passes
  permission refusal, real-port replacement under the same router/export, exact
  zero-ownership retirement, and old-port closure. The endpoint directory and
  secret credential likewise pass atomic replacement through a live relay with
  exact closure retirement and target/authorization rebinding. The stateless
  host router passes activation rollback and complete route-and-listener closure
  replacement on one real address, with exact retirement and inert predecessor
  handlers. The fail-closed effect-authority provider likewise replaces through
  its active effects-socket closure with a stable router/export, exact two-row
  retirement, and a fresh deny-by-default socket. One authenticated twenty-three-
  row real-Chromium transition now selects exact candidates for slots, session
  configuration, effects, artifact references, debug, inspection, the stateful
  management-operator authority, management transport/static/authoring/source-
  read/source-publication, the stateful authoring workspace, and all nine
  effects-enabled WebSocket-profile view consumers; every text, confirmation,
  artifact, inspection, trace,
  operator, editor, configuration, and canvas surface rebinds its retained
  services and final disposal reaches zero client ownership. A separate eleven-
  row real-Chromium WebRTC transition selects slots, session configuration,
  media, transport, video protocol, debug, effects, artifact references,
  inspection, video controls, and transport diagnostics. It retires the active
  media/transport dependency closure, restores the unchanged durable workspace
  and reducer through two byte-free state transfers, remounts the reducer
  disconnected, and then establishes a fresh offer before restoring inspection,
  effect negotiation, camera capture, response audio, and sealed-artifact
  execution.
  Every shipped browser view consumer now has selected-
  implementation replacement evidence; the replacement slots and session-
  configuration providers reconstruct their DOM/contribution state, the
  replacement inspection/static clients reload exact live/catalog evidence,
  and three digest-bound state transfers preserve the reducer, private operator
  capability, and authoring document while invalidating derived authoring
  results.
  A separate authenticated reducer-only replacement migrates its bounded
  canonical machine/outbound/cursor/local-item state and private expiring
  inspection lease through an exact schema, retains the connected WebSocket
  session without another realtime handshake, and remounts all dependents with
  no capability bytes in evidence. It refuses snapshots during reconnect,
  response, playout, pending-tool, or unsent-command work.
  The same transition replaces the active WebSocket transport at a safe point,
  observes a disconnected remounted reducer, and establishes exactly one fresh
  protocol session before resuming inspection, effects, and conversation.
  Reversible management-operator loss privately suspends both declared states,
  omits their bytes from live evidence, restores them across the remounted
  dependency closure, and remains retryable after an intentionally failed
  stateful remount. Deployment-host topology, server-sealed authority rotation,
  topology/state-schema-changing client replacement, and signed native
  lifecycle/leak evidence keep this parent open.
- [ ] The browser and macOS applications connect to the same unchanged server
  APIs and pass the shared protocol/client conformance corpus. The public
  `companion` command now supervises the real server and host while real
  Chromium WebRTC plus an exact manifest-derived native WebSocket probe use one
  unchanged server; this is not evidence that a signed Darwin application
  connected.
- [x] The browser application is assembled from a host-served locked client
  graph; obsolete standalone presentation forks have no production command or
  route.
- [ ] The macOS application is assembled from the same logical service
  contracts with native implementations and has real macOS release evidence.
  A native provider registry/factory seam and observer/effects profile split
  now exist. The provisioned gate now requires a gate-built exact server and a
  nonce-bound, create-only contract/receipt for ordered browser then signed
  native sessions, with distinct media, permission, continuation, tool,
  inspection, and cleanup evidence. Linux Swift and portable receipt-verifier
  tests are not substitutes for actually passing that Darwin run.
- [ ] Static Graph IR, live snapshots/deltas, causal traces, and authoring are
  clean versioned APIs usable without any shipped UI. The default compiled
  server profile now exposes session live/delta/trace through exact outer-realm
  plugins, and the separate operator overlay supplies the other local API
  families. All six operator API route consumers now pass one atomic six-row
  replacement with stable router/export and provider identities, unchanged
  method admission, and exact zero-ownership retirement. Their stateless root
  router also passes activation rollback and successful full six-API closure
  replacement while all providers stay active and retired handlers become
  inert. Every operator request is now lifecycle-owned, and all seven provider
  bindings pass one atomic thirteen-row provider/API closure replacement after
  draining and joining a blocked source read while the router/export stays
  exact. A separate eight-request transition concurrently blocks and drains
  every operator API family, including three unique live/model/trace workers in
  one session-API scope, before replacing all six routes. Replacing the session
  provider around the same external registry also preserves its registered
  runtime and later mutations without transferring registry ownership to the
  plugin realm. The same continuity now covers deployment-owned operator
  capabilities across authorizer replacement, including live revocation and
  fresh scoped issue. Strict production launch profiles can now opt into a
  separate environment-delivered `mgmt_` operator capability: startup retains
  only its hash, rejects both environment and resolved-value aliasing with the
  gateway bearer, and overlays the exact graph/schema/descriptor catalog plus
  pure in-memory authoring while excluding session inspection, source I/O, and
  reconciliation. Scenario, Meeting Assistant, and Realtime-CU profile
  generators expose that same selection. The shipped inspection view now reads
  a bounded 256-event resumable delta page beside live/static evidence, rejects
  session, graph, fingerprint, cursor, baseline, and event-sequence
  substitution, and passes isolated JavaScript plus complete real-Chromium
  developer/observer profile coverage. Reconciliation candidates now cross a
  shared strict Go request validator, the server and headless client, an exact
  POST-only host relay, and the descriptor-locked browser management transport.
  The host independently validates the request-bound receipt before emitting
  non-secret identity evidence; the browser independently rebinds that evidence
  and receipt. This transport/API coverage does not provide a production
  session reconciler or prove a graph-routing safe-point swap. Reconciliation
  E2E, cross-owner provider-state migration, and topology-changing client
  replacement remain open.
- [x] View plugins have no implicit effect authority; the authority host still
  proves admission, target, confirmation, ledger, dispatch, and audit. The
  descriptor-locked observer WebSocket and WebRTC profiles omit the effects,
  artifact-reference, confirmation, and artifact view plugins and advertise no
  effects endpoint. Effects-enabled profiles must separately mount the typed
  authority service and exact permission grants; the host retains declaration
  admission, confirmation, target fencing, idempotent execution, and audit,
  while the server-sealed receipt bridge binds each accepted call to the exact
  session, declaration, arguments, and target. Focused browser-profile and
  effects-host authority tests pass without a provider or browser credential.
- [ ] Cross-client realtime/media/tool/inspection/reconnect/reconciliation E2E
  tests and presentation performance gates pass without regression. Focused
  real-Chromium WebSocket/WebRTC media, tool, inspection, authoring, artifact,
  reconnect/dispose, JavaScript, Swift-Linux, shuffled race, and host/client
  benchmark gates are green. The public companion command additionally crosses
  the real subprocess boundary and proves Chromium WebRTC plus native-profile
  WebSocket traversal, clean gateway routes, two sessions, and bounded
  shutdown cleanup. A fresh 2026-08-30 candidate run retained passing
  release reports for `local.presentation.chromium`,
  `local.presentation.shared-server`, `local.client.javascript`, and
  `local.client.swift-linux` in `.runtime/release-validation/*-candidate-01`;
  the focused package matrix also passed normal, three shuffled runs, race,
  vet, and all presentation/client benchmarks. On 2026-08-31 the signed-native
  receipt contract and its portable tamper/replay verifier were added, but no
  Darwin receipt was produced on Linux. Signed native macOS, complete
  reconciliation/leak evidence, and the project-wide release matrix remain
  open.
