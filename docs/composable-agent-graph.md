# Composable Real-Time Agent Element Graph

- **Status:** implementation in progress; unchecked validation gates remain open
- **Scope:** the OpenRealtime runtime, component API, architecture catalog,
  configuration, inspection, benchmark, server, and client composition paths
- **Audience:** runtime authors, element and model-adapter authors, deployment
  authors, benchmark authors, and UI/tooling authors

This document proposes a thorough refactoring of OpenRealtime from a set of
binding-shaped realtime voice architectures into a typed, inspectable element
graph for general real-time agents. It is intentionally broader than audio. A
graph may observe and act through text, audio, cameras, screen or computer-use
video, still images, files, tool streams, user-interface state, and future
modalities without changing the kernel's model taxonomy.

The proposal preserves the strongest parts of the existing system: protocol
compatibility, the canonical trajectory, safe-point commits, asynchronous
event handling, interaction policies, duplex evidence, explicit authority,
the irreversibility ledger, sidecar isolation, provider adapters, and the
measurement discipline. It changes how those parts are assembled.

This document is both the accepted design and its implementation tracker. The
external Realtime API remains authoritative. Old internal implementations and
configuration switches are reference material only; production paths must use
the new graph, plug-in, and profile contracts, and unchecked boxes must not be
reported as complete.

## Executive summary

The central decision is:

> Make a small ClickNP/Verilog-inspired element-netlist language (`.ortg`) the
> default hand-authored topology format; type-check it against
> machine-readable element descriptors into an immutable Graph IR; and
> execute, validate, visualize, version, and reconcile that IR. Normalized
> YAML/JSON, visual editing, and optional code builders elaborate through the
> same compiler.

The design borrows the useful semantic ideas of Click Modular Router and
ClickNP—small stateful elements, multiple ports, explicit channels, bounded
depth, explicit `Tee` and `Mux`, reusable groups, and a separate management
plane. Its default syntax adopts the concise node-and-edge feel of ClickNP and
Verilog without copying ClickNP's fixed data type, positional assumptions, or
combined element-configuration language. It borrows from n8n the visual
workflow and per-node execution-inspection experience. It borrows from
DeepSeek Harness (`dsh`) and Cordis the plugin discipline, reactive dependency
management, reversible lifecycle effects, and safe dynamic composition.

The result is not a fixed ASR-to-LLM-to-TTS pipeline and not a prescribed
fast/slow architecture. A model, policy, context engine, codec, timer, state
store, authority check, media adapter, or ordinary computation is an element
with typed ports. A reusable policy is an element or a typed subgraph. “Fast,”
“slow,” “foreground,” “background,” “reasoning,” “non-reasoning,” “Omni,” and
“interactive” describe selected deployments or capabilities; they are not
kernel element species.

In particular:

- real-time behavior is achieved by explicit triggers, bounded channels,
  concurrency, deadlines, interruption, and interaction policy; it does not
  require one full-duplex end-to-end model;
- a full-duplex or speech-to-speech model is nevertheless a valid element and
  can run under native interaction, external interaction, or a composition of
  controllers;
- interaction policy is first-class because deciding *whether and when* to
  listen, speak, wait, interrupt, act silently, or yield is independent of the
  model that generates content;
- observation and action spaces are formal typed port signatures, including
  their temporal protocols, rather than lists of modality strings;
- trigger conditions are visible as connections to trigger ports; context
  arriving on a state port need not invoke a model;
- latency and price are evidence profiles attached to an exact element,
  configuration, hardware, and load condition, not promises encoded as port
  types;
- an output may have only one channel unless an explicit `Tee` copies it, and
  an input may have only one writer unless an explicit `Mux`, `Merge`, mixer,
  or arbiter defines how writers combine;
- a slow or deliberative model may feed context, TTS, native audio, tools, a
  feedback loop, or several of those through a `Tee`; no mandatory
  slow-to-fast handoff exists;
- configuration, topology, deployment placement, empirical profiles, and
  secrets remain separate concerns;
- Mermaid is an export format, not the semantic source of truth;
- the compiler and inspector make omitted interrupt, failure, timeout,
  authority, and output paths visible without dictating which valid choice a
  developer must make.

The design flow is:

```mermaid
flowchart LR
    Descriptor[Element descriptors] --> Elaborate[Elaborate and type-check]
    Netlist[Compact ORTG netlist] --> Elaborate
    Manifest[Normalized YAML or JSON] --> Elaborate
    UI[Visual graph editor] --> Elaborate
    SDK[Optional Go or TypeScript builders] --> Elaborate
    Elaborate --> IR[Immutable Graph IR]
    IR --> Catalog[Catalog and fingerprints]
    IR --> Validate[Static and startup validation]
    IR --> Runtime[Runtime and reconciliation]
    IR --> Render[Mermaid, DOT, SVG, canvas]
    Runtime --> Trace[Live traces and profiles]
    Trace --> Render
```

This diagram describes the toolchain rather than an agent topology. Agent
diagrams are generated from their Graph IR.

## 1. Problem statement

OpenRealtime already supports several important arrangements: componentized
ASR/LLM/TTS, speech-to-speech sidecars, external and native interaction,
background reasoning, visual observation, a bounded visual reflex, computer
use, and upstream Realtime endpoints. The existing abstractions made those
arrangements possible, but topology is still distributed across constructors,
large binding runtimes, architecture ownership vectors, command-line flags,
and prose invariants.

The consequences are structural:

1. Adding a new composition often means adding a binding branch or editing a
   large runtime rather than connecting existing parts.
2. The architecture catalog currently derives one of a few provider
   topologies and requires audio input, audio output, and turn generation. It
   cannot naturally identify a text-and-files agent, a silent visual actor, or
   an agent whose speech and computer-use paths use different cognition and
   interaction policies.
3. “Slow cognition is always the engine's” and “slow cannot speak” are enforced
   as global assumptions even though they are only choices of the current
   reference architecture.
4. The statement that interaction “carries no data” usefully separates content
   generation from control, but it hides the fact that interaction decisions,
   interrupts, deadlines, and acknowledgements must themselves travel through
   explicit, inspectable, typed control channels.
5. Configuration is a flattened view of a very large flag set. It records
   settings but cannot declare arbitrary topology, per-edge queueing, or a new
   reusable policy graph.
6. Capabilities are represented mainly as broad booleans and ownership
   columns. Those fields do not express the complete observation space,
   action space, temporal protocol, trigger relation, state, or effect
   authority of an arbitrary component.
7. Timing is partly embedded in control flow. It is difficult to inspect which
   event wakes which model, how a completed slow result reaches output, which
   work a barge-in cancels, or how channel depth contributes to latency.
8. The visual shape of a running architecture must be reconstructed from code
   and status fields. There is no single graph that can be type-checked,
   rendered, diffed, replayed, and observed live.

The refactoring must solve these problems without discarding the safety and
correctness properties already earned by the project.

## 2. Scope and non-goals

### 2.1 In scope

The element graph must represent agents using any combination of:

- typed user and system text, including partial and revised text;
- microphone or other continuous audio;
- synthesized, model-native, recorded, or mixed audio output;
- physical-camera video;
- screen, browser, virtual-display, mobile-device, and computer-use video;
- still images attached to a turn or generated by a tool;
- files and multimodal attachments represented by durable handles and typed
  metadata rather than copied blindly through every channel;
- model-native latent or interaction state where a provider exposes only an
  opaque capability boundary;
- tool calls, tool results, jobs, timers, schedules, and environmental events;
- computer, browser, device, API, and message actions;
- streaming, non-streaming, revising, stateful, request/reply, and segmented
  protocols;
- model and non-model computation;
- local, sidecar, remote, and distributed elements.

### 2.2 Non-goals

This proposal does not:

- select one universally best agent graph;
- require every model to support every modality;
- equate real-time with audio, streaming tokens, or full duplex;
- make latency statically provable through the graph type system;
- automatically insert semantic adapters whose choice changes behavior;
- make irreversible real-world actions reversible;
- replace the OpenRealtime/OpenAI-compatible wire protocol with the internal
  graph protocol;
- require live topology editing in the initial implementation stage;
- expose model-private reasoning text to other elements unless a provider
  explicitly offers a safe, typed output for it;
- remove named reference architectures or benchmark identities; it makes
  those identities graph-based rather than binding-species-based.

## 3. What “real-time agent” means

Real-time is a relationship between an observation, a decision opportunity,
and a deadline after which an action is stale or disruptive. It is not a model
family.

Examples include:

- starting an answer soon enough to preserve conversational rhythm;
- deciding that no answer is appropriate while someone else is speaking;
- cancelling or redirecting speech when the user corrects the premise;
- clicking a transient interface control before it disappears;
- warning about a camera event before it is no longer actionable;
- incorporating a tool result without waiting for another user turn;
- updating a text UI incrementally while a deeper answer continues;
- declining to act because evidence or authority did not arrive before a
  deadline.

A componentized system can meet these requirements. For example, audio input,
ASR, an interaction model, a small response model, TTS, and a background
reasoner can all run concurrently on bounded channels. The agent can continue
observing while audio is playing, and an external interaction policy can
cancel or defer output. None of those properties requires the content model
itself to be full duplex.

A full-duplex multimodal model can reduce boundaries and preserve native
audio information, so it remains an important option. It does not remove the
need for explicit authority, tool execution, audit, state, background work,
external policies, or measured latency. The graph must let a developer select
native interaction, external interaction, or a deliberate composition over
the same foreground model.

## 4. Design principles

### 4.1 Composition, not taxonomy

The kernel knows elements, ports, channels, state, lifecycle, and effects. It
does not switch on names such as `cascade`, `omni`, `duplex`, `fast`, or
`slow`. Those remain library graphs, deployment roles, or evidence labels.

### 4.2 Observation and action spaces are typed

An element's input ports form its observation space. Its output ports form its
action space. The types include temporal protocol and provenance, not only a
modality label. A graph's external ports define the observation and action
spaces of the composite graph.

### 4.3 Triggering is explicit

Receiving context and being invoked are different operations. Trigger,
interrupt, deadline, and acknowledgement ports make activation and
cancellation paths visible. An element may also declare that a stream port is
intrinsically reactive, but that fact remains in its contract.

### 4.4 Timing is operational evidence

Queue depth, delivery mode, deadlines, and trigger relations are graph
semantics. Provider latency, cancellation latency, throughput, and cost are
empirical profiles. Static checks may reject an impossible hard contract or
warn about an unsupported SLO, but must not present a measured distribution as
a type-level guarantee.

### 4.5 No implicit branching or merging

Fan-out requires `Tee`. Multiple writers require a `Mux`, `Merge`, `Join`,
mixer, arbiter, or another element with defined semantics. The graph never
silently copies, interleaves, or chooses outputs.

### 4.6 Policy is ordinary computation

An interaction policy, endpoint detector, rollout selector, commitment rule,
tool policy, computer-use arbiter, or latency race is an element or subgraph.
It can be implemented by rules, code, a small model, a native model head, or a
composition. “Policy” conveys purpose, not a privileged runtime mechanism.

### 4.7 Choices must be visible, not prescribed

The compiler enforces mechanical correctness. Lint profiles expose omitted
design obligations. They do not require slow output to pass through fast
cognition, require a particular barge-in behavior, or prevent a deliberative
model from speaking. An intentional omission is represented by an explicit
`Ignore`, `Drop`, `Reject`, or terminal sink.

### 4.8 One semantic representation

The default `.ortg` source, normalized YAML/JSON, a visual editor, and optional
code builders elaborate the same Graph IR through one type checker. Runtime
execution, rendering, architecture fingerprints, validation, traces, and
benchmarks use that IR. Mermaid and DOT are generated views and cannot drift
into competing definitions.

### 4.9 Configuration is owned locally

Each element owns a typed configuration schema. The graph connects instances.
Deployment config selects implementations and resources. Performance profiles
record evidence. Secrets are resolved at mount time and never enter graph
fingerprints or exported diagrams.

### 4.10 Lifecycle is compositional

Elements declare dependencies and acquire resources inside a scoped lifecycle.
Removing or replacing an element cancels work and reverses its runtime
registrations. Real-world effects that already crossed the action boundary are
recorded and repaired where possible, never described as reversible.

### 4.11 Compatibility remains a boundary

The gateway continues to translate external protocol events into internal
typed messages and typed graph output into protocol events. External protocol
compatibility must not constrain every internal element to wire-level event
shapes.

## 5. Foundations and related work

### 5.1 Click Modular Router and ClickNP

Click's durable contribution is the element graph: small modules with ports,
explicit wiring, reusable compound elements, and connector elements whose
semantics are visible. ClickNP extends the idea to a hardware-oriented data
plane with persistent element state, a data handler, a signal handler, bounded
channels, host control, and graph-level port counts.

OpenClickNP's topology makes several useful choices:

- `A -> B` is lossless and backpressures the producer when the channel is
  full;
- `A => B` is lossy and drops when the channel is full;
- source and destination ports can be addressed explicitly;
- channel depth can be overridden in topology;
- a `Tee` performs one-to-many copying;
- a `Mux` performs many-to-one arbitration;
- reusable element groups encapsulate subgraphs.

OpenRealtime should adopt those semantics and the source language's compact
netlist style where they fit, but not the language wholesale. ClickNP's fixed
flit type, positional conventions, and element configuration model do not fit
a general agent. OpenRealtime messages require strict payload and
temporal-protocol types, richer lifecycle, run correlation, cancellation,
authority, and empirically measured timing.

### 5.2 Verilog-style elaboration

Verilog source describes modules, ports, and nets, then an elaborator produces
the concrete design. The useful lesson is not to reproduce Verilog syntax; it
is that graph construction must be pure and separate from execution. No
OpenRealtime frontend may start goroutines, load models, open devices, or dial
services. It produces an immutable graph definition that is validated before
mounting.

### 5.3 n8n and visual workflow systems

n8n demonstrates discoverable node configuration, visual branching, explicit
switch and merge nodes, reusable sub-workflows, and per-node execution
inspection. Those affordances are valuable. A production realtime-agent graph
additionally requires strict pre-mount port types, temporal protocols, bounded
queue semantics, causal identities, cancellation, effect authority, clock
domains, safe feedback, and latency evidence.

### 5.4 DeepSeek Harness and Cordis

DeepSeek Harness (`dsh`) uses an everything-is-a-plugin architecture powered
by Cordis. The Cordis paper, *A Programming Paradigm for Spatiotemporal
Composability*, separates two concerns:

- **spatial composability:** components declare dependencies and react to
  dependencies appearing, disappearing, or changing;
- **temporal composability:** a component's tracked runtime effects can be
  unwound when the component is removed.

Cordis models these as reactive coeffects and revertible effects mediated
through a context. OpenRealtime should apply the principles to graph element
lifecycle: declared services activate elements, scoped effects own their
disposers, and graph reconciliation mounts and unmounts components safely.
The data/control graph and the dependency/lifecycle graph are related but not
identical and should be inspectable as separate layers.

## 6. Core terminology

| Term | Meaning |
| --- | --- |
| element | A stateful or stateless computation with typed input and output ports and a lifecycle contract. |
| instance | One named occurrence of an element definition with one configuration. |
| port | A directional, typed endpoint with a temporal protocol and cardinality. |
| channel | A bounded connection from one output port to one input port. |
| observation space | The product of the input-port types visible to an element or composite graph. |
| action space | The tagged product/sum of output-port types an element or composite graph may produce. |
| trigger | An input whose arrival creates a reaction or run rather than merely updating state. |
| interrupt | A run-scoped control message requesting cancellation, preemption, truncation, or another defined transition. |
| reaction | The contract relating a trigger to sampled inputs, state transitions, outputs, and terminal outcomes. |
| policy | An element or subgraph whose outputs select or constrain behavior. |
| subgraph | A graph exported as an element with typed boundary ports. |
| capability | Something an implementation can support, independent of whether a graph selects it. |
| Graph IR | The immutable, language-neutral, fully elaborated graph consumed by the runtime and tooling. |
| profile | Versioned empirical evidence about timing, capacity, reliability, or cost under stated conditions. |

## 7. Formal element contract

An element definition is modeled as:

$$
E = (\mathcal{O}, \mathcal{A}, \mathcal{R}, S, D, F, Q)
$$

where:

- \(\mathcal{O}\) is the typed observation space: input ports and their
  protocols;
- \(\mathcal{A}\) is the typed action space: output ports and their protocols;
- \(\mathcal{R}\) is the reaction relation: triggering inputs, state sampled
  atomically for each trigger, concurrency, interruption, and terminal
  outcomes;
- \(S\) is persistent internal state together with snapshot, compatibility,
  and optional migration schemas;
- \(D\) is the dependency or coeffect declaration: services, devices,
  providers, clocks, secrets, schemas, and authority the element requires;
- \(F\) is the effect declaration: registrations and resources that must be
  disposed, plus any external effect authority the element may request;
- \(Q\) is a set of empirical profiles keyed by exact implementation,
  configuration, placement, hardware, load, and measurement revision.

Models are not a separate formal category. A model element typically has a
larger \(\mathcal{O}\), a probabilistic \(\mathcal{A}\), and profiles sensitive
to reasoning configuration. A code context engine, timer, ASR adapter, media
resampler, or policy has the same contract shape.

### 7.1 Composite contracts

After internal edges are connected, the unbound exported inputs and outputs
form the composite graph's observation and action spaces. Dependencies and
effects are the checked composition of its elements. A subgraph may rename or
intentionally hide internal ports, but it cannot claim an external type or
authority its contents do not supply.

The runtime may estimate a composite performance envelope from channel bounds
and element profiles. That estimate is evidence, not part of type
assignability. The composite must be measured directly before production SLO
claims are made.

## 8. Port and protocol type system

Payload type alone is insufficient. These are different types even when all
carry strings:

```text
Event<UserUtterance>
Stream<TextDelta>
Segmented<TextDelta, RunID>
Revisions<Transcript, RevisionID>
State<ContextSnapshot>
Trigger<GenerateRequest>
Interrupt<RunID>
```

The initial protocol families should be small and explicit:

| Protocol | Semantics |
| --- | --- |
| `Event<T>` | One immutable occurrence, ordered within its channel. |
| `Stream<T>` | An ordered sequence of independently meaningful items. |
| `Segmented<T, K>` | Begin/delta/end or equivalent framed stream correlated by key `K`. |
| `Revisions<T, K>` | Successive replacements or refinements with explicit stability/finality. |
| `State<T>` | Latest-value state sampled by reactions; arrival does not imply a run. |
| `Trigger<T>` | Arrival opens a reaction with a run identity. |
| `Interrupt<K>` | A control request addressed to an active run or scope. |
| `Request<T, K>` / `Reply<T, K>` | Correlated request/reply without hiding the two channels. |

Types should also carry important media and authority distinctions:

- audio sample format, sample rate, channel count, framing, and clock domain;
- image/video encoding, dimensions or negotiated constraints, source, capture
  time, coordinate space, and grounding mode;
- text provenance, participant, stability, language, and visibility where
  semantically required;
- attachment handle, MIME type, content hash, retention scope, and access
  capability;
- tool proposal versus executable call;
- unconfirmed versus authorized action;
- prepared versus committed output;
- private model state versus user-visible content.

Common correlation metadata belongs in runtime envelopes rather than being
redeclared by every payload: session ID, source ID, turn or opportunity ID,
run ID, sequence, capture and receive timestamps, causal parents, trace ID,
and cancellation scope.

### 8.1 Assignability and adapters

All graph frontends use invariant, exact protocol types by default. Graph
source does not repeat those types: the elaborator resolves them from the
versioned element descriptors and reports errors at the relevant node, port,
or edge source location. If an output does not match an input, the developer
inserts an explicit adapter:

- `Resample` changes audio format;
- `Decode` changes media encoding;
- `Stabilize` turns ASR revisions into committed utterance events;
- `Collect` turns a segmented stream into a completed value;
- `Segment` turns completed text into safe speech units;
- `Describe` turns visual frames into text observations;
- `ResolveAttachment` provides bounded access to file contents;
- `Authorize` turns a validated proposal into an authorized action;
- `Commit` moves prepared output across the action boundary.

The compiler may suggest compatible adapters. It must not silently insert one
when the choice changes information, timing, authority, or failure behavior.

### 8.2 Runtime-negotiated types

Some formats and capabilities are known only after a provider or sidecar
handshake. A port then declares a bounded type constraint rather than an
untyped value. Startup resolves the constraint, records the selected concrete
type in the live graph, and refuses incompatible edges before the session is
made available. Negotiation never silently inserts a lossy conversion.

### 8.3 Degrees of composability

The compiler reports one of three outcomes rather than treating every mismatch
as equivalent:

- **directly composable:** output and input protocol types, constraints,
  cardinality, and authority match;
- **adaptably composable:** a registered adapter can bridge the types, but the
  author must select it because it changes representation, timing, stability,
  or authority;
- **not composable:** no sound adapter exists, the protocols conflict, the
  effect authority cannot be supplied, or a required temporal/placement
  contract cannot be met.

Timing SLOs do not change type assignability. A directly typed path may still
be unsuitable for a deadline, which is reported by profile validation.

## 9. Element families

Element families are library organization, not closed kernel enums:

- **sources and sinks:** gateways, microphones, cameras, screen capture,
  clients, files, schedules, speakers, displays, tool executors;
- **media transforms:** codec, resample, VAD, keyframe selection, chunking,
  segmentation, mixing;
- **perception:** ASR, OCR, speaker identity, visual description, object/event
  detection, document parsing;
- **state and context:** trajectory, latest state, windows, retrieval,
  multimodal context construction, memory projections;
- **cognition:** text LLMs, VLMs, speech-to-speech models, end-to-end
  multimodal models, planners, reasoners, classifiers, code logic;
- **interaction and control:** endpoint, floor, opportunity, barge-in,
  commitment, preparation, cadence, deadlines, gates, races, arbiters;
- **connectors:** `Tee`, `Mux`, `Switch`, `Merge`, `Join`, `Latest`, `Delay`,
  `Race`, media-specific mixers;
- **action and authority:** proposal validation, policy, confirmation,
  authorization, commitment, pacing, dispatch, audit, repair;
- **supervision:** timeout, retry, circuit breaker, health, fallback, restart,
  and terminal error routing;
- **adapters:** explicit bridges between otherwise incompatible types or
  external protocols.

An implementation can belong to several descriptive families. The runtime
cares only about its contract.

### 9.1 Composition dimensions the graph must cover

The first production catalog should exercise the following dimensions rather
than merely declare that the generic graph could support them:

| Area | Representative alternatives | Composition concern made explicit |
| --- | --- | --- |
| speech recognition | batch ASR, simulated partials, native streaming ASR, model-native transcription, no ASR | revisions versus committed utterances, endpoint ownership, cadence, audio format |
| text cognition | small non-reasoning model, low-latency reasoning model, slow deliberative model, code/rules | trigger, sampled context, reasoning configuration, authority, latency profile |
| multimodal cognition | VLM, speech-to-speech, end-to-end audio/video model, separate modality encoders | direct versus derived evidence, native state, streaming and interrupt support |
| speech output | non-streaming TTS, streaming TTS, model-native audio, recorded clips, no speech | safe segmentation, first-audio latency, pacing, truncation, mux versus mixer |
| visual observation | physical camera, screen stream, still attachment, keyframes, OCR, narration, direct pixels | source identity, capture time, coordinate space, latest-value/drop policy |
| computer use | direct visual reflex, deliberative planner, code automation, human control | action arbitration, stale-frame detection, target fence, confirmation, serialized device access |
| context | append-only trajectory, latest state, window, retrieval, per-model projection | update versus trigger, snapshot consistency, media handles, feedback |
| interaction | acoustic rules, transcript model, multimodal model, native head, human override | evidence selection, floor, deadline, act arbitration, cancel scope |
| files and attachments | eager parser, lazy resolver, retrieval index, multimodal model attachment | retention, access capability, content hash, size bound, untrusted provenance |
| execution | in-process code, sidecar, remote API, persistent job | placement, transport guarantee, retry/idempotency, cancellation, lifecycle |

This table is not a Cartesian-product test requirement. It is a review
checklist ensuring that interfaces are derived from real variations already
present or anticipated in the repository rather than from an audio pipeline.

## 10. Authoring model: concise netlist first, multiple frontends

No single authoring experience is best for element implementers, application
developers, operators, and visual-tool users. The project should standardize
one semantic compiler and offer several thin frontends. For checked-in graphs,
the default should match the thing being authored: a compact declarative
netlist, not a verbose object tree and not executable Go.

| Representation | Strength | Cost or limitation | Recommended role |
| --- | --- | --- | --- |
| `.ortg` element netlist | direct node/port/edge notation, concise diffs, easy visual correspondence | requires a small parser, formatter, and editor support | default hand-authored and documented topology |
| strict YAML/JSON graph form | generic parsers, schema tooling, easy for controllers to emit | repeats edge field names and obscures connection-heavy graphs | normalized interchange and GitOps/API form |
| visual editor | discoverable ports, immediate shape, live diagnostics | poor for bulk generation and text review | first-class frontend over the same graph model |
| Go builder | strong IDE support for built-ins and natural for repository tests | recompilation, Go-only audience, no faithful visual round-trip | optional internal SDK and graph generator |
| TypeScript builder | broad application-developer familiarity and UI ecosystem | executable build step and another toolchain | likely first external code SDK if demanded |
| Mermaid | excellent documentation rendering | lacks executable port, type, lifecycle, and channel semantics | generated view only |

### 10.1 Why the netlist should be the default

Most topology lines describe a connection. YAML expands one connection into
`id`, `from`, `to`, and `channel` fields, so the syntax becomes more prominent
than the graph. A Click-like line maps directly to the visual relation and is
easier to scan, review, and rearrange:

```text
deliberative.text -> deliberative_fork.in;
```

This does require project-owned syntax tooling. The cost is justified only if
the language stays deliberately small. OpenRealtime Graph (`.ortg`) is a
declarative netlist, not a general programming or configuration language. It
has node declarations, explicit edges, imports, subgraph boundaries, and
comments. It has no loops, conditionals, variables, arbitrary expressions,
provider settings, secrets, or runtime effects. Ordinary edges have exactly
one visible choice: `->` is lossless and backpressured; `=>` is lossy and
best-effort.

The hard part—the descriptor resolver, type checker, lifecycle validator, and
Graph IR compiler—is required for every frontend anyway. The `.ortg`-specific
work is consequently bounded to parsing, formatting, source mapping, syntax
highlighting, completion, and refactoring. A parser, canonical `ort graph fmt`,
and precise diagnostics are release requirements for making it the default;
a half-supported DSL would be worse than YAML.

The syntax should be inspired by Click/ClickNP rather than source-compatible
with it. OpenRealtime needs named typed ports, temporal protocols, trigger and
interrupt paths, versioned descriptors, authority types, and richer channel
semantics. Reusing ClickNP's entire grammar would import assumptions that do
not fit a general agent graph.

### 10.2 Why Go should not be the default

Go remains an excellent language for implementing in-process elements and
generated graph libraries. It is not the most developer-friendly deployment
format:

- changing a queue or model connection should not require rebuilding the
  server;
- application and model developers may use Python, TypeScript, Rust, or a
  sidecar rather than Go;
- a visual editor cannot reliably rewrite arbitrary Go while preserving its
  abstractions and comments;
- loading executable authoring code complicates sandboxing and reproducible
  deployment;
- Go compile-time types cover only linked native elements, while configured
  plugins, remote implementations, and negotiated sidecars must still use the
  language-neutral descriptor type checker.

The formal guarantee belongs to element descriptors, the graph elaborator,
and Graph IR. A typed Go SDK can catch some mistakes earlier, but it is a
convenience frontend rather than the foundation of soundness.

### 10.3 Element definitions and descriptors

An element implementation is code, but its composition surface is a
language-neutral, immutable descriptor. Go elements should derive or register
that descriptor from typed declarations; sidecars and remote plugins publish
the same descriptor during packaging and confirm it during handshake. Graph
authors should not copy port types into every graph.

An illustrative descriptor fragment is:

```yaml
type: cognition.TextModel
ports:
  context:
    direction: input
    protocol: State
    payload: context.Snapshot
  trigger:
    direction: input
    protocol: Trigger
    payload: cognition.Generate
  cancel:
    direction: input
    protocol: Interrupt
    key: flow.RunID
  text:
    direction: output
    protocol: Segmented
    payload: text.Delta
    key: flow.RunID
reaction: cognition/model-generation
configSchema: cognition/text-model-config
```

The complete descriptor also carries cardinality, state, dependency, effect,
authority, lifecycle, and negotiation declarations. A graph can reference a
contract descriptor while deployment selects any implementation that proves
it satisfies that contract. Implementation-specific values extend the
contract's configuration schema and are resolved before mount; they cannot
silently change the node's declared ports.

Human graph source uses stable symbolic element names, with no `@1` or `@2`
suffixes. Exact descriptor revisions and content digests belong in a generated
lockfile and the elaborated Graph IR. This keeps source readable while
preserving reproducibility: normal builds consume the lock, and an explicit
`ort graph update` operation changes locked element revisions.

### 10.4 Default `.ortg` topology syntax

The graph source contains only topology: stable node instances, symbolic
element or subgraph references, exported boundary ports, explicit edges, and
lossless/lossy arrows. It does not contain provider credentials, prompts,
placement, empirical measurements, type annotations, connector counts, or
element revision suffixes.

For example:

```text
graph fast_and_deliberative_voice {
    interaction.Opportunity      :: opportunity;
    flow.Tee                     :: trigger_fork;
    cognition.TextModel          :: fast;
    cognition.TextModel          :: deliberative;
    flow.Tee                     :: deliberative_fork;
    context.TrajectoryProjection :: context;
    speech.UtteranceMux          :: speech_arbiter;
    speech.TTS                   :: tts;

    opportunity.generate     -> trigger_fork.in;
    trigger_fork.out         -> fast.trigger;
    trigger_fork.out         -> deliberative.trigger;
    fast.text                -> speech_arbiter.in;
    deliberative.text        -> deliberative_fork.in;
    deliberative_fork.out    -> context.background;
    deliberative_fork.out    -> speech_arbiter.in;
    speech_arbiter.out       -> tts.text;
}
```

The initial grammar can remain approximately this small:

```text
graph    := "graph" name "{" { node | edge | boundary } "}"
node     := element_reference "::" name ";"
edge     := [ "edge" name "=" ] endpoint arrow endpoint ";"
arrow    := "->" | "=>"
endpoint := node "." port
```

The full grammar also needs imports, exports, comments, and optional explicit
edge names, but no general expression evaluator. Edge IDs are derived
deterministically from endpoints unless `edge name =` is supplied for a
configuration overlay or stable telemetry. The arrow's delivery semantics are
stored on the IR edge and shown by the inspector.

Port types are absent because repeating them would permit drift. The compiler
resolves `fast.text`, `speech_arbiter.in`, and every other port from locked
element descriptors. Explicit `Tee` and arbiter nodes preserve the fan-out and
multi-writer rules.

`Tee.out` and `UtteranceMux.in` are homogeneous variadic port groups. Each
edge reference materializes one lane, so the compiler infers their exact arity
and unifies their generic payload/protocol type from neighboring ports. There
is no `outputs = 2`, `inputs = 2`, or numeric lane index in normal source. If
inputs have different meanings—such as `primary` and `fallback`—the element
descriptor declares different named ports instead of relying on positions.

### 10.5 Use YAML for values and interchange, not primary topology

YAML is a good fit for hierarchical configuration values and deployment
overlays. It is less effective as the primary surface for an edge-heavy graph.
The compiler should nevertheless accept and emit a normalized strict YAML 1.2
graph form, with equivalent JSON, for controllers, APIs, generated workflows,
and teams that standardize on generic data formats. Duplicate keys, custom
tags, merge keys, and ambiguous implicit scalar conversions are rejected.

The topology above is reusable across models and environments. Runtime values
are a separate, schema-checked artifact keyed by stable node ID:

```yaml
apiVersion: openrealtime.ai/config/v1alpha1
graph: fast_and_deliberative_voice
nodes:
  fast:
    model: deepseek-chat
    reasoning: none
  deliberative:
    model: deepseek-reasoner
    reasoning:
      effort: high
  speech_arbiter:
    policy: segment-lock
  tts:
    voice: alloy
```

Deployment binds implementations, placement, resources, and secret
references without changing functional topology:

```yaml
apiVersion: openrealtime.ai/deployment/v1alpha1
graph: fast_and_deliberative_voice
nodes:
  fast:
    implementation: providers.deepseek.text
    placement: llm-pool
    secrets:
      apiKey: secret://deepseek/api-key
  tts:
    implementation: providers.openai.tts
    placement: media-pool
```

Queue depth is not repeated on every connection. Each protocol/port contract
has a bounded default. An author who has measured a reason to override it may
use a separate channel overlay keyed by an explicit or endpoint-derived edge
ID:

```yaml
graph: fast_and_deliberative_voice
edges:
  deliberative.text->deliberative_fork.in:
    depth: 8
```

The compiler resolves this overlay before validation and records the effective
bound in Graph IR and its deployment fingerprint. Strong typing can infer the
channel's data type and connector arity; it cannot infer the operationally
correct queue depth under a particular load, so a bounded default plus an
exception-only override is the honest abstraction.

The boundaries are deliberate:

- **connector arity and channel type** are inferred from connections and
  descriptors, so they are not configuration;
- **element values** such as model, prompt, reasoning effort, voice, endpoint
  threshold, or arbitration policy belong in an element-owned config schema;
- **delivery** is expressed directly by `->` or `=>`; exceptional channel
  depth overrides live in the channel overlay because depth affects timing
  and liveness but normally uses a bounded default;
- **implementation, placement, transport, resource limits, and secret
  references** belong to deployment;
- **latency, capacity, reliability, and cost** belong to immutable evidence
  profiles.

Element config is data, not arbitrary executable code. If a behavior requires
code, it should be an element or reusable subgraph with a typed contract.
Builder SDKs may construct typed config values, but the result is validated
against the same schema and recorded as data.

### 10.6 Elaboration provides the type guarantee

The compiler performs these steps before any resource is acquired:

1. parse `.ortg`, normalized YAML/JSON, or a builder AST with precise source
   locations;
2. resolve locked element and subgraph descriptors;
3. infer generic types and materialize variadic connector lanes from edges;
4. resolve channel defaults, overrides, and exported boundaries;
5. type-check direction, protocol, payload, cardinality, authority, and
   negotiated constraints;
6. validate cycles, reactions, required ports, configuration schemas, and
   selected lint obligations;
7. freeze immutable Graph IR and its deterministic fingerprint.

An error should read, for example, “`deliberative.text` produces
`Segmented<TextDelta, RunID>` but `tts.audio` accepts
`Stream<AudioFrame>`; insert a TTS element,” and point to the edge. This is a
formal pre-mount type check, not duck typing. It is performed by the graph
compiler rather than depending on a particular frontend language.

### 10.7 Optional programmatic builders

Programmatic builders remain useful for parameterized graph libraries, tests,
and large generated topologies. The first can be a Go SDK because the runtime
is written in Go; a generated TypeScript SDK may be more approachable for
external application developers. Both use descriptors to expose typed port
handles and emit the same Graph IR.

Wiring must use an explicit operation such as
`graph.Connect(g, output, input, options...)`. Raw assignment cannot capture
edge identity, depth, delivery, diagnostics, or provenance. Builder execution
is pure elaboration: it does not start goroutines, load models, open devices,
or dial providers. The emitted `.ortg`, normalized YAML/JSON, or IR—not the
builder process—is what deployment reviews, signs, and mounts.

### 10.8 Visual editing and subgraphs

The visual editor uses element descriptors for its node and port palette and
invokes the same compiler on every edit. It can save canonical `.ortg` through
the formatter or normalized YAML/JSON. Non-semantic node positions and
collapsed groups live in a separate layout artifact or excluded annotation so
they do not change graph identity. Mermaid, DOT, and SVG remain generated
views.

A versioned subgraph is instantiated exactly like an element. Its descriptor
exposes typed boundary ports and structural/config parameters. The IR retains
hierarchy so an inspector can collapse or expand it. Language macros, YAML
anchors, and a general template evaluator are not the composition mechanism.
Reference architectures, interaction policies, computer-use lanes, and
provider stacks should be reusable subgraphs. Algorithmic generation remains
available through an SDK that emits a concrete graph for review.

### 10.9 Immutable authoring snapshots and recovery boundary

Interactive authoring uses the UI-independent management Authoring API rather
than granting a renderer filesystem or compiler authority. Each analysis
response is bound to the exact input bytes by a SHA-256 source digest and owns
independent copies of its diagnostics, catalog metadata, resolved values
properties, and text edits. Results are deterministically ordered and bounded;
the server and client validate the complete response envelope before admitting
it as an authoring snapshot.

The strict parser remains the only path to compilation. When an in-memory
`.ortg` buffer is incomplete, a separately bounded recovery parser synchronizes
only at statement terminators and the graph boundary and retains only complete
declarations. It may support explicit descriptor-derived completions, but it
cannot produce Graph IR, a resolution lock, a formatter rewrite, reconciliation
input, or any runtime effect. A formatter edit exists only for a buffer accepted
by the strict parser; it is an atomic edit set bound to the original path,
source digest, exact source range, and old text, so applying it to a stale buffer
fails closed.

Element configuration remains a separate values artifact. When a deployment
provides a bounded schema resolver, analysis validates the complete
self-contained Draft 2020-12 object schema before exposing directly declared
root-property metadata, canonical property subschemas, defaults, enums,
requiredness, schema identity, and content digest. External references and
invalid schemas admit no property metadata and produce diagnostics. Composition
is never flattened speculatively: the projection is marked incomplete and the
exact property subschemas remain authoritative. An absent resolver is reported
truthfully as unresolved rather than inventing fields.

The browser configuration editor, language-service LSP hover projection, and
native SwiftUI editor render the complete contract. The LSP form uses the
protocol's zero-based UTF-16 positions and deterministic plaintext so
descriptor-supplied markup is data, preserves absent versus explicit `null`
JSON, and fails closed on a split surrogate, malformed retained schema, or
oversized projection. A closed in-memory workspace index now resolves imports,
import aliases, nested graph declarations, and subgraph boundary definitions
against exact version/source-digest identities without discovering or opening
paths. Remaining work includes broader visual editing and mediation of actual
file writes. None of that may weaken the strict compile/reconcile boundary
above.

## 11. Graph IR

Graph IR is the canonical executable and inspectable representation. It is
versioned independently of any authoring frontend.

At minimum it contains:

```text
GraphDefinition
  format version
  graph ID, revision, lineage, and content fingerprint
  exported boundary ports
  nodes
    stable instance ID
    element type URI and immutable revision
    implementation reference
    typed ports and reaction contract reference
    configuration reference and non-secret digest
    state schema and lifecycle contract
    dependency/effect declarations
  edges
    stable edge ID
    source and destination port IDs
    concrete or constrained protocol type
    delivery and ordering semantics
    bounded depth
    transport/placement reference
  nested graph boundaries
  validation profile references
  empirical profile references
```

Graph IR must not contain credentials, bearer tokens, raw private prompts, or
large media. It records references and digests sufficient for reproducibility
and evidence.

The same IR powers:

- runtime construction;
- static and startup validation;
- architecture catalog fingerprints and lineage;
- Mermaid, DOT, SVG, and interactive-canvas rendering;
- live graph inspection;
- trace replay and deterministic simulation;
- benchmark identity and comparison;
- graph diff and future hot reconciliation;
- descriptor and schema generation for `.ortg`, normalized, visual, and code
  frontends.

## 12. Channel and connector semantics

### 12.1 Bounded channels

Every data or control channel is bounded. Unbounded buffering converts
temporary overload into unbounded memory and stale action.

The normal connection syntax is intentionally only the two ClickNP arrows:

```text
producer.out -> consumer.in;  // lossless; backpressure on full
producer.out => observer.in;  // lossy; drop on full
```

- `->` applies backpressure and is the normal default;
- `=>` implements ClickNP-like best effort with a precisely documented
  dropped-item rule;
- the payload and temporal-protocol type are inferred from the endpoints and
  checked for exact compatibility;
- an explicit edge name is needed only when the endpoint-derived identity is
  insufficient for an override or stable telemetry.

`=>` is syntax, not permission to discard anything. The resolved protocol
contract declares whether loss is legal and what unit may be dropped. The
compiler should normally reject lossy trigger, interrupt, authority,
commitment, and effect-result paths, and reject dropping individual deltas when
that would corrupt segmented framing. Lossy delivery is natural for replaceable
camera frames or telemetry, but remains a checked semantic choice.

Depth is measured in protocol items. Every edge receives a bounded default
from its resolved port/protocol contract; inspection may translate it to media
time or estimated bytes where the item type permits it. An exceptional depth
override is separate channel configuration and does not require giving every
edge a profile name. The effective depth is part of the elaborated IR and is
therefore available to liveness validation, fingerprints, and traces.

Behavior such as latest-value coalescing, sampling, debouncing, batching, or
drop-oldest should normally be represented by an explicit element. This keeps
semantically important behavior visible and testable.

An external source may be physically unable to backpressure. Its element must
declare how capture overflow is handled. The compiler rejects a supposedly
lossless path if neither the source nor the path can honor backpressure within
its bound.

`Lossless` describes a live graph's bounded handoff, not magical exactly-once
delivery across process crashes. A persistent or replayable edge uses an
explicit journal/log element and stable item identities. Retried effectful
work requires an idempotency contract; the runtime must never infer that a
physical action is safe to repeat.

### 12.2 `Tee`

`Tee<T>` performs logical copying from one input to a variadic group of
outputs. The elaborator creates one output lane for every edge leaving
`tee.out` and infers `T` from the neighboring ports. For immutable envelopes
and large media, copying should use reference counting or copy-on-write rather
than deep copying. A strict lossless tee consumes an input only when every
lossless branch accepts it. A best-effort branch may use `=>` so an observer or
telemetry tap cannot stall the main path.

An ordinary singular output may not connect directly to several inputs.
Repeated connections from the declared variadic `Tee.out` group are distinct
inferred lanes, not implicit fan-out. Requiring the visible `Tee` forces the
author to confront backpressure and branch lifetime without making the author
count its outputs.

### 12.3 `Mux`, `Merge`, `Join`, and mixers

These are not interchangeable:

- `Mux<Event<T>>` selects one complete event from several inputs according to
  an explicit arbitration policy;
- `SegmentMux<T>` or `UtteranceMux<T>` locks or preempts at defined segment
  boundaries so token or audio fragments do not interleave accidentally;
- `Merge<T>` combines values only when an associative/commutative or otherwise
  declared merge law exists;
- `Join` waits for correlated inputs and defines timeout and partial-result
  behavior;
- `Race` selects a winner and defines what happens to losers;
- `AudioMixer` combines simultaneous samples rather than merely interleaving
  frames;
- `Switch` or `Demux` routes an item to selected outputs without copying it to
  every output.

An ordinary singular input may not have several direct writers. A connector
such as `Mux` declares a homogeneous variadic input group, and each incoming
edge materializes a distinct inferred lane. The connector documents ordering,
fairness, preemption, and cancellation without an `inputs = N` declaration.

### 12.4 Feedback and cycles

A cycle must contain an explicit causal break such as `State`, `Delay`, a
seeded queue, or an element whose reaction can proceed without consuming the
cycle. Queue depth alone does not prove liveness. The graph validator analyzes
cycles for missing initial state, unbounded amplification, and cancellation
scope. Stateful feedback is legal and visible; accidental combinational or
deadlocked feedback is rejected.

## 13. Triggers, timing, and interruption

### 13.1 Reaction contracts

A text model might expose:

```go
type TextModelPorts struct {
    Context In[flow.State[context.Snapshot]]
    Trigger In[flow.Trigger[cognition.Generate]]
    Cancel  In[flow.Interrupt[flow.RunID]]

    Text    Out[flow.Segmented[text.Delta, flow.RunID]]
    Final   Out[flow.Event[cognition.Response]]
    Tools   Out[flow.Stream[tool.Proposal]]
    Outcome Out[flow.Event[cognition.Outcome]]
}
```

Updating `Context` does not run the model. A `Trigger` opens a run and samples
the declared state consistently. `Cancel` is addressed to a run or scope.
`Outcome` reports success, cancellation, timeout, refusal, and failure as a
closed typed result.

A native multimodal model may instead declare audio or event streams as
reactive inputs, plus explicit commit and interrupt ports. The contract—not a
global `omni` switch—states its behavior.

### 13.2 Timing elements

The standard library should include visible elements for:

- periodic cadence and clock ticks;
- debounce, throttle, and sampling;
- acoustic and semantic endpoints;
- inactivity and quiet deadlines;
- turn projection;
- absolute and relative deadlines;
- timeout fallback;
- speculative races and adoption;
- safe-point commitment;
- playback pacing;
- scheduled and delayed jobs.

A deadline cannot make a model faster. It can bound how long the graph waits,
select a fallback, cancel stale work, and produce evidence about the miss.

### 13.3 Time model

Every runtime uses a monotonic clock for durations and deadlines. Wall-clock
time is metadata for audit and cross-system correlation, never the source for
an in-process timeout. Media and external events retain both capture/event time
and runtime receive time. Queue and service latency are measured from the
appropriate pair rather than conflated.

Ports and elements declare clock-domain constraints where they matter. A
join across microphone audio, camera frames, screen capture, and remote tool
events defines synchronization tolerance, lateness, watermark, and missing
input behavior. The runtime does not assume that the newest-arriving item is
the newest-captured item.

Staleness is a policy over event time, source revision, and target state. A
computer click planned from an old frame, an answer derived from superseded
speech, and a delayed notification may be type-correct but no longer valid.
`FreshnessGate`, source-revision checks, deadlines, and safe-point comparison
make those decisions visible.

### 13.4 Interrupt propagation

Interrupts are graph messages, not hidden global calls. A cancellable element
declares which scopes it accepts and how quickly it acknowledges. Connector
and subgraph boundaries declare whether an interrupt is forwarded,
translated, absorbed, or split.

The compiler and inspector should be able to answer:

- Which event triggered this run?
- Which state revision did it sample?
- Which active runs does this barge-in target?
- Does cancellation reach the model, TTS, queued speech, and playback?
- Which component acknowledged it, and after how long?
- Which output had already crossed an irreversible boundary?
- What fallback or repair path follows timeout or partial cancellation?

If a developer intentionally allows a model or tool to finish, an explicit
`IgnoreInterrupt`, detached cancellation scope, or background-job boundary
records that choice.

### 13.5 Management is a separate plane

Realtime trigger and interrupt ports are part of the typed graph. Management
operations—configuration reconciliation, health, state snapshot, metrics,
drain, and lifecycle—use a standard management interface attached to every
element. They must not be confused with conversational control. ClickNP's
host-signal idea informs this separation, but OpenRealtime need not use a
special topology sigil for ordinary inspection.

## 14. Interaction as a first-class control graph

Interaction is the set of decisions about when and whether other behavior is
appropriate. It includes more than endpointing:

- listen or continue listening;
- acknowledge without taking the floor;
- answer now, prepare, defer, or abstain;
- speak through overlap, yield, or interrupt;
- cancel, truncate, or repair output;
- act silently;
- ask for clarification or confirmation;
- wait for visual, tool, identity, or timing evidence;
- choose among competing speakers or action producers.

An interaction element consumes typed evidence and emits typed acts or control
messages. Its implementation may be predicates, an external model, a native
model head, or a composite. It does not generate the substantive answer unless
the developer deliberately combines those responsibilities in one element.

Floor ownership remains distinct. A component may detect a likely turn
boundary while another decides whether that opportunity should cause speech.
Likewise, concurrent input/output capability does not imply native
interaction, and native interaction capability does not force its selection.

When multiple controllers can emit the same act, an explicit arbiter is
required. Examples include predicate priority for emergency barge-in, a text
policy for semantic decisions, a native model head for prosodic overlap, or a
human override. Arbitration is topology, not a merged boolean vector.

The current named policies remain valuable but become elements or subgraphs:
trigger, preparation, rollout, floor, barge-in, commitment, repair,
backchannel, turn projection, deferral, overlap classification, and
interaction-act selection. Reference graphs choose and connect them. The
kernel does not keep a hard-coded master policy struct.

## 15. Model descriptors and selection

For each model implementation and exact configuration, a descriptor records:

1. **Observation space:** accepted typed modalities, protocols, context limits,
   attachment access, and whether inputs are incremental, stateful, or
   committed.
2. **Action space:** text, revisions, segmented text, native audio, images,
   video, tool proposals, interaction acts, state updates, or other outputs.
3. **Reaction contract:** trigger ports, continuously reactive streams,
   sampled context, maximum concurrency, interrupt support, and terminal
   outcomes.
4. **Reasoning capability:** supported modes such as none, hidden reasoning,
   effort levels, token budgets, or provider-specific controls. The instance
   configuration selects a supported mode.
5. **Authority ceiling:** which proposal types it may produce and whether an
   external authority element may ever promote them. A descriptor never grants
   effect authority merely because the provider can format a tool call.
6. **Empirical profiles:** cold and warm latency, first output, completion,
   cancellation acknowledgement, throughput, failure rate, concurrency,
   token/media rates, and cost.

Capabilities describe what is available. Connections and element
configuration describe what is selected. A model capable of direct vision may
receive only narrated vision in one graph; a native interaction model may be
controlled externally in another. Live status records both capability and
selection.

“Reasoning” is not a universal output modality. Hidden chain-of-thought stays
inside the provider. A model that exposes structured plans, confidence,
citations, or safe summaries does so through separately typed ports.

## 16. Latency, capacity, and cost

### 16.1 Temporal profiles

Latency belongs in versioned evidence profiles, not in type assignability. A
profile is keyed by:

- element implementation and adapter revision;
- exact model and provider endpoint;
- configuration digest, including reasoning mode and output policy;
- warm/cold state and cache condition;
- hardware and placement;
- offered concurrency and load;
- input shape and size distribution;
- measurement date, sample count, and uncertainty.

Useful measurements include:

- input-arrival to trigger;
- trigger to first accepted output;
- trigger to first committable output;
- inter-output cadence;
- completion latency;
- interrupt to acknowledgement and quiescence;
- queue wait and service time separately;
- cold start and recovery;
- deadline success, error, and stale-result rate;
- cost per trigger, unit time, token, media unit, or action.

### 16.2 Graph-level analysis

The compiler identifies causal critical paths, parallel branches, queue bounds,
and deadline elements. It can estimate distributions through trace replay or
simulation and flag a profile that cannot plausibly meet an SLO. It must not
naively add p95 values and call the result a p95. Correlation, queueing, races,
and cold starts matter.

Runtime traces are authoritative. Every item should retain capture, enqueue,
dequeue, trigger, first-output, commit, emission, cancellation, and completion
timestamps where applicable. The live inspector overlays measured values and
queue occupancy on the exact graph that produced them.

### 16.3 Real-time without full duplex

Several graphs can meet the same latency goal:

- a small non-reasoning text model triggered from partial ASR, with streamed
  TTS and external barge-in;
- a reasoning model speculatively started before an endpoint and adopted only
  if its source revision remains valid;
- a speech-to-speech model under an external interaction controller;
- a fast visual reflex racing a slower planner;
- a full-duplex native-interaction model;
- fast and deliberative models both connected to an utterance arbiter;
- cached or code-based responses for narrow high-deadline actions, with a
  model fallback.

The graph exposes the trade: information loss, latency, cost, incorrect early
commitment, authority, and implementation complexity. It does not declare one
arrangement intrinsically “realtime.”

## 17. Configuration and deployment planes

The design separates seven artifacts, two of which—the lock and channel
override—are generated or exceptional rather than everyday authoring:

| Artifact | Owns |
| --- | --- |
| element code and descriptor | ports, protocols, reactions, state, dependencies, effects, config schema |
| `.ortg` source or normalized graph form | symbolic instances, topology, `->`/`=>` delivery, exported boundaries |
| resolution lock | exact element/subgraph descriptor revisions and content digests |
| element values | model IDs, provider options, prompts, reasoning settings, voices, policy-specific parameters |
| optional channel overrides | exceptional queue depths keyed by stable edge identity |
| deployment configuration | placement, transport, resource pools, replicas, credentials references, admission limits |
| evidence profiles | measured latency, capacity, reliability, cost, and provenance |

A repository layout might be:

```text
graphs/
  assistant.ortg
  policies/
    voice.ortg
    computer.ortg
  openrealtime.lock
config/
  assistant.values.yaml
channels/
  assistant.yaml
deploy/
  workstation.yaml
profiles/
  measured-a100.json
```

Stable node IDs bind entries in the values artifact to element-owned schemas.
Loss is visible directly in `.ortg` as `=>`; lossless `->` is the normal case.
Depth uses a bounded port/protocol default unless the optional channel overlay
changes it. Exact element resolution lives in the generated lock. Transport
and placement settings use stable node and edge IDs in deployment config. A
repository may split values or overrides into per-element files without
changing this semantic separation.

The old flattened flag file is reference material, not a supported production
frontend. New launches select a profile and populate typed element configs;
remaining topology flags are deletion work, not an API that the graph runtime
must preserve.

## 18. Dynamic composition and lifecycle

### 18.1 Dependency context

Each graph, session, and nested subgraph has a scoped service context. Elements
declare required and optional dependencies—clock, provider client, media
store, tool registry, authorization service, GPU pool, target surface,
telemetry, or another named service. The runtime activates an element only
when required dependencies are satisfied and deactivates or replaces it when
they are withdrawn or changed according to policy.

These dependency edges are coeffects and form a separate inspectable overlay.
They are not ordinary observation channels: a model client being available is
not a user observation.

### 18.2 Reversible lifecycle effects

During mount, an element may register handlers, subscribe to events, allocate
queues, start supervised workers, reserve devices, or publish services. Every
such registration belongs to its lifecycle scope and has an inverse or
disposer. Unmount cancels work, waits within a bound, unwinds effects in the
declared order, and reports leaks.

This is the Cordis temporal-composability discipline applied to runtime
resources. It does not claim to reverse bytes already played to a speaker, a
click already delivered, or a message already sent. Those cross the existing
commitment and irreversibility boundary.

### 18.3 Reconciliation protocol

Future live graph replacement follows a transaction-like sequence:

1. elaborate and validate the candidate graph without acquiring resources;
2. diff stable node, port, edge, config, state, and dependency identities;
3. resolve and pre-mount new dependencies and resources where safe;
4. identify affected cancellation scopes and irreversible in-flight actions;
5. quiesce, drain, cancel, or detach affected channels according to explicit
   policy;
6. reach a graph safe point and atomically publish the new routing table;
7. migrate state only through a compatible schema and explicit migrator;
8. activate new elements and resume admitted work;
9. dispose removed effects in reverse ownership order;
10. record the transition, losses, retained state, and new graph fingerprint.

If safe migration is impossible, the runtime refuses the live update or
requires a session restart. It never guesses how to preserve opaque model
state.

## 19. Runtime execution model

### 19.1 Preserve the current invariants

The existing event-loop invariant remains fundamental:

> Commit is unconditional; acting is conditional; every deferral has a
> wake-up. Committed work is never silently dropped.

In the graph design, the invariant is implemented through typed trajectory
append, trigger, deferral, and wake channels plus validator obligations. A
deferring element must declare the event that can release its condition. A
terminal `Drop` is an explicit action with a reason, not silent disappearance.

Safe-point compare-and-append, source revisions, cancellation of stale work,
and the single external action boundary remain reusable kernel services and
elements.

### 19.2 Scheduling

The runtime scheduler manages bounded channels and reaction tasks. It must
support:

- independent concurrency across elements and runs;
- per-element and per-resource concurrency limits;
- fair or explicitly prioritized channel service;
- run and cancellation scopes;
- virtual clocks for deterministic tests;
- deadlines without one goroutine or timer per tiny event where avoidable;
- backpressure propagation;
- supervised workers and bounded shutdown;
- deterministic ordering where the contract promises it;
- trace context across local, sidecar, and remote boundaries.

The scheduler should not centralize every policy decision. Policy elements
produce control messages; the scheduler provides mechanics.

### 19.3 State and trajectory

The canonical trajectory remains an append-only causal record, but it becomes
an explicit service/element rather than an implicit shared assumption of every
possible graph. Some small graphs may use a different typed state element;
reference agent graphs use the trajectory for user observations, model
outputs, tool proposals, authoritative calls, results, visibility, repairs,
and audit.

Context engines are ordinary stateful transforms over trajectory and media
handles. Several context engines may serve different models in one graph—for
example, a low-latency compact audio context, a deliberative multimodal
context, and a computer-use context with only the current target and frame.

### 19.4 Placement

Placement is independent of functional topology. An edge may compile to an
in-process queue, shared memory, sidecar stream, or remote transport while
retaining its logical type, depth, ordering, and trace identity. A transport
adapter must report which delivery guarantees it can actually honor. Crossing
a process or machine boundary never silently weakens the channel contract.

### 19.5 Failure and supervision

Failure is part of an element's terminal contract, not an out-of-band log line.
Elements publish typed failure outcomes with run identity and retryability
evidence. Supervisors select restart, retry, fallback, circuit-open, detach,
session-fail, or explicit degradation behavior.

Retries are permitted only when the operation's idempotency and source
revision make them safe. A model read may often retry; a click, payment,
message send, or partly played utterance may not. Panics and sidecar exits are
contained to their lifecycle scope, all affected runs receive terminal
outcomes, and bounded cleanup precedes restart. Health affects dependency
activation and admission rather than silently routing to an unverified
implementation.

### 19.6 Admission and resource isolation

Profiles and deployment declarations name concurrency, memory, queue, GPU,
bandwidth, and external-rate limits. Admission occurs before a graph accepts a
session or creates work it cannot bound. Limits can be scoped by deployment,
tenant, session, element, model pool, or effect target. Overload is surfaced as
backpressure or a typed refusal that policy can handle; it is never converted
into unlimited buffering.

## 20. Authority, effects, and safety

The element graph must make authority at least as strong as the current
action boundary.

Recommended distinct types include:

```text
Proposal<ComputerAction>
Validated<ComputerAction>
Authorized<ComputerAction>
Committed<ComputerAction>
Result<ComputerAction>
```

A model output cannot connect directly to an effectful sink that accepts only
`Authorized<T>`. Validation, target fencing, confirmation, policy, commitment,
dispatch, and audit are visible elements or a reviewed composite. Tool schema
visibility is not execution authority.

The same rule applies beyond computer use: speech, external text, messages,
files written, API calls, device actions, and future effect types cross an
explicit commitment boundary. Prepared work remains cancellable. Once emitted,
the ledger records what crossed and any repair obligation.

Provenance remains typed. Text read from a screen or file cannot silently
become a user instruction. A trusted transformation may summarize or classify
it, but the origin and trust boundary stay present. Attachment handles carry
access scope and retention; an element receives only the file capabilities its
port and dependency contract grant.

## 21. Visualization and live inspection

Graph IR makes the architecture visually inspectable without maintaining a
second hand-authored diagram.

### 21.1 Static view

The renderer shows:

- node instance and element revision;
- named ports and compact protocol/payload types;
- channel direction, delivery, and depth;
- connector and arbitration semantics;
- trigger, interrupt, state, data, and effect paths with distinct styles;
- subgraph hierarchy and exported boundaries;
- required services and placement as optional overlays;
- unresolved or explicitly ignored obligations;
- graph fingerprint and configuration revisions.

Mermaid is suitable for documentation export. DOT/SVG can provide more exact
layout. The interactive canvas is the complete view and may use n8n-like node
configuration and execution inspection. None is the semantic source.

### 21.2 Live view

For a selected session or run, the UI overlays:

- active triggers and run IDs;
- last item and causal path;
- queue occupancy, high-water mark, drops, and backpressure;
- state revision sampled by each run;
- first-output, completion, and cancellation latency;
- deadlines and misses;
- model reasoning configuration without exposing private reasoning content;
- effect authority, confirmation, commitment, and execution status;
- failures, retries, fallbacks, and circuit state;
- element health, resource placement, and dependency activation;
- configuration and graph changes over time.

The runtime now derives a first live reaction-evidence layer from the frozen
reaction contract rather than guessing from payloads. Successful consumption
on declared trigger and interrupt ports and successful emission on declared
output and outcome ports populate the node overlay. Repeated trigger envelopes
with the same non-empty run ID count as one active run, while the first
nonzero first-output, completion, and cancellation times remain stable for the
life of the mount and through shutdown. Raw trigger and outcome item IDs stay
inside the in-process view: management snapshots redact them and payload-free
recordings omit them while retaining the counters and timing evidence.

The session-scoped inspection API also exposes the exact source-free static
model at `/openrealtime/v1/sessions/{session}/model` under the same narrow
capability as the live snapshot. The server revalidates graph fingerprint,
revision, node set, and element identities before returning it. The browser
operator panel then joins that model to the redacted live view and renders one
text-only node card with declared triggers, sampled state, interrupts,
outcomes, concurrency, causal breaks, effects, reversibility, and authority
beside observed active-run and timing evidence. It labels declared effect
authority separately from observed timing; it does not invent a live
authorization decision or a trigger-relative latency that the current schema
does not contain. A missing legacy static model leaves a visibly live-only
view, while identity drift or unredacted identifiers fail the join closed.

Trace replay animates the same graph and permits inspection of historical
runs. A developer should be able to select an audible word or computer click
and trace backward to the observation, policy decision, model run, state
revision, authority decision, and queue delays that caused it.

## 22. Validation and design completeness

The framework cannot prevent every poor policy, but it can make missing design
choices difficult to overlook.

### 22.1 Four validation layers

| Layer | Checks |
| --- | --- |
| authoring frontend | `.ortg` or normalized-form syntax and source locations; optional SDK compile-time checks |
| graph elaborator | port direction, payload and temporal protocol, IDs, cardinality, required ports, fan-out/merge, cycles, reaction reachability, channel bounds, authority paths, config schemas |
| startup resolver | dynamic plugins, negotiated formats and capabilities, dependencies, placement and transport guarantees |
| runtime verifier | observed latency/cost, queue bounds, cancellation, lifecycle leaks, SLO and safety invariants |

The elaborator is the normative static type checker. Dynamic plugins selected
by configuration still require runtime descriptors and startup resolution.
Native, sidecar, and remote elements use the same canonical type IDs and
validator; an optional SDK compiler only moves some diagnostics earlier.

### 22.2 Errors

The core compiler should reject:

- input/output or protocol type mismatch;
- two direct writers to one input;
- one output wired to several inputs without a `Tee`;
- incompatible or undefined merge/arbitration semantics;
- an unconnected required port;
- duplicate unstable IDs;
- a causal cycle without a valid break or initial state;
- a lossless promise that the selected source/transport cannot honor;
- a proposal connected around the required authority boundary;
- state migration without compatible schemas;
- missing required dependency or transport guarantee;
- an architecture whose exported contract claims unavailable capabilities.

### 22.3 Lint obligations

Profile-specific lint can require an explicit decision for:

- trigger source and trigger reachability;
- timeout and failure handling;
- interruption and acknowledgement paths;
- stale result handling;
- unconsumed model outcomes or tool results;
- user-speech overlap while output is active;
- queue overflow at non-backpressurable media sources;
- stream framing and safe preemption boundaries;
- unhandled partial, final, cancelled, refused, and failed outcomes;
- executable action confirmation, target, and audit;
- dependency loss and restart behavior;
- latency budget and queue contribution;
- retention and access of media and files.

Different validation profiles may suit conversational voice, silent computer
use, meeting observation, or offline file work. A developer may satisfy a
non-applicable obligation with an explicit `Ignore`, `Drop`, `Detach`, or
`Reject` element carrying a reason. The graph then documents the decision.

## 23. Reference compositions

Reference graphs are useful defaults, examples, benchmark identities, and
tested bundles. They are not privileged runtime species.

### 23.1 Componentized conversational agent

```text
microphone -> Tee -> ASR -> Context -> TextModel -> TTS -> speaker
                \-> Endpoint -> InteractionPolicy -> trigger/cancel
camera -> Latest -> VisionModel ---------------------> Context
```

This graph can observe while speaking and can be externally interrupted. It
can meet realtime goals without a full-duplex model.

### 23.2 Speech-to-speech foreground with external interaction

```text
microphone -> Tee -------------------------> SpeechModel -> audio arbiter
                \-> PolicyASR -> InteractionModel ------> act adapter

trajectory -> BackgroundReasoner -> context injection --^
```

The speech model retains native audio generation while an independently
measurable controller decides opportunities and overlap. If supported, another
reference graph selects the model's native interaction head over the same
foreground.

### 23.3 Fast and deliberative models both able to speak

```text
                    +-> fast model -------------------+
turn/context -> Tee |                                  |-> UtteranceMux -> TTS
                    +-> deliberative model -> Tee -----+
                                             \-> Context
```

The deliberative output can go directly to speech and context. Replacing the
`UtteranceMux` with a priority mux, race, or audio mixer changes visible
composition semantics. Routing deliberative output only to context recovers
the current “fast speaks last” reference pattern without making it a kernel
rule.

### 23.4 Independent voice and computer-use policies

```text
microphone -> ASR -> voice interaction -> voice cognition -> speech action

screen -> Latest -> visual reflex ----+
       \-> visual planner ------------+-> ActionArbiter -> Authority -> computer
                          tool result ---------------------> planner/context
```

Voice may use transcript-based interaction while computer use races a bounded
visual reflex against a slower planner. Both can share trajectory and tool
results without sharing trigger, latency, or arbitration policy.

### 23.5 Text, files, and multimodal attachments

```text
text event -----------------------+
file handle -> parser/retriever --+-> ContextJoin -> reasoning model -> outputs
image attachment -> vision -------+
timer/tool result ----------------+
```

No audio ports or duplex state are required. The same kernel supports the
agent because realtime concerns still include trigger timing, background jobs,
deadlines, tool results, interruption, and user-visible commitment.

## 24. Refactoring the current repository

The refactoring should be incremental and should reuse behavior proven by the
existing test suite.

| Current area | Target responsibility |
| --- | --- |
| `gateway` and transports | Protocol source/sink elements around an unchanged compatibility boundary. |
| `api/v1` | Keep the stable component API; add a versioned element/graph API rather than breaking v1 in place. |
| `architecture` | Catalog immutable Graph IR identities, exported contracts, requirements, lineage, and evidence profiles instead of deriving one of three binding topologies. |
| `binding` | Transitional compatibility layer; ultimately replaced by element factories and reference graph builders. |
| `binding/cascade` | Decompose the large runtime into perception, state, policy, cognition, action, and connector elements. |
| `binding/sidecarbinding` | Split into a sidecar model element, native media/control ports, trajectory adapters, and reference graphs. |
| `binding/omni`, `binding/duplex`, `binding/upstream` | Become reference graph constructors and provider/transport adapters, not distinct runtime species. |
| `eventloop` | Preserve safe-point and eventual-wake invariants while evolving into graph scheduling, trigger, supervision, and trace services. |
| `trajectory` | Preserve the causal append-only store and expose it as a typed state/log service and elements. |
| `continuation` | Split provider-neutral invocation/result contracts from graph model-element ports; retain adapters and safe-point runner logic. |
| `interaction` | Wrap existing policies as typed elements/subgraphs; replace one master policy assembly with explicit graph connections and arbiters. |
| `perception` | Turn gates, observers, ASR, video selection, narration, and identity into independent typed elements. |
| `cognition` | Turn context construction, fast/deliberative runs, visual reflex, and projections into elements; remove fixed role assumptions. |
| `action` | Preserve the ledger, confirmation, authorization, speech pacing, dispatch, and repair as typed authority/commit/action elements. |
| `computeruse` | Preserve target fencing and dispatcher; expose proposals, authorization, serialized target execution, and visual consequence as explicit ports. |
| `sidecar` | Generalize handshake and transport descriptors for arbitrary typed model elements while retaining protocol-version conformance. |
| `providers` and `adapters` | Register implementation factories and descriptors; do not own topology. |
| `session` | Supply scoped duplex/media/session services where selected by a graph; text-only graphs need not instantiate audio state. |
| `cmd/openrealtime` | Select graph, typed configs, deployment, and overrides through the new profile APIs; remove obsolete production flags rather than making them a second architecture. |
| `presentation/host`, `presentation/browser`, and `macos` | Remain descriptor-locked profiles over the shared plugin/client contracts in [Composable Presentation and Observability](composable-presentation.md); the retired standalone presentation forks are not compatibility paths, and the gateway owns no UI. |
| `bench` and measurement | Record Graph IR fingerprint, element/config revisions, live resolution, traces, and exact selected paths. |

### 24.1 Invariants to demote from the kernel

The following current statements become properties of named reference graphs:

- slow cognition is always engine-owned;
- slow cannot speak;
- fast is always the last writer before audio;
- every architecture has audio input, audio output, and turn generation;
- perception, fast cognition, and action must share one of three owner
  topologies;
- a sidecar-backed graph always requires a slow continuation provider;
- interaction selection is completely described by a fixed controller vector
  and one catalog arbitration enum.

The safety principle “model output is not effect authority” remains a kernel
invariant. So do bounded channels, explicit merge/fan-out, causal commits,
irreversibility accounting, and protocol validation.

### 24.2 Target package shape

Names are provisional, but responsibilities should separate roughly as:

```text
graph/
  ir/          versioned Graph IR and canonical encoding
  syntax/      `.ortg` parser, formatter, source maps, and editor services
  manifest/    normalized strict YAML/JSON interchange
  resolve/     symbolic-name resolution and lockfiles
  sdk/         optional Go and generated external builders
  validate/    core and profile-specific validation
  runtime/     mounting, scheduler, channels, scopes, supervision
  reconcile/   graph diff and lifecycle transition
  inspect/     snapshots, traces, render models

flow/          Tee, Mux, Join, Switch, State, Latest, Race, Delay
element/       descriptors, factories, ports, reactions, lifecycle
elements/
  gateway/
  media/
  perception/
  cognition/
  interaction/
  action/
  authority/
  sidecar/

graphs/        shipped reference `.ortg` files and optional generators
catalog/       graph, element, config, deployment, and profile catalogs
```

New production and benchmark paths are implemented directly in this structure.
The old implementation is reference material only: it may be deleted, and it
does not justify a migration layer, compatibility runner, historical-attempt
reconstruction, or alternate benchmark path.

## 25. Implementation plan

### Living implementation tracker

Last reconciled with the repository on **2026-09-02**. This is the progress
source of truth for the refactor. It must be updated in the same commit that
closes or materially advances an implementation item. The checked implementation
ledger and `git log origin/main..main` together identify the accepted local
checkpoints without relying on a self-referential “latest commit” marker.

A checked box means the implementation and the evidence required by that item
are both present. Partially implemented work stays unchecked and names what is
already available. A phase is not complete until its exit gate is checked.
Broad objectives must be split into independently reviewable boxes as work
lands so that a remaining end-to-end gate does not hide completed primitives,
reference graphs, or conformance evidence.

At this reconciliation, 221 of 269 explicit tracker boxes are checked (82.2%)
and 48 remain open. The percentage records completed reviewable slices; it does
not dilute an open parent, benchmark population, platform gate, or definition-
of-done requirement.

| Phase | Current state | What exists now | Principal remaining work |
| --- | --- | --- | --- |
| 0 — contracts | Complete | Accepted design, terminology, authoring decisions, and historical quality targets | Keep decisions and superseded ADRs synchronized as implementation lands |
| 1 — graph foundation | Exit evidence pending | Typed descriptors/runtime, `.ortg`, strict YAML/JSON interchange, Go SDK, lockfiles, Graph IR, validation, connectors, rendering, and a coarse reference mount kept outside the production profile | Full new-architecture integration and benchmark-quality evidence |
| 2 — component/cascade | In progress | Acoustic admission/endpointing, ASR, commit-bound trajectory-prefix activation, cognition, interaction/result commit, speech, tools, explicit `Tee`/`Mux`, full locked fast-only/slow-only/both-speaking reference graphs, executed-turn regression, and retained safe-point comparison | Shared-server launch integration and measured behavioral quality; the retained diagnostic records current divergence rather than claiming completion |
| 3 — sidecar/end-to-end | Complete | Typed v1-v4 sidecar negotiation, one graph-native external-model element, locked omni/duplex/upstream topologies, exact protocol-v4 mount/dial/media conformance, and executed native/external interaction parity | Provider quality and performance remain in the shared benchmark matrix rather than reopening this architecture gate |
| 4 — modalities/authority | Complete | Typed visual observation, multimodal text/image/file/attachment ingress and retention, explicit streaming camera/screen/video cadence, complete audio-free text/file cognition, complete silent Realtime-CU, independent voiced/silent action composition, explicit dual-lane visual-reflex/slow-planner arbitration and feedback, plus proposal, confirmation, target-fence, ledger, and dispatch elements | Keep locked reference graphs and adversarial authority evidence synchronized with contract revisions |
| 5 — config/catalog | In progress | Resolution locks, strict node-ID-keyed values, separate deployment, secret-reference, and evidence-profile artifacts, exact plan-bound graph-native launch, immutable production graph/config/profile catalog entries, mount-time secret assembly, exact redacted deployment evidence through live inspection/trace/replay/benchmarks, graph-contract-owned session-adapter capability validation, authenticated benchmark/gateway inspection, and reviewed graph-path attestation | Legacy serve-flag removal and executed candidate artifacts |
| 6 — inspection/authoring | In progress | Static rendering, reaction-contract-driven live trigger/run/cancel/outcome timing evidence, first-trigger-relative node latency, closed payload-free live authority outcomes, exact session-scoped static/live browser joins, explicit channel depth/delivery/occupancy/loss/queue-wait views, pseudonymous cross-node flow-stage routes with per-stage timing, direct-parent lineage, and closed observation/state-revision/policy/model-run classification, live graph/node/queue/flow evidence, deterministic semantic graph diff, session-keyed bounded runtime recording, payload-free trace artifacts, exact replay, bounded `.ortg` recovery, strict formatter edits with browser application, compiled-fingerprint-bound canonical `.ortg` and normalized YAML/JSON canvas node rename plus edge removal and creation, resolved values-property metadata, complete browser, LSP plaintext, and native SwiftUI values-contract projections, standards-shaped LSP diagnostics, completions, definition links, and versioned rename/format edits, bounded full-text document synchronization, a transport-neutral strict JSON-RPC/LSP adapter, digest-bound in-memory multi-file/subgraph indexing and navigation, separately authorized rooted source-read/publication boundaries with browser load/create/update controls, source-mapped text-only browser diagnostics, a compiler-backed language-service core exposed through the UI-independent management API, and authenticated rollback-safe stateless, multi-row, and stateful browser implementation replacement | Complete signed-native, topology-changing, and remaining capability-specific presentation replacement gates |
| 7 — reconciliation | In progress | Compiled scoped dependencies, lifecycle-owned services/workers/disposers, bounded dependency-closure quiescence, and immutable-plan implementation/config/permission/state reconciliation with effect-restricted candidate pre-mount, exact state migration, exact retired-ownership audits, refusal, and rollback | Graph-routing safe points, state-schema-changing and bounded topology-plan changes, and cross-system leak-proof ownership |
| 8 — obsolete-path deletion | In progress | Historical-attempt reconstruction and benchmark migration/parity code are absent; generic ownership, continuation, Graph IR, and catalog boundaries no longer impose engine-owned/silent slow cognition or mandatory audio; ADR-0015 separates retained invariants from superseded binding-topology mandates; old implementation remains reference-only | Finish direct production/evaluation profiles, then delete unreachable reference code, obsolete switches, and topology-derived catalog/status facts |

Current checkpoint notes:

- Accepted implementation checkpoints are the commits already in the current
  branch history and the ledger below. Worktree-only slices remain unchecked
  until their review and required evidence are complete; no hard-coded branch
  or remote position is a completion oracle.
- The committed graph-native slice now extends from acoustic and multimodal
  ingress through state, independently triggered cognition, explicit
  interaction/speech routing, tool authority, and external-model topologies.
- Observation commits now carry a store-attested compact trajectory-prefix
  identity and exact State item ID. Activation policies consume that
  self-contained basis, and cognition verifies and reconstructs the immutable
  prefix from a later append-only State snapshot if delivery order overtakes
  the trigger. This removes the former cross-port context/commit join race
  without copying a growing trajectory into each activation.
- State inputs that can release a pending irreversible action transition are
  lossless contracts. In particular, canonical call promotion and tool-result
  commit cannot use replaceable/lossy State delivery: dropping the last
  required snapshot would otherwise strand completed external work forever.
- Deterministic graph diff, session-keyed bounded runtime recording, replayable
  payload-free trace artifacts, and the canonical `.ortg` language-service
  core, adaptive video ingress/cadence, and the full conversational reference
  family are committed. Bounded recovery, formatter edits, and resolved schema
  metadata now have a UI-independent Authoring API checkpoint; strictly
  revalidated browser, bounded UTF-16 LSP plaintext, and native SwiftUI views
  render the complete values contract. The same LSP boundary now projects full
  diagnostics, exact-symbol completions, real-range definition links, and
  versioned rename/formatting workspace edits into deterministic protocol wire
  types. A strict one-message JSON-RPC adapter now freezes the descriptor
  catalog, owns bounded monotonic full-text document snapshots, projects those
  operations through the standard LSP lifecycle, and performs no transport,
  workspace discovery, filesystem, process, or runtime I/O. A separate
  immutable index now fingerprints the caller-supplied document population and
  retains each path, URI, version, and source digest while projecting imports,
  nested graph declarations, and subgraph boundary ports to exact cross-file
  ranges. Standard LSP definition requests rebuild and lease that closed index;
  unrelated descriptor and same-document definitions retain their existing
  behavior. The rendered browser canvas now selects nodes from one compiled
  fingerprint and invokes a separately contracted, capability-scoped rename;
  the compiler produces the graph-wide edit set while the API, relay, browser,
  and workspace independently rebind source identity, spans, and compare-and-
  swap state before advancing the source revision. The same canvas now removes
  one selected named or endpoint-derived canonical edge through an independently
  validated editing capability, advances the revision once, and recompiles the
  remaining graph under a changed fingerprint. Directional port controls can
  now create one named edge against that exact predecessor fingerprint; the
  compiler validates the candidate at the next revision before any edit set or
  candidate fingerprint is returned, and every API, relay, client, and browser
  boundary revalidates the exact canonical source mutation. The same immutable
  editing boundary now admits exact canonical normalized YAML/YML and JSON. It
  regenerates one source-digest-bound full-document replacement for node rename,
  edge removal, or edge creation, while the compiler remains the single Graph IR
  and candidate-fingerprint authority. The API, headless client, server, host
  relay, browser client, and workspace independently reparse or reconstruct the
  requested mutation; browser JSON rejects duplicate keys and reproduces Go's
  canonical field/escaping order, while browser YAML admits only the canonical
  manifest layout. Go, adversarial JavaScript, repository-wide test/vet,
  targeted race, every real-Chromium profile, and the shared browser/native
  companion gate are green. The remaining phase gates concern complete
  server/native presentation lifecycle evidence rather than visual mutation.
- Seven of the fifteen project-level definition-of-done outcomes are proven end
  to end. The remaining eight have named completed subgates, but direct
  benchmark execution, production inspection, reconciliation, native-client
  evidence, and obsolete-path deletion are still outstanding.

Integrated checkpoint ledger:

- [x] Typed `.ortg`/YAML/JSON/Go authoring, immutable Graph IR, lockfiles,
  validation, bounded runtime, rendering, and a coarse reference mount that is
  not an accepted production profile.
- [x] Graph-native ASR, observation commit, compare-and-append trajectory,
  provider-neutral cognition, TTS, playback, and exact live resolution.
- [x] Explicit acoustic admission/endpoint policies with visible tick, commit,
  verdict, cancellation, flush, state, and terminal paths.
- [x] Independent fast/deliberative activation policies consume the exact
  store-attested committed-prefix identity, with bounded cancellation memory;
  cognition verifies the digest, session, State identity, causal parent, and a
  scalar replay floor before reconstructing that prefix from append-only State.
- [x] Explicit fast-only, slow-only, and both-speaking interaction components;
  no kernel-owned slow-to-fast handoff or model-to-model edge.
- [x] Typed tool proposal admission, confirmation, target fencing, idempotent
  ledger commit, dispatch, and release/audit outcomes.
- [x] Typed audio-free text/image/file/attachment ingress, bounded retention,
  revocable leases, resolution, cancellation, and release paths, plus locked
  text/file cognition and silent computer-use agents with executed regressions.
- [x] Graph-native external-model contract and locked omni, duplex, and
  upstream topology variants over shared element contracts.
- [x] Authenticated benchmark and gateway evidence bound to exact Graph IR,
  configuration, selected paths, runtime, and capability identities.
- [x] Deterministic semantic graph diff plus bounded, fingerprinted,
  payload-free trace artifact and exact replay validation.
- [x] Opt-in per-mount trace recording with exact graph/config/runtime evidence,
  session-keyed correlation pseudonyms, bounded compaction, queue residence
  timing, deterministic export, and key erasure.
- [x] Compiler-backed canonical `.ortg` diagnostics use immutable source
  snapshots, source-mapped errors, stable revision cursors, and bounded results.
- [x] Descriptor-derived element metadata drives element, compatible-node, and
  directional-port completion plus hover and go-to-definition.
- [x] Separate strict deployment bindings and graph-scoped secret-reference
  catalogs bind into Graph IR and the CLI without mixing topology, values,
  placement, resource selection, or credential material.
- [x] Bounded graph-aware rename and strict in-memory edit application reject
  stale cursors, overlapping edits, and ambiguous node references; the same
  compiler-backed editing boundary exposes separately authorized,
  fingerprint-bound canvas node rename and exact canonical edge removal.
- [x] Bounded `.ortg` recovery retains only complete synchronized statements,
  supports explicit descriptor-backed partial completion, and remains outside
  strict compilation, locking, reconciliation, formatting, and runtime paths.
- [x] Strictly parsed snapshots expose atomic source-digest-bound canonical
  formatter edits through the UI-independent management Authoring API.
- [x] Bounded self-contained values-schema resolution exposes deterministic,
  recursively independent property metadata and diagnostics through the same
  API without flattening composition or inventing unresolved fields.
- [x] Standard-catalog camera, screen, and video ingress exposes typed raw-frame
  and capability-free reference outputs with generation/session fencing.
- [x] Fixed, adaptive, and manual visual-observation cadence has explicit
  tick/refresh/end/cancel contracts and bounded latest-frame/change state.
- [x] The locked adaptive-video reference makes capture-pressure loss visible
  and composes observer refresh, close, cancellation, and terminal paths.
- [x] The locked conversational fast-only reference routes only the fast
  prepared stream directly to segmentation and speech.
- [x] The locked conversational slow-only reference routes only the
  deliberative prepared stream directly to segmentation and speech.
- [x] The locked conversational both-speaking reference routes both prepared
  streams through an explicit interaction speech arbiter.
- [x] All three conversational references share an asserted identical
  non-routing backbone and compile, bind values, resolve providers, and mount.
- [x] Execute complete conversational turns through all three locked reference
  graphs and retain the regression artifacts.
- [x] Retain the already-captured historical trace only as a diagnostic target.
  The payload-free ordinary-turn record is fingerprint-bound and exposes the
  old/new divergence; it is not a runtime dependency or a release arm.
- [ ] Complete the remaining visual/UI rendering, reconciliation, direct
  benchmark execution, and obsolete launch-path deletion.

Active acceptance queue (work in the shared worktree remains unchecked until
it has been reviewed, tested, and committed with its evidence):

- [ ] Complete the retained multimodal-review and recording checkpoint.
  - [x] Seal the exact eleven-scenario source population and the exact
    165-attempt review population behind portable source/evaluation receipts,
    add credential-free bundle verification, and require the exact
    `gemini-3.7-flash` advisory-review population in the release plan. The
    deterministic scorer remains authoritative; the retained live Gemini run
    currently proves transport and media decoding only, not a live 165-attempt
    baseline/candidate comparison.
  - [x] Add descriptor-attested FFmpeg encoder and full-decode attestor plugins
    with create-only manifests, exact raw-frame retention, synchronized stereo
    WAV plus MP4 output, odd-dimension padding provenance, sandbox boundaries,
    and normal, shuffled, race, vet, and benchmark coverage.
  - [x] Reject external hard links at every shared review-media, evaluation,
    and external-receipt read/write boundary. The fail-closed Linux, Darwin, and
    Windows implementations have adversarial source/bundle/receipt tests plus
    normal, shuffled, race, vet, cross-build, and performance coverage.
  - [x] Make provider-evaluation publication crash-consistent across the
    create-only evaluation directory and its caller-owned external receipt.
    Stage and verify the complete provider response first, durably publish its
    portable receipt before exposing the final sibling name, and prove restart
    recovery, pre-receipt quarantine, root/symlink swap refusal, and no
    duplicate provider call after the durable boundary.
  - [x] Add the provider/storage/UI-neutral candidate evidence plug-in and
    lifecycle, including explicit production-versus-hermetic origin, exact
    shared-session audio/video callbacks, exact external-harness media and
    structured artifacts, immutable attempt/result snapshots, create-once
    identities, and fail-closed population sealing. FDB, FDB v3, FD-Bench, and
    tau-Voice now call this candidate-only boundary; no historical run or
    migration path is represented by the API.
  - [x] Add the provider-neutral candidate review campaign and aggregate
    publication boundary. It streams only a sealed new-run source receipt,
    publishes one crash-recoverable create-only evaluation and external
    receipt per attempt, and then publishes a case-by-case media-linked
    `REVIEW.md`, campaign manifest, and external aggregate receipt only after
    reopening the complete transitive population. Normal, ten shuffled runs,
    race, vet, interruption recovery, quarantine, tamper/link adversaries, and
    exact-sixteen publication/reopen benchmarks are green; no historical
    loader, baseline registry, migration path, server dependency, or UI is in
    this API.
  - [x] Finish the four-case Meeting Assistant bundle with synchronized WAV and
    MP4 per case, deterministic pass/fail plus advisory model review, durable
    source-before-model and per-evaluation receipts, case-by-case `REVIEW.md`,
    credential-safe exact `gemini-3.7-flash` integration, real FFmpeg full
    decoding, retry/recovery, and independent root audit. Clean candidate-21
    sealed 4/4 reportable graph-native attempts, four exact-model evaluations,
    zero quarantine, and credential-free reopening. Its result is
    `meeting-candidate-exact4-20260831-21-visual-horizons-full-reviewed.json`
    (`sha256:e7e56ea603b884b082f87315ed42e200b0fe3a4493375d2e9706b2538e486355`),
    sealed by review manifest
    `sha256:e899491212b6426114661d0898d9797823207b660f59a85671f50cb93257d020`.
  - [x] Finish the exact sixteen-case Realtime-CU bundle with real Chromium
    capture rather than generated-frame substitution, synchronized WAV and MP4
    per case, pixel and set-of-mark coverage, deterministic pass/fail plus
    advisory model review, durable receipts, case-by-case review, real FFmpeg
    full decoding, retry/recovery, and independent root audit. Clean
    candidate-05 sealed 16/16 reportable graph-native attempts, all pixel and
    set-of-mark variants, sixteen exact-model evaluations, zero quarantine,
    and credential-free reopening. Its result is
    `realtime-cu-candidate-exact16-20260831-05-full-reviewed.json`
    (`sha256:1f61f502203c6e259cccc0eda67b7435fd8201660d54d3a436fe6f3020cf3d30`),
    sealed by review manifest
    `sha256:dfa568a061dabc9323f74d195bccc891056d423e76f9360df6f0cc690ad93bcb`.
  - [x] Wire scenario, Meeting Assistant, Realtime-CU, and every applicable
    FDB/FDB3/FD-Bench/tau2 benchmark recording and advisory-review bundle into
    the release matrix with retained source/evaluation receipts, audio/video
    decode assertions, exact population counts, redacted summaries, and
    credential-free offline verification. The Meeting and Realtime-CU gates
    now require the exact Gemini plug-in, sandboxed FFmpeg/FFprobe media path,
    fresh create-only review roots, first/last synchronized source and review
    media, external source receipts, per-evaluation receipts, and sealed-bundle
    completion output. The scenario source and population-configurable
    evaluation gate retain the corresponding first/last population evidence
    and external receipts. FDB, FDB v3, FD-Bench, and both tau-Voice conditions
    use the suite-neutral fresh-prefix source/campaign/aggregate composition;
    their interrupted-run recovery and completed-aggregate verifier accept no
    historical bundle, migration input, or baseline registry. The live
    populations remain separately tracked below and are not claimed by this
    wiring checkpoint.
  - [ ] Run and report one provisioned live attempt for each of the eleven
    interaction scenarios against the accepted historical trail. The newest
    sealed 11×1 diagnostic passed 8/11 with all eleven attempts and exact-model
    evaluations credential-free reopened, but predates the latest repairs.
    Per the 2026-09-02 operator direction, the next 11×1 pass runs once; code
    changes do not trigger another complete scenario rerun unless explicitly
    requested.
  - [x] Run and independently reopen the live four-case Meeting Assistant
    review. Candidate-21 retained 4/4 reportable graph-native attempts and four
    exact-model evaluations behind the source/evaluation/aggregate receipts
    identified above.
  - [x] Run and independently reopen the live exact-sixteen Realtime-CU
    review. Candidate-05 retained 16/16 reportable graph-native attempts and
    sixteen exact-model evaluations behind the source/evaluation/aggregate
    receipts identified above.
  - [ ] Run the signed Darwin macOS client gate. Local synthetic,
    transport-only, Linux Swift, or manifest-derived probes do not close this
    native requirement.
- [x] Complete the sidecar/model protocol-v4 hardening checkpoint.
  - [x] Publish the deployment registry and codec with a genuinely atomic,
    create-only service batch operation. `Bootstrap.Install` publishes both
    services through `ServiceSet.InstallIfAbsent`, which canonicalizes the
    whole batch and refuses every colliding name under one lock *before* it
    writes any member. Making the batch install as it checks is caught by both
    `TestServiceSetInstallIfAbsentCollisionIsAllOrNothing` and
    `TestBootstrapInstallCollisionDoesNotPartiallyPublish`.
  - [x] Validate every frame's media metadata and byte lanes against the exact
    negotiated per-port format profile. `validatePortFrame` enforces the
    negotiated payload mode, the per-port JSON and binary maxima, and
    `MediaFrameMetadata.validateAgainst`, in both directions. It is wired into
    the live send and receive paths of `sidecar/client.go`,
    `elements/model/runtime.go`, and `meeting/graphnative/foreground_session.go`
    rather than into tests alone. Dropping either the byte maxima or the media
    profile comparison fails
    `TestV4NegotiatesEveryMediaPortAndEnforcesPayloadPairings`,
    `TestV4OpaqueBinaryPortsDoNotAcquireMediaOrAudioSemantics`, and
    `TestV4MediaFramesMustConformToTheExactNegotiatedProfile`.
  - [x] Add a standard protocol-v4 conformance CLI fixture rather than routing
    version 4 through the version-1 audio-only `Hello` path. `RunConformance`
    dispatches version 4 to `runElementConformance`, which offers the
    descriptor-backed `StandardElementConformanceHello`. Running
    `openrealtime conformance sidecar -protocol-version 4 -- python3
    sidecars/v4_conformance_sidecar.py` passes all fifteen checks, including
    descriptor attestation, applied-config digest, per-port negotiation, the
    typed request and result, causal correlation, and typed cancellation.
  - [x] Prove locked mount/dial negotiation for the omni, duplex-native, and
    upstream-native external-model references.
    `TestLockedExternalModelComponentsMountAndNegotiateExactV4Sessions` mounts
    each locked graph, captures the dial it actually makes, and proves the
    offered `Hello`, every negotiated port format, the live capability
    resolution, and the data plane. Offering version 1, or dropping the
    required capabilities from the offer, fails it.
  - [x] Pass focused and repository-wide race, test, vet, and diff gates; then
    review and commit the slice with its tracker boxes.
- [x] Complete the deployment and secret-artifact foundation checkpoint.
  - [x] Expand sparse per-node deployment bindings to deterministic effective
    implementation, placement, transport, resource, and secret-slot bindings.
  - [x] Bind a public redacted per-node deployment identity into immutable
    Graph IR while retaining a separate private deployment fingerprint, so a
    credential-reference rotation cannot become guessable from public IR.
  - [x] Parse strict bounded YAML/JSON secret-reference catalogs with exact
    graph-scoped coverage, immutable provider selection, fresh bounded value
    resolution, defensive copies, and erasable handles.
  - [x] Make `graph check`, `compile`, and `render` accept separate values,
    deployment, and secret artifacts and reject missing or excess secret
    references before resolving credentials.
  - [x] Expose deployment reference/digest changes in the static inspection
    model and deterministic semantic graph diff.
- [x] Carry the exact private deployment and secret-catalog identities through
  mount-time assembly, live inspection, trace recording/replay, and benchmark
  evidence without exposing locators or credential bytes. `Plan` freezes both
  opaque SHA-256 identities; assembly and `PreparedPlan` rederive and compare
  them before provider acquisition, then mount resolves only the exact sealed
  node/slot bindings. The mounted live view, trace-v2 envelope, and deterministic
  replayer retain the same canonical `DeploymentEvidence`; benchmark inspection
  authors and matches that exact value in graph-native execution evidence and
  refuses deployment/catalog drift. The end-to-end runtime test marshals the
  live, recorded, and replayed forms and rejects any secret reference, provider
  locator, or credential byte, while the benchmark and CLI tests independently
  prove redacted propagation into reviewed candidate evidence.
- [x] The scenario CLI reconciles a reviewed execution requirement against the
  exact bound Graph IR before reading credentials or starting protocol work.
- [x] All eleven scenario paths bind authenticated live graph evidence to the
  exact `scenario-name#run` attempt scope without retaining inspection tokens.
- [x] Delete omitted and reference-only scenario execution modes. The
  `scenario` command now accepts only a runnable architecture cell carrying an
  exact graph-native execution requirement, requires the complete eleven-case
  suite and retained review bundle, and reconciles the launch profile plus
  `-inspection-graph` before credential access. The former implicit diagnostic
  branch, partial `-only` flag, and unattested raw-record output are removed.
- [ ] Execute and compare the eleven interaction scenarios; scenario names
  remain individually tracked below. A local diagnostic baseline now exists and
  is *not* the reviewed comparison this box requires: fifty-five attempts, five
  per scenario, against a frozen graph-native launch profile
  (`openrealtime.launch.scenario-local@1`, sensevoice + vllm `qwen-fast` +
  fish-audio, all loopback) in the unattested diagnostic mode, with no
  `-review-dir` and no execution requirement. Thirty of fifty-five passed. The
  split is deterministic rather than noisy — six scenarios passed five of five,
  five failed zero of five, and none was flaky — which is what makes the result
  worth keeping: the failures are capability gaps, not variance.
  Passing: `asked not to be interrupted`, `cutting in on something wrong`,
  `ordering from a waiter`, `waiting out a silence they asked for`,
  `an acknowledgement is not an interruption`, `an ordinary question`.
  Failing: `count-as-they-go` (counts the first animal, then stops),
  `a recorded menu` (never calls `press_key`), `translating as they speak`,
  `somebody else's conversation` (speaks when not addressed), and
  `telling them what it saw`. Conversational turn-taking and silence discipline
  hold; standing-policy execution, tool invocation, simultaneous speech, and
  visual triggering do not.
  A single attempt per scenario is not enough to classify one: the first
  eleven-scenario run scored `waiting out a silence they asked for` as a
  failure, and it then passed ten of ten across two configurations. That is the
  reason this box asks for fifteen attempts, and the reason the run above is
  recorded as a diagnostic rather than as evidence.
- [x] Complete descriptor-driven values-schema authoring support.
  - [x] Snapshot immutable exact descriptors and reject unknown identities or
    Graph IR/descriptor contract drift before deriving a schema.
  - [x] Generate a deterministic, bounded Draft 2020-12 values envelope with
    exact graph/node identity, strict configless nodes, and no topology values.
  - [x] Resolve caller-supplied self-contained config schemas into deterministic
    `$defs` while rejecting invalid drafts, external references, nested resource
    IDs, non-object contracts, and schema-ID collisions.
  - [x] Keep opaque config references honest when unresolved: expose only the
    object/node-index contract, mark the bundle incomplete, and support a
    release gate that requires every reference to resolve.
  - [x] Pass focused adversarial, mutation-isolation, determinism,
    concurrent-reader race, cancellation, and bounds tests plus graph-wide race
    and vet gates.
  - [x] Integrate resolved property metadata, schema identities, completeness,
    and diagnostics into immutable recovery/analysis snapshots and the
    UI-independent management Authoring API.
  - [x] Render every Authoring API configuration field in the browser
    configuration-value editor. The client now revalidates the exact contract
    status, JSON Pointer, schema identity/digest, completeness,
    `additionalProperties`, title/description/format, default, enum, and
    canonical property schema before the text-only view admits it; malformed
    metadata and markup-bearing values have focused client/view and real-browser
    coverage.
  - [x] Render the same metadata through a standards-aligned LSP hover. It maps
    zero-based UTF-16 positions to the exact source snapshot, emits only
    deterministic plaintext, includes every contract/property field and
    absent/null distinction, and rejects split surrogates, malformed retained
    JSON, and oversized output.
  - [x] Render the same metadata in native-client configuration editors. Both
    native distributions now install a descriptor-locked authoring provider
    restricted to the management endpoint and a SwiftUI plaintext editor
    projection. The
    UI-independent Swift client binds each in-memory source digest to an exact
    header-only operator capability and response identity, validates every
    configuration and property field including absent/null distinctions, and
    rejects redirects, duplicate keys, unknown fields, noncanonical ordering,
    oversized responses, stale capability leases, and metadata drift. Swift
    5.10 normal and repeated adversarial suites, macOS syntax parsing, Go profile
    and cross-client integration, race, vet, and repository compile gates pass.
- [x] Complete the sound computer-use authority and trajectory feedback chain.
  - [x] Join tool proposals to exact cognition-result provenance by run and
    call identity without trusting externally asserted causal metadata.
    `validateCandidateResultBinding` is the only place that sees the candidate
    and the result together. Its three reachable refusals — a different
    canonical context prefix, a different source revision, and a result not
    causally descended from the candidate's own activation evidence — are
    covered by `TestProvenanceJoinRejectsCandidateResultProvenanceDrift`. Its
    run and session refusals cannot be reached through the ports, because both
    artifacts are bound to their own envelopes upstream and meet only under one
    run key; they are pinned directly by
    `TestCandidateResultBindingRefusesRunAndSessionDriftItCannotReachThroughPorts`
    so that being unreachable does not become a licence to delete them. The
    validator was 58.3% covered and every one of its five refusals could be
    replaced with `if false` without failing a test; it is now 100% covered and
    all five mutations fail.
  - [x] Promote an admitted proposal to an authorized trajectory tool call
    without colliding with the proposal item. The authorized call takes its own
    `canonicalTrajectoryItemID`, scoped by session, model run, proposal item,
    and call ID, and refuses a `trajectory_id_collision` if that identity is
    already occupied. Making the promoted item reuse the proposal ID fails the
    canonical-append tests.
  - [x] Commit dispatch results as typed tool-result trajectory items causally
    linked to the authorized call. Dropping `CausalParentIDs` from the promoted
    item fails with `tool call does not causally promote proposal item`.
  - [x] Compose and execute the locked silent computer-use reference only after
    those trust and feedback paths exist. The locked reference pins all twenty
    nodes and passes `graphvalidate.ComputerUse` warnings-as-errors, and the
    authority chain is enforced by the type system rather than by convention:
    confirmation yields `ConfirmedAction`, the target fence `AuthorizedAction`,
    canonical commit `CanonicalAction`, and only the ledger yields the
    `ExecutableAction` that dispatch accepts. Bypassing the target fence or the
    ledger boundary is `E_TYPE_MISMATCH` at compile time, not a missed runtime
    check.

Reference-agent tracker:

- [x] Componentized conversational voice-agent family with separate acoustic
  policy, ASR, observation commit, trajectory, independent fast/deliberative
  activation and cognition, result commit, interaction routing, segmentation,
  TTS, playback, cancellation/timing boundaries, and state. The fingerprinted
  complete-turn artifact is retained; its historical safe-point trace remains
  diagnostic only because it exposes rollout divergence.
- [x] Focused fast-only, slow-only, and both-speaking interaction graphs prove
  that either cognition stream can speak through explicit segmentation and
  arbitration topology.
- [x] Deliberative prepared output can route directly toward speech
  segmentation while result and tool paths remain independent; reusable
  `Tee`/`Mux` connectors require no kernel-owned slow-to-fast handoff. A
  complete foreground-context feedback variant remains part of the full agent.
- [x] Locked full-duplex/end-to-end external-model topology with native
  interaction ports.
- [x] Locked external-interaction topology over the same `model.External`
  contract; executed native/external parity remains open.
- [x] Live meeting assistant combining speech, screen control, concurrent slow
  document work, corrections, and output arbitration. The four-case production
  Meeting Assistant campaign exercises open/share/present, a follow-up during
  analysis, a visual alert during presentation, and spoken navigation
  correction through the graph-native profile; candidate-21 passed 4/4 with
  synchronized reviewed A/V and zero invalid actions or deadline misses.
- [x] Adaptive-observation video/YouTube watcher with explicit external ticks,
  fixed/adaptive/manual cadence, bounded change detection, latest-frame state,
  raw/reference separation, and typed visual-observer lifecycle controls.
- [x] Vision-language and silent computer-use agent with typed authority,
  target fencing, confirmation, execution, and visual feedback. The locked
  production Realtime-CU graph routes retained screen observations through
  observation commit and vision-language cognition, then requires provenance
  join, proposal admission, declared-tool lookup, confirmation, target fence,
  canonical-call commit, idempotent ledger commit, and dispatch before a client
  effect can occur. The exact tool result and subsequent visual observation
  re-enter the canonical trajectory before another effect is admitted. Its
  stable Realtime endpoint regression exercises that complete two-effect loop,
  and the clean reviewed Chromium-backed candidate campaign passed all sixteen
  pixel and set-of-mark cases with synchronized A/V and exact live graph
  attestation.
- [x] Independent voice and computer-use policies composed in one graph. The
  production Scenario Conversation topology has separate voiced and silent
  invocation branches over one committed trajectory. Voiced prepared text is
  the only branch connected to segmentation, TTS, and playback; either branch
  may propose an action only through the independent provenance, admission,
  declared-tool, confirmation, target-fence, canonical-call, ledger, and
  dispatch policy chain. Its stable Realtime endpoint regression now executes
  a voiced tool/result/speech turn and a distinct silent, target-fenced
  `computer.click` turn, proving that the computer-use action reaches the
  client while the silent branch emits no synthesized audio.
- [x] Audio-free typed text, image, file, and multimodal-attachment ingress,
  retention, and resolution component plus the locked text/file cognition and
  silent action agents described above.
- [x] Locked sidecar omni, duplex, and upstream topology graphs built over the
  shared `model.External` element rather than separate binding species.

Evaluation and direct-candidate tracker. The checked production surface is the
new plug-in/API design. Old implementations are reference material only; the
benchmark owner's recorded numbers are quality targets for the review, not a
second production arm or a reason to preserve migration code. Unit, race, vet,
fuzz, synthetic integration, mount, and protocol-conformance tests may close a
component checkpoint, but none establishes behavioral quality or closes this
tracker by itself:

- [x] Remove the benchmark migration/parity subsystem, paired-arm CLI flags,
  historical-registry implementation, legacy-baseline release gates, and final
  migration-comparison gate. Direct benchmark commands now exercise only the
  new implementation.
- [x] Delete the historical session-live route/header plug-in and fixed gateway
  HTTP facade. Negotiated inspection capabilities now name the canonical
  `/openrealtime/v1/sessions/{session}/live` resource and use only the
  management capability header; its source-free static-model sibling uses the
  same session authority, and compiled server profiles own all route mounts.
- [x] Freeze every direct candidate contract before its full run.
  - [x] Treat the benchmark owner's recorded original numbers as the trusted
    historical comparison targets. Do not reconstruct historical attempts or
    require historical media.
  - [x] Pin fixture/task revisions, model/provider revisions, voices, tools,
    timing policy, machine class, concurrency, trial count/seeds where
    applicable, and every graph/config/profile identity for each new run. The
    digest-checked dataset/preparation gates freeze the FDB, FD-Bench, and tau2
    populations; the release matrix freezes complete case/condition sets,
    transports, repetitions, review models, and review concurrency; reviewed
    execution plus live inspection binds each graph/config/profile and runtime
    identity before protocol work. Candidate provenance captures the exact
    executable and CPU/GPU/OS/architecture/Go host class, and comparison refuses
    build or machine drift. Tau2's last implicit run-defining values are now
    explicit and fail-closed: trial count, campaign and Python hash seeds,
    scheduler concurrency/workers, cadence, timeout, local caller/ASR/synthesis
    models and voices, and fresh per-condition run prefixes.
- [ ] Retain every new attempt—including failed, timed-out, and regressed
  diagnostics—with deterministic outcomes, exact graph/runtime evidence,
  playable audio and/or synchronized video as applicable, review manifests,
  and create-only external receipts.
  - [x] Make attempt-stage retention restartable for the direct FDB v1.5,
    FDB v3, FD-Bench, and tau-Voice campaigns. Each complete attempt now
    publishes an atomic canonical commit marker after its media, context,
    outcome, transcript, and artifacts are durable; `-review-resume` takes a
    crash-released exclusive lease, admits only the same suite/cell/origin,
    executable, and machine, reuses exact committed outcomes without replay,
    and preserves markerless or explicitly incomplete directories under
    `interruptions/` before retry. Finalization-stage debris remains
    fail-closed, and this subgate does not close the parent until every
    required population has actually run and been reviewed.
- [ ] Review every retained candidate recording with the exact
  `google/gemini-3.7-flash` plug-in. Advisory review exposes media and behavior
  problems but never changes the deterministic scorer.
  - [x] Review and independently reopen every recording in the four completed
    required candidate populations: Meeting Assistant 4/4, Realtime-CU 16/16,
    FDB v1.5 498/498, and FDB v3 100/100. Their 618 exact-model evaluations,
    usable media, create-only receipts, deterministic outcomes, and advisory
    disagreements remain sealed. The one-pass scenario 11×1, active FD-Bench,
    and both tau-Voice populations keep the universal review gate open.
- [ ] Publish a case-by-case pass/fail document that places the new totals,
  safety/deadline outcomes, and latency distributions beside the trusted
  historical numbers without inventing unavailable historical detail.
  - [x] Publish sealed media-linked case-by-case `REVIEW.md` reports for the
    four completed candidate populations. They preserve deterministic scoring,
    exact-model advisory findings, reportability, and each available safety or
    timing result without reconstructing historical attempts. The final shared
    side-by-side report remains open until every required population and its
    accepted historical comparison are present.

The historical scenario trail includes the accepted 140/165 total at fifteen
repetitions: 15/15 each for asked-not-to-be-interrupted, recorded-menu,
requested-silence, acknowledgement, ordinary-question, and count-as-they-go;
14/15 third-party conversation; 13/15 correction interruption; 11/15 each for
translation and visual description; and 8/15 waiter. Those totals remain useful
comparison context, but the 2026-09-02 operator-scoped checkpoint is exactly one
new retained attempt for each of the eleven cases. It is reported descriptively
and does not claim statistical parity with the historical 165-attempt sample.

- [x] Bind the shared benchmark/session evidence path to exact Graph IR,
  element/config/deployment identities, selected edges, authenticated live
  resolutions, and capability evidence.
- [x] Wire the scenario CLI to that authenticated graph-native evidence path,
  with exact per-attempt scopes for all eleven cases and explicit refusal of
  unattested execution behavior.
- [ ] Re-run and compare all eleven interaction scenarios once (eleven newly
  retained candidate attempts, one per case):
  The exact population, sealed review path, offline verifier, and exact-model
  advisory transport are implemented. The newest sealed 11×1 deterministic
  diagnostic, candidate run 18 at `85b6479`, passed 8/11 with all eleven
  attempts behaviorally reportable: asked-not-to-be-interrupted, recorded-menu,
  correction interruption, waiter, live translation, third-party conversation,
  acknowledgement, and ordinary-question passed. Its remaining failures are
  exact and retained: count-as-they-go said `One` only after split recognition
  of the first animal and never said `Two`; the silence timer fired at the
  requested time but asked whether the user was still reading instead of still
  there; and the second submitted build-complete image produced no answer.
  The complete exact `google/gemini-3.7-flash` advisory attempt retained usable
  media and evaluations for 11/11, agreed with 10/11 deterministic outcomes,
  and disagreed only on the waiter because its audio exposed invented future
  menu turns despite satisfying the narrow deterministic check. The immutable
  candidate binary credential-free reopened the source receipt
  `sha256:fa5e0a5e5d210de6d55486c82f273c5ddf9d6fd34830130725fc754fe2b1ff59`,
  all eleven source/media/evaluation bundles, and aggregate evaluation receipt
  `sha256:b91951da8b4f18a27b7af105e6eb5517d3aebc357e7d1ab035cb4365a6f3c1d8`.
  A distinct advisory attempt correctly remained nonreportable when the
  validator rejected a finding timestamp beyond the sealed WAV duration; no
  aggregate receipt was published. The current main branch now contains the
  model-independent evidence and continuation repairs below, but they have not
  yet been exercised by a new immutable live candidate. The requested one-pass
  eleven-attempt run has not started. This box remains open until those eleven
  attempts run once, are retained and reviewed/reopened, and receive an honest
  case-by-case report. A failure is recorded as a remaining behavior gap; it
  does not automatically trigger another complete scenario run.
  - [x] Preserve split same-speaker endpoints after the last actually played
    assistant boundary and pass exact typed transcript, quiet, and direct-image
    condition evidence through the activation guard. Commit `0e13305` covers
    second-clause animal evidence, canceled/unheard speech, current-image byte
    isolation, and visual condition recovery with normal, ten repeated, ten
    shuffled, race, and vet gates.
  - [x] Pin a bounded profile-owned continuation policy that forbids invented
    future turns and stage directions and preserves the requested action,
    subject, and wording for time- or event-deferred work. Commit `f872d38`
    carries the exact policy through immutable application configuration into
    each continuation invocation and passes focused repeated/shuffled/race,
    full-repository test, and vet gates.
  - [ ] `count-as-they-go`
  - [ ] `asked not to be interrupted`
  - [ ] `a recorded menu`
  - [ ] `cutting in on something wrong`
  - [ ] `ordering from a waiter`
  - [ ] `translating as they speak`
  - [ ] `waiting out a silence they asked for`
  - [ ] `somebody else's conversation`
  - [ ] `an acknowledgement is not an interruption`
  - [ ] `telling them what it saw`
  - [ ] `an ordinary question`
- [x] Run all four OpenRealtime Meeting Assistant v1 tasks through the new
  production profile. Clean-worktree candidate-21 at commit `c250d83` passed
  four independent focused two-case runs (8/8) before its immutable full run.
  The authenticated production-shared WebSocket campaign then passed 4/4 with
  zero infrastructure failures, invalid actions, deadline misses, or session
  timeouts. Every attempt is graph-native-attested with playable synchronized
  A/V, and all four exact `google/gemini-3.7-flash` reviews agreed with the
  deterministic pass with no significant, minor, or limitation findings. The
  retained result is
  `meeting-candidate-exact4-20260831-21-visual-horizons-full-reviewed.json`
  (`sha256:e7e56ea603b884b082f87315ed42e200b0fe3a4493375d2e9706b2538e486355`);
  its complete create-only source/evaluation receipt bundle is sealed by review
  manifest
  `sha256:e899491212b6426114661d0898d9797823207b660f59a85671f50cb93257d020`.
- [x] Run all sixteen OpenRealtime Realtime-CU v1 cases (eight task families
  under pixel and set-of-mark observation variants). Candidate-05 at clean
  commit `7f16cb1` completed 16/16 with zero infrastructure failures through
  the immutable binary, frozen Qwen/Whisper deployments, production profile,
  authenticated WebSocket, real Chromium capture, and live-inspection path.
  The normalized pixel contract raised pixel quality from candidate-04's 0/8
  to 4/8; set-of-mark held at 4/8, for 8/16 overall versus the trusted
  slow-only reference's 6/16. All eight successes met their deadlines versus
  five timely reference successes, while cue-to-action latency improved from
  4,645/13,579 ms reference p50/p95 to 671/1,296 ms. Observation-path latency
  remains a final aggregate non-regression concern: frame-to-observation p50
  rose from 6 to 321 ms and cue-to-observation p50 from 312 to 691 ms. All 16
  synchronized A/V attempts are graph-native-attested and reportable; the
  exact `google/gemini-3.7-flash` reviews were retained, reopened without
  credentials, and agreed with every deterministic outcome. The retained
  result is
  `realtime-cu-candidate-exact16-20260831-05-full-reviewed.json`
  (`sha256:1f61f502203c6e259cccc0eda67b7435fd8201660d54d3a436fe6f3020cf3d30`);
  its complete create-only source/evaluation receipt bundle is sealed by review
  manifest
  `sha256:dfa568a061dabc9323f74d195bccc891056d423e76f9360df6f0cc690ad93bcb`.
- [x] Compose the FDB v1.5, FDB v3, FD-Bench, and tau-Voice direct commands
  with the candidate-only evidence plug-in. `-review-prefix` reserves a fresh
  current-run source namespace, preflights exact `gemini-3.7-flash` without a
  remote model call, retains shared-session or external-harness WAV per attempt,
  publishes create-only per-attempt advisory evaluations plus a media-linked
  aggregate, and reopens the transitive receipts without credentials. The
  execution commands additionally accept `-review-resume` for the exclusive
  attempt-stage recovery boundary described above. The
  `review-candidate` command resumes an interrupted new campaign; the
  `verify-candidate-review` command verifies a finished one. Neither command
  reads or reconstructs historical attempts.
- [x] Run all 498 FDB v1.5 tasks through the new graph-native endpoint and
  retain per-attempt review media. Candidate-04 at clean commit `fc8195a`
  completed 498/498 with zero infrastructure failures and graph-native
  execution evidence on every task. Of 430 applicable cases, interruption
  yielded on time in 15/156, backchannel held in 90/90, background speech held
  in 89/89, and talking-to-other held in 93/95; another 68 recordings were
  honestly not applicable. Interruption latency remains a failed quality gate
  at 2,412/10,132 ms p50/p95. Exact `google/gemini-3.7-flash` review retained
  usable WAV media for all 498 attempts and agreed with 450/498 deterministic
  outcomes; its 215 significant findings (163 on interruption cases) are
  retained for the later non-regression repair loop. Credential-free reopening
  verified all 498 evaluations and zero quarantine. The retained result is
  `fdb-candidate-full498-20260831-04-full-reviewed.json`
  (`sha256:587bde40005606951fe785e6c70a931aaf785cc79eefa9589772f1b6ff9e73f1`);
  its complete create-only source/evaluation receipt bundle is sealed by review
  manifest
  `sha256:99b822af255d90b1f12696e66b0f467d76977fef261f1626d2e8ab7b8965bd60`.
- [x] Run all 100 released FDB v3 examples through the new graph-native
  endpoint and retain per-attempt review media. Candidate-02 at clean commit
  `f30ad89` completed 100/100 with zero infrastructure failures and
  graph-native execution evidence on every task. This closes the complete-run
  requirement, not the later quality/non-regression gate: only 9/100 tasks
  called every expected tool with the accepted arguments, 10/100 called every
  expected tool with a wrong identifier value, and 81/100 made no complete
  matching call. The exact-success split was ecommerce 2/29, finance 5/25,
  housing 2/26, and travel 0/20. Exact `google/gemini-3.7-flash` review retained
  usable WAV media and agreed with the deterministic outcome for all 100
  attempts; 95 attempts retained at least one significant finding (145 total).
  Credential-free reopening verified all 100 evaluations and zero quarantine.
  The retained result is
  `fdbv3-candidate-full100-20260901-02-full-reviewed.json`
  (`sha256:9b98c5a9cb6373f8e7846923dca3708f6141e6773139c489c383f9f74668c518`);
  its complete create-only source/evaluation receipt bundle is sealed by review
  manifest
  `sha256:73ba92f8d3cb2327127f23e35682f3719af840187d62806b5e6c9a2f5931720a`.
- [ ] Run all 6,147 FD-Bench conversations across all 21 released
  conditions with comparable endpointing, overlap, answer, and latency
  distributions; an aggregate over a subset of conditions is not a full run.
  The immutable candidate-04 run was healthy in the background and had reached
  attempt 976/6,147 at 2026-09-02 01:48 UTC. This dated progress checkpoint is
  not a partial-condition aggregate and does not satisfy the box.
- [x] Add a canonical pinned tau2 inventory boundary that calls the upstream
  `base` split, refuses dirty task/loader inputs and data-path overrides, and
  validates the exact 50 airline / 114 retail / 114 telecom partition.
- [ ] Run the complete 278-task tau2-bench/τ-Voice task set in
  both control and regular conditions with task and interaction metrics. The
  2026-09-01 preflight revalidated the patched checkout at
  `c3398666e6559e3a063da3fc04b5acf7f941464e`, its exact 50/114/114 base-split
  inventory, and the audio-native environment. Both release cells now pin one
  trial, seed 300 plus `PYTHONHASHSEED=0`, three in-process simulations and zero
  workers, 0.2-second cadence, ten-minute task timeout, fresh condition-specific
  run prefixes, and the exact caller, ASR, synthesis, and voice identities; a
  release-validation test rejects drift in any of those values. A retained earlier one-trial
  control diagnostic completed 278/278 and passed 160 (36/50 airline, 61/114
  retail, 63/114 telecom), but it predates graph-native execution evidence and
  the candidate review plug-in, so it remains nonreportable and is not reused.
  The exact current control and regular populations have not started.
- [x] Preserve the DynaCU-Bench runner as independent optional validation. The
  standalone `bench dynacu` command pins the unmodified AOI environment at
  `3c9f452aca697ee61de87a4907f0937d6c486a99`, requires the complete 100
  dynamic plus 50 static task inventory for a complete report, and remains an
  opt-in provisioned release-matrix entry with its own preparation script,
  driver, report boundary, tests, and documentation. It is not substituted for
  the repository-owned Realtime-CU release gate.
- [ ] Produce clean-worktree new-architecture artifacts for every required suite and
  compare them with the benchmark owner's accepted original numbers;
  investigate differences instead of accepting a merely runnable graph.
- [ ] Demonstrate that the final graph-native configurations meet or improve on
  the trusted original numbers in aggregate pass rate and per-case behavior, with
  no safety regression and no material deadline or latency-distribution
  regression.
- [ ] For every failed non-regression gate, retain the artifact, trace the
  changed outcome through exact graph/config/runtime evidence, fix the cause,
  rerun the affected cases, and then rerun the complete suite. Repeat until the
  full candidate—not only a patched subset—passes.
  - [x] Trace and repair the first retained FDB v1.5 interruption-latency
    defect without rewriting its failed candidate-04 result. Exact transcript
    evidence showed model generation becoming terminal while already-segmented
    TTS/playback remained active for seconds. The Scenario Conversation adapter
    now consumes bounded segmentation and post-effect playback receipts,
    tolerates cross-boundary receipt reordering, revokes the complete pending
    run before waiting for policy acknowledgement, and directly addresses each
    active playback/TTS utterance. Segmentation retains completed-stream
    utterance IDs only within its existing terminal bound and consumes them on
    the first late cancellation, so replay cannot duplicate speech interrupts.
    Focused normal, ten shuffled, race, affected integration, repository-wide
    test, and repository-wide vet gates are green. The affected interruption
    slice and then all 498 tasks still require immutable reruns after the active
    FD-Bench campaign releases the shared model deployments; this subgate does
    not close the parent quality gate.

### Phase 0: accept contracts and terminology

- [x] Review this design against current ADRs and measurement requirements.
- [x] Decide `.ortg`, normalized graph, and Graph IR versioning; the initial
  grammar; the strict YAML subset; and stable type-identity rules.
- [x] Mark the old audio/topology and slow-speech restrictions as historical
  observations rather than future kernel invariants.
- [x] Add architecture tests that preserve relevant behavior contracts as
  historical quality targets.

### Phase 1: build the graph foundation

- [x] Implement element descriptors, the `.ortg` parser and formatter, normalized
  YAML/JSON loader, shared graph elaborator, Graph IR, bounded edge
  declarations, source-mapped diagnostics, and deterministic fingerprints.
- [x] Implement symbolic element resolution, generated lockfiles, generic type
  unification, and inferred variadic lanes for `Tee`, `Mux`, and arbiters.
- [x] Add an optional typed Go builder for repository tests and generated graphs;
  make it call the same elaboration and validation path.
- [x] Implement `Tee`, backward-compatible event `Mux`, protocol-generic typed
  `Mux`, state, explicit sinks, and core validation.
- [x] Implement Mermaid/DOT export and static inspection.
- [x] Wrap the old binding runtime as one coarse reference element to prove the
  mount boundary. It is not an accepted production profile and remains slated
  for deletion.

- [ ] **Exit gate:** every production reference architecture runs directly
  through Graph IR with protocol conformance and complete new-candidate
  benchmark evidence; no coarse reference element is reachable.
  - [x] Scenario Conversation, Meeting Assistant, Realtime-CU, and the adaptive
    video reference construct fine-grained plans and start as `NativeBinding`/
    `NativeRuntime` sessions with exact adapter profiles, authenticated live
    inspection, and no compatibility-node identity. Remaining legacy voice,
    sidecar, and upstream launch paths plus their incomplete candidate evidence
    keep the universal parent gate open.

### Phase 2: decompose the component/cascade path

- [x] Extract acoustic admission/endpointing, ASR, observation commit,
  commit-bound trajectory-prefix context, explicit generation activation, fast/deliberative text
  cognition, model-result commit, interaction/segmentation, TTS, playback,
  tool authority, and typed outcomes into independently registered elements.
- [ ] Extract the remaining gateway/session adapters and any pacing/timing
  adapters required by the complete reference without falling back to a
  binding-owned event loop.
  - [x] Extract one policy-free `SessionAdapter` seam and the direct Scenario
    Conversation, Meeting Assistant, Realtime-CU, and adaptive-video protocol
    translators. `NativeRuntime` owns graph mount/run supervision, inspection,
    recording, shutdown, and graph-derived status; adapters only translate the
    stable session operations to typed graph boundaries and drain typed
    outputs. Adapters for the retained legacy launch families and remaining
    pacing/timing control flow keep the parent open.
- [x] Preserve trajectory compare-and-append and event-loop wake invariants in
  the graph-native store/commit feedback loop and direct behavior-contract tests.
- [x] Check in exact-lock conversational fast-only, slow-only, and
  both-speaking reference graphs with an identical non-routing backbone,
  explicit cancellation/timing boundaries, no model-to-model edge, fresh-lock
  assertions, values binding, and mounted provider-resolution evidence.
- [ ] Validate the reference graph directly at every safe point. Checked-in
  acoustic, ASR/trajectory, activation, and interaction components plus the
  complete conversational family cover the topology. The retained historical
  ordinary-turn artifact is diagnostic only; production acceptance comes from
  the new graph's direct integration and benchmark outcomes.
  - [x] Compile from fresh locks, bind, mount, and directly execute the acoustic,
    ASR/trajectory, activation, interaction, and complete fast-only/slow-only/
    both-speaking reference safe points. The payload-free ordinary-turn
    comparison is fingerprint-bound and retained as diagnostic evidence;
    shared-server integration and full live quality populations keep the
    parent open.
- [x] Add alternative tests where deliberative output speaks directly or fast
  and deliberative streams meet at an explicit stream-aware arbiter.

- [ ] **Exit gate:** no binding-specific control flow is necessary to express
  the componentized reference graph.
  - [x] Express and execute the direct conversational, Scenario Conversation,
    Meeting Assistant, Realtime-CU, and adaptive-video compositions through
    typed graph elements plus policy-free boundary translators under
    `NativeRuntime`. Retained legacy voice, sidecar, upstream, and pacing paths
    keep the universal no-binding-control-flow gate open.

### Phase 3: decompose sidecar, end-to-end, and upstream paths

- [x] Generalize the sidecar capability/type handshake through the versioned
  element-graph protocol while preserving its explicitly supported earlier
  wire-version negotiation.
- [x] Expose native audio, transcription, interaction acts, model state, text
  injection, tools, and cancellation as independent ports/capabilities.
- [x] Check in locked `omni`, `duplex`, and upstream topologies over the shared
  `model.External` element. All three exact artifacts compile, bind, mount
  through the same generic factory, negotiate their selected protocol-v4 ports
  and capabilities, and cross a typed audio/prepared-audio/outcome data plane.
  Provisioned provider quality remains part of the separate benchmark gates.
- [x] Express native and external interaction as topology changes over the same
  foreground element contract. The locked cross-mode regression now sends an
  `answer` act into the external-interaction graph and receives the same act
  from the native-interaction graph while both retain the identical common
  full-duplex data plane.

- [x] **Exit gate:** adding a new capability combination does not require a new
  binding package or runtime switch. Graph-selected optional ports and explicit
  live capability requirements produce the omni external-interaction, duplex
  native-interaction, and upstream native-interaction combinations through one
  descriptor, factory, runner, codec, and deployment registry; the locked
  mount/dial/data-plane regression exercises each without a mode branch.

### Phase 4: generalize modalities and authority

- [x] Extract typed visual observation plus still-image, attachment, file, and
  multimodal-content ingress/retention/resolution elements.
- [x] Add streaming camera/screen/video frame and capability-free reference
  ingress, explicit fixed/adaptive/manual observation cadence, bounded change
  detection/latest-frame state, and visual-observer lifecycle composition.
- [x] Extract action proposal admission, confirmation, target fencing, tool
  lookup, idempotent ledger commit, dispatch, and release/audit outcomes.
- [x] Compose visual reflex, slow planner, action arbitration, authority,
  execution, and visual feedback into complete computer-use reference graphs.
  The locked audio-free `visual-reflex-planner` graph independently activates
  fast and slow cognition from the same committed observation, binds each
  admitted proposal to exact candidate/result provenance, and lets the bounded
  `authority.ActionArbiter` expose only the first complete action to the shared
  confirmation, target, canonical commit, ledger, dispatch, and result-commit
  path. It cancels known and late losing runs, closes all-no-action observation
  groups, and fails closed on replay, identity drift, excess lanes, and bounded
  capacity. Its executed race regression proves exactly one dispatch and then
  proves that both lanes consume the canonical result plus changed-screen
  feedback without opening a second effect; focused normal, ten shuffled, race,
  broader elements/graphs test, and vet gates are green.
  - [x] Compose and execute the durable single-planner Realtime-CU reference
    from typed audiovisual observation commit through visual reactivation,
    one-effect-at-a-time admission, provenance, confirmation, target fencing,
    canonical/ledger commit, client dispatch, result commit, and changed-screen
    feedback.
- [x] Add an audio-free text/image/file/attachment component graph with typed
  exported contracts.
- [x] Add the complete text-and-files cognition agent and silent computer-use
  agent with no audio requirement. The locked `text_file_cognition` graph
  commits authenticated text/file observations, activates a deployment-selected
  silent model from the exact committed prefix, resolves file bytes only through
  the retained-media lease path, returns the lease, and commits prepared text
  without tool or audio authority. Its executed two-turn regression reads exact
  retained file bytes; the independently locked silent computer-use regression
  crosses the complete proposal-to-authorized-result chain without audio.
- [x] Preserve injection checks, confirmation, idempotency, target identity,
  and ledger release gates in decomposed action elements and adversarial tests.
- [x] Adversarially cover the refusal surface of the authority, provider,
  canonical-record, admission, descriptor-lock, policy, and capture validators.
  A mutation sweep — replacing one guard at a time with `if false` and rerunning
  the suite — found one hundred and four guards across thirty-two validators,
  in fifteen packages, that no test reached. The
  line above already claimed adversarial tests for target identity and ledger
  release gates; that claim was only partly supported, because four of the six
  guards in `validateTargetAuthorization` and the ledger's own check that a
  proposal and its authorized call are distinct items were among them.
  Also unreached: the candidate-result provenance join in full, the descriptor
  lock's dependency and effect equality and its generic resolution, the
  admission governor's interactive reservation, `ValidateDescriptor` in full,
  the refusal to show a tool catalogue to a model with no tool authority, the
  refusal of a receipt dated after the current instant, and the client-effect
  JSON depth bound in both directions. Later passes added the cognition-result
  coherence check, the audio-frame stream identity, the post-commit silence
  gate, the transcript-event act tables, completion provider state, the
  `element` package's own contract surface (`Canonical`, `ValidateIdentity`,
  `ValidateConcretePort`, `ValidateFor`, all at zero coverage), ASR input
  continuity, perception frame kinds, the vision input boundary, and the
  generation invocation's bounds. One hundred and one now fail under mutation.
  Of the three that do not, two are internal cipher-error paths no caller can
  reach and one is genuine redundancy: `durationSamples` already refuses an
  out-of-range sample rate with the identical message.

- [x] **Exit gate:** the architecture catalog accepts graphs whose exported
  contracts contain no audio ports. `graph/catalog.NewEntry` derives boundary
  contracts from a validated `config.Plan` rather than modality flags, and the
  deterministic strict catalog round-trip accepts a text-only input/output
  graph while asserting that neither exported boundary acquires an audio type.
  This closes audio-optional catalog admission; production catalog/profile
  integration remains the separate open Phase 5 item below.

### Phase 5: replace configuration and catalog assembly

- [x] Catalog graph definitions and immutable element/config/profile revisions.
  Every normal graph launch now derives an immutable `graph/catalog.Entry`
  from the exact prepared plan before assembly selection or adapter binding.
  The entry fingerprints Graph IR, exported contracts, lineage, exact element
  and implementation revisions, configuration/deployment/resolution identity,
  dependencies, effects, channel policy, maturity metadata, and only the
  empirical artifact identities derived from the separately bound evidence
  manifest. Callers cannot inject a competing profile list. Scenario
  Conversation, Realtime-CU, and Meeting Assistant publish explicit candidate
  metadata through their production constructors; launch results retain the
  entry and direct/profiled server graph bundles retain a validated frozen
  catalog document. Missing/invalid metadata fails before resource acquisition.
  Normal, effect-free regression, exact-production-profile, race, and vet
  coverage exercise construction, round-trip, mutation isolation, and drift
  refusal.
- [x] Replace topology-derived ownership validation with graph contract and live
  capability validation.
  - [x] Eliminate the duplicated ownership/capability state from the
    graph-native binding. `NativeBinding` no longer implements the legacy
    `Binding` interface or exposes `Ownership`/`Capabilities`; it returns a
    defensive copy of the one frozen, fingerprinted session-adapter profile,
    so construction cannot leave a second shadow copy that later drifts from
    the graph contract.
  - [x] Remove shared topology-combination restrictions and replace the
    graph-native gateway projection with one exact graph contract. Generic
    `Ownership.Validate` now checks only the owner vocabulary; the frozen
    `SessionAdapterProfile` fingerprints its graph, boundary map, ownership,
    and advertised capabilities. It refuses video, computer-use, observation,
    manual-turn, selectable-voice, audio-input/output, transcription,
    text-injection, or visual-input claims without their required typed
    operations; named observers additionally require observation support. It
    also refuses absent or drifted mounted boundaries. The gateway's session
    seam now contains only `Name` and `Start`; graph-native providers supply
    their exact Graph IR plus frozen adapter profile, which the gateway
    revalidates before using its operation-backed protocol capabilities.
    Authenticated inspection and benchmark evidence separately retain each
    live node capability. The version-2 server-provider contract carries the
    same projection-free seam. Retained legacy launches use a separately
    validated, construction-time snapshot until their independent Phase 8
    deletion gate; they cannot make `NativeBinding` regain the old interface.
    Normal affected suites, fifty shuffled graph/legacy contract runs, ten
    shuffled server-profile E2E runs, targeted race, vet, Windows/amd64 and
    Darwin/arm64 production builds, and the low-parallelism full repository
    gate are green.
- [x] Load separate `.ortg` or normalized graph artifacts, typed element values,
  deployment bindings, secret references, and evidence profiles.
  - [x] Topology, lock, strict values, deployment-binding, and secret-reference
    artifacts have bounded parsers and a compiler/CLI binding path.
  - [x] Resolve deployment-selected implementations and secret providers at
    mount time and attest their exact private artifact identities. Assembly
    selects exact immutable implementation/provider registrations against the
    frozen plan; preparation validates deployment and catalog fingerprints
    without resource acquisition; mount alone resolves erasable secret handles
    and emits canonical public/private deployment plus provider-runtime
    evidence. Drift, missing/excess catalog entries, provider mismatch, and
    ambient secret inputs all fail before factory mount or credential use.
  - [x] Add separate evidence-profile artifacts and the normal graph-native
    launch path over all of these planes. `graph/launch.New` now requires a
    distinct evidence manifest and binds every claimed profile to the exact
    prepared graph, plan fingerprint, node contract, and selected immutable
    implementation before assembly selection or adapter binding. Empty
    manifests explicitly make no claims but still bind to one graph. Scenario
    Conversation, Realtime-CU, and Meeting Assistant load their embedded
    manifests through their ordinary production constructors; launch results
    and direct/profiled server graph bundles retain the validated document for
    management and catalog consumers. Missing manifests and graph, plan, node,
    element, or implementation drift fail without resource acquisition.
- [ ] Delete topology flags and flattened configuration paths after their
  profile equivalents land; do not translate them into a compatibility graph.
  - [x] Make the strict graph-native `serve -launch-profile` path reject every
    explicitly supplied legacy binding, provider, model, voice, timing, and
    behavior selector before profile, credential, provider, or listener work.
    Its ordinary production composition consumes only the separately frozen
    graph/config/deployment/secret/evidence/application profile planes. The
    legacy flag-only serve path remains reachable when no launch profile is
    supplied, so its parser and constructors still keep the parent open.
- [x] Update benchmark architecture cells and gateway inspection to attest exact
  graph/config fingerprints, reviewed selected edges, live runtime identities,
  and capability evidence before credential access.

- [ ] **Exit gate:** normal new deployments do not configure topology through
  the ninety-plus serve flags.
  - [x] Make the strict production `serve -launch-profile` path consume only
    graph, values, deployment, secret, evidence, and application-profile
    artifacts and reject every legacy topology/provider/model/voice/timing/
    behavior selector before resource or credential work. The still-default
    flag-only legacy serve path keeps the universal deployment gate open.

### Phase 6: production inspection and authoring

- [x] Add live graph identity, exact node resolution, queue occupancy/counters,
  bounded flow histories, and queue-wait telemetry contracts.
- [x] Preserve each node's exact reaction and effect contract in the
  frontend-neutral static inspection model. Generated Mermaid/DOT and the
  browser authoring canvas now expose trigger, sampled-state, interrupt,
  outcome, concurrency, causal-break, effect, reversibility, and authority
  facts from the same immutable Graph IR; the projection is mutation-isolated.
- [x] Add complete live trigger, cancellation, authority, and per-stage latency
  operator views over those contracts.
  - [x] Populate reaction-declared trigger, unique active-run, interrupt,
    first-output, completion, and outcome evidence at the port boundary;
    preserve it through shutdown and payload-free trace replay without
    exporting raw item/run IDs.
  - [x] Retain the first observed trigger timestamp through live inspection,
    payload-free recording, and exact replay, and render trigger-to-first-output,
    trigger-to-completion, and trigger-to-cancellation durations in the joined
    browser operator view. Repeated triggers and shutdown cannot rewrite the
    first occurrence, while Go and JavaScript boundaries reject an output or
    completion that predates its retained trigger. This closes node-local
    trigger-relative timing.
  - [x] Join every exact static internal-edge delivery/depth contract to its
    redacted live queue evidence and render occupancy, high-water mark,
    enqueue/dequeue counts, drops, backpressure, and cumulative/per-dequeue
    queue wait as text-only channel cards. The public Go and browser boundaries
    reject invalid delivery/depth, counter arithmetic, undeclared or missing
    edges, and live occupancy beyond the immutable depth; focused adversarial
    JavaScript and real-Chromium DOM gates cover the complete projection.
  - [x] Join every retained pseudonymous live flow to its exact immutable
    internal-edge sequence and render the repeated cross-node stages, first/last
    traversal times, elapsed duration, and truncation state in the browser
    operator view. Feedback-loop edge repetitions remain visible instead of
    being rejected as duplicates. Empty flows, raw correlations, unknown graph
    edges, regressing timestamps, and markup-shaped node/edge identities fail
    closed before rendering; focused normal, ten shuffled, race, full
    presentation-subtree, real-Chromium, vet, and locked-bundle benchmark gates
    are green.
  - [x] Retain one monotonic timestamp per retained internal-edge traversal,
    including repeated feedback-loop edges; preserve the parallel sequence
    through recursively independent live snapshots, management redaction,
    session-keyed trace pseudonymization, deterministic encoding, and exact
    replay; and render each stage's mount-clock time plus delta from the prior
    stage. Older trace/live values may omit the optional sequence, while new
    values reject partial, regressing, out-of-span, future, or rewritten timing.
    Zero-origin mount clocks remain valid. Focused normal, ten shuffled, race,
    wider graph/management/presentation, vet, real-Chromium, redaction, and
    locked-browser benchmark gates are green.
  - [x] Project typed action-policy outcomes into a closed payload-free live
    authority-decision vocabulary: outcome kind, operation, irreversible-effect
    crossing, and mount-clock observation time. Action outcome publication
    refuses unknown vocabulary before emission; runtime telemetry ignores
    out-of-vocabulary projections and retains no call, run, session, provider,
    code, message, digest, or payload-derived timestamp. Recursively independent
    live snapshots, management redaction, bounded recording, deterministic
    encoding, and exact replay preserve the latest decision and its history;
    zero-origin clocks remain explicit and regressing clocks clamp rather than
    rewrite evidence. The joined browser node card renders the categorical
    outcome beside its immutable reaction/effect contract and rejects unknown
    or premature decisions; trace/replay additionally rejects future,
    disappearing, or regressing evidence. Focused normal, ten shuffled, race,
    wider subtree, vet, real-Chromium, redaction, and locked-browser benchmark
    gates are green.
  - [x] Serve the exact source-free static inspection model under the same
    narrow session capability and join it to redacted live node evidence in a
    text-only browser operator view. Identity drift, unredacted identifiers,
    and markup-shaped metadata fail closed; legacy live-only sessions are
    labeled rather than promoted to a fabricated contract view.
- [x] Add deterministic semantic graph diff plus bounded, fingerprinted,
  payload-free trace artifacts and exact Graph IR replay validation.
- [x] Integrate opt-in runtime recording with nonblocking coalesced capture,
  exact configuration/runtime/capability evidence, session-keyed correlation
  pseudonyms, explicit dropped-record evidence, deterministic export, and key
  erasure on shutdown.
- [x] Add a visual editor that reads and writes canonical `.ortg` or normalized
  YAML/JSON using the same descriptors and validator.
  - [x] Complete the effects-profile browser source cycle over separately
    authorized rooted read and publication boundaries. `SourceReading` requires
    its own `authoring.source.read` capability scoped to an opaque configured
    root identity; it is not implied by document paths, analysis, compilation,
    session inspection, or create/update authority. The held-root reader
    accepts only canonical bounded `.ortg`/YAML/JSON paths, rejects symlinked,
    hard-linked, nonregular, empty, oversized, malformed UTF-8, identity-swapped,
    and concurrently changed targets or parents, and serializes acquisition
    with publication. Its exact source response binds root, path, byte count,
    source digest, and a deterministic result digest. The API, headless client,
    host relay, and browser client independently revalidate that binding and
    keep the capability in its header. Only effects-enabled browser profiles
    receive the source-reading plugin and network grant; observer profiles
    receive neither read nor publication access. The editor's Load control does
    not publish unsaved textarea contents: it replaces them with the exact
    rooted source, advances workspace identity, clears stale analysis,
    rendering, and publication state, updates the predecessor digest, and
    retains only payload-free read evidence beside the document. Traversal,
    wrong-root, missing or unstable targets, source and result forgery,
    malformed Unicode, duplicate or unknown JSON, provider loss, disposal,
    Go/JavaScript digest parity, read-versus-write authority separation, and
    target/parent races are covered through rooted unit tests,
    server/client/relay tests, JavaScript conformance, and real Chromium
    create/update/load/final-file verification.
  - [x] Apply compiler-produced canonical `.ortg` formatter edits through the
    browser workspace without adding network, filesystem, or effects authority.
    The management client independently binds the edit set to the exact path
    and SHA-256 source identity, validates bounded UTF-8 byte positions and
    line/column pairs, exact old bytes, ordering, non-overlap, and output size,
    and rejects missing, recovered, stale, forged, overlapping, or inconsistent
    canonical/noncanonical results. The editor enables Format only for the
    analyzed noncanonical workspace, refuses a textarea changed after analysis,
    atomically advances the workspace epoch while preserving the graph
    revision, and clears stale analysis, compilation, rendering, read, and
    publication projections. JavaScript conformance includes multibyte source
    positions and proves that local formatting makes no request; the
    effects-enabled and both WebSocket/WebRTC observer real-Chromium profiles
    render the exact noncanonical diagnostic, install canonical bytes, compile,
    and render afterward. Three shuffled complete browser runs, repository-wide
    tests, and vet are green. This formatter checkpoint does not itself claim
    graphical mutation.
  - [x] Initiate canonical `.ortg` node mutation from the rendered browser
    canvas without granting direct filesystem, effects, or hidden source
    authority. Deterministically sorted node controls retain the exact compiled
    fingerprint; the workspace rejects stale selection, collision, no-op, and
    exhausted-revision requests before mutation, then applies the compiler's
    source-digest-bound declaration/edge/boundary edit set under epoch/request
    compare-and-swap, advances the document revision exactly once, and clears
    stale analysis, compile, rendering, source-read, and publication state. A
    distinct editing contract keeps the existing cross-platform analysis/
    compile/render contract stable. The management engine, capability registry,
    strict HTTP API, headless client, host relay, and browser transport each
    authorize or independently validate the operation; the browser additionally
    lexes canonical topology syntax and requires the edits to cover the exact
    selected-node span set. Missing, extra, overlapping, stale-position, wrong-
    name, wrong-evidence, invalid-input, collision, late-response, provider-loss,
    and disposal cases fail closed. Go/JavaScript conformance, targeted race,
    effects Chromium, both observer transports, and the shared browser/native
    companion gate compile the original `runtime`, rename it graph-wide to
    `renamed_runtime`, recompile revision 2 under a changed fingerprint, and
    render the renamed node. Edge removal, creation, and normalized YAML/JSON
    mutation are covered by the following independently evidenced slices.
  - [x] Remove one selected canonical `.ortg` edge from the compiled canvas
    without granting filesystem, effects, or hidden source authority. The
    syntax AST now owns the exact named-or-endpoint-derived edge identity used
    by Graph IR; the editor removes exactly one statement and its attached
    comments through a source-digest-bound atomic edit. The management engine,
    capability registry, strict HTTP API, headless client, host relay, browser
    transport, and separately versioned editing/workspace contracts each
    authorize or independently revalidate the path, source digest, edge
    identity, edit span, canonical result, and response evidence. The workspace
    binds selection to the compiled fingerprint and revision, rejects stale or
    late requests, advances revision exactly once, and clears every stale
    analysis/compile/render/read/publication projection. Named, unnamed, lossy,
    missing, duplicate, noncanonical, multibyte-comment, forged-result,
    overlapping/stale-position, revoked-capability, provider-loss, and disposal
    cases fail closed. JavaScript conformance plus effects, WebSocket observer,
    WebRTC observer, and shared browser/native-companion Chromium gates select
    `optional`, remove it after the node rename, recompile revision 3 without
    that edge under a changed fingerprint, and preserve the renamed runtime.
    Edge creation and normalized YAML/JSON mutation are covered by the next
    independently evidenced slices.
  - [x] Create one selected named canonical `.ortg` edge from deterministic
    compiled output/input port controls without granting filesystem, effects,
    or hidden source authority. The request binds the canonical source,
    revision, edge identity, endpoints, delivery mode, and exact predecessor
    Graph IR fingerprint. The management engine recompiles that predecessor,
    generates one source-digest-bound canonical edit, locks resolution, and
    compiles the candidate at the next revision before returning its distinct
    fingerprint. The capability registry, strict HTTP API, headless client,
    host relay, browser transport/editing service, and versioned workspace
    contract independently revalidate the request, edit, predecessor/candidate
    identities, and payload-free response evidence. The workspace admits only
    ports with the selected compiled direction, rejects stale, duplicate,
    invalid, exhausted-revision, or late requests, advances the document once,
    and clears every stale analysis/compile/render/read/publication projection.
    Lossless and lossy syntax are supported at the API boundary while the
    canvas creates the explicit lossless default. Missing or ambiguous nodes,
    incompatible direction/contract compilation, noncanonical source, forged
    identities/positions/content/evidence, revoked capability, provider loss,
    and disposal fail closed. Go and JavaScript conformance, repository-wide
    tests and vet, targeted race, all real-Chromium profiles, and the shared
    browser/native-companion gate recreate `producer.out -> sink.in` as
    `restored` after removal, recompile revision 4 under the server-produced
    candidate fingerprint, and preserve `renamed_runtime`.
  - [x] Extend the same compiled-fingerprint-bound canvas mutations to exact
    canonical normalized YAML/YML and JSON without adding another graph model or
    authority plane. An immutable normalized editor parses through the strict
    manifest frontend, rejects noncanonical bytes, ambiguous declarations,
    collisions, missing edges/endpoints, stale source identity, invalid delivery,
    and bounds violations, then regenerates exactly one full-document edit for
    graph-wide node rename, named or endpoint-derived edge removal, and named edge
    creation. The management engine recompiles edge-creation predecessors and
    candidates through the same compiler and resolution lock; request/result
    validators, strict HTTP server, headless client, host relay, browser client,
    and workspace independently rebind the exact path, source digest, canonical
    result, predecessor/candidate fingerprints, and response evidence. Browser
    JSON uses the duplicate-key-checking codec and reproduces Go field order and
    escaping; browser YAML accepts only the deterministic manifest layout and
    structurally updates nodes, endpoints, edges, and boundaries. YAML, YML, and
    JSON editor tests; YAML/JSON engine and mounted HTTP sequences; normalized
    relay requests; adversarial JavaScript; repository-wide test/vet; targeted
    race; all real-Chromium profiles; and the shared browser/native companion are
    green. Real canvas runs compile, rename, recompile, remove, recompile, create,
    and finally recompile both YAML and JSON under distinct server fingerprints.
- [x] Add an immutable, pure language-service snapshot with compiler-backed,
  source-mapped diagnostics, stable source-digest cursors, bounded results, and
  recursively independent metadata for concurrent readers.
- [x] Generate descriptor-derived element metadata plus element, compatible
  node, and directional port completion for canonical `.ortg` source.
- [x] Resolve separate values-schema references and generate JSON Schema and
  configuration-value editor metadata without mixing values into topology.
  - [x] Generate and compile immutable descriptor-derived values schemas with
    strict graph/node envelopes and deterministic content digests.
  - [x] Support explicit schema resolution and truthful unresolved-contract
    reports without inferring fields from opaque descriptor references.
  - [x] Expose bounded resolved property metadata, completeness, identities,
    and diagnostics through immutable management Authoring API snapshots.
  - [x] Render all of that metadata in the browser configuration editor through
    a strictly revalidated, text-only client projection with exact absent/null
    handling and adversarial markup coverage.
  - [x] Render the same metadata in a bounded deterministic LSP plaintext hover
    with exact UTF-16 position conversion and adversarial markup coverage.
  - [x] Render the same metadata in native-client configuration editors through
    an analysis-only, header-capability-scoped management provider and a
    deterministic plaintext SwiftUI projection. Both native profile manifests
    pin the provider, its management permission ceiling, and the view
    dependency; strict Swift response validation preserves every values-contract
    field and absent/null distinction without interpreting metadata as markup.
- [x] Add bounded graph-aware node rename, hover, port completion, and
  go-to-definition for fully parsed canonical `.ortg`.
- [x] Add independently bounded recovery parsing for incomplete `.ortg` and
  source-digest-bound formatter edits through the UI-independent management
  Authoring API, while keeping recovery outside compile/reconcile paths.
- [x] Add LSP/UI rendering, multi-file/subgraph navigation, and strict
  file-write mediation.
  - [x] Project immutable single-document diagnostics, exact-symbol
    completions, and the complete values-contract hover into deterministic,
    bounded LSP wire types. All ranges use zero-based UTF-16 coordinates;
    completions retain the source digest plus exact descriptor revision/digest,
    and truncated diagnostics fail instead of being mislabeled as a protocol
    `full` report. Split-surrogate, stale-span, unknown-severity, truncation,
    recovered syntax, compiler-backed Unicode, deterministic JSON, concurrent
    reader race, shuffled, vet, and performance gates are covered without
    adding file or process authority.
  - [x] Project go-to-definition, node rename, and formatter edits into exact
    LSP location and workspace-edit forms without inventing descriptor spans or
    losing immutable snapshot identity. Same-document targets use the caller's
    validated document URI; descriptor and port targets use deterministic,
    read-only plaintext virtual documents with generated real ranges and a
    path-only credential-free URI. Rename and formatting return only versioned
    `documentChanges` after exact path, URI, version, and source-digest checks.
    Stale identity/span/text, invalid URI/version, overlap, malformed UTF-8,
    recovery-format, markup, source/virtual-target, deterministic concurrent
    reader, shuffled, race, vet, and performance gates fail closed or pass as
    applicable; the boundary still performs no I/O.
  - [x] Add bounded, versioned document synchronization and a transport-neutral
    JSON-RPC/LSP adapter without granting it filesystem authority. The adapter
    freezes the latest descriptor catalog at construction, owns only immutable
    in-memory snapshots, requires monotonically increasing nonnegative 32-bit
    versions, accepts exactly one full-text change, and stages analysis before
    compare-and-swap publication so rejected, stale, concurrent, incremental,
    oversized, and over-capacity changes cannot mutate the retained document.
    Its one-message JSON-RPC 2.0 boundary has independent request, response,
    document, URI, method, and ID limits; duplicate-safe structural validation;
    strict envelopes and typed method params; preserved string/integer IDs;
    standard lifecycle, error, cancellation, and content-modified forms; and
    UTF-16/full-sync capabilities. It serves diagnostics, completion, hover,
    definition, versioned rename, standard formatting edits, and exact
    read-only virtual descriptor documents without sockets, framing, file or
    process access, workspace discovery, or mutation authority. Lifecycle,
    malformed/duplicate/unknown JSON, missing/null/fractional/oversized IDs,
    stale and adversarial synchronization, frozen-catalog, deterministic wire,
    disposal erasure, concurrent reader/change race, ten shuffled runs, vet,
    and allocation/performance gates are green.
  - [x] Add an explicit workspace/index boundary for digest-bound multi-file and
    subgraph navigation. The immutable UI-independent index accepts only an
    explicitly supplied, independently bounded document set; verifies every
    path, URI, version, source digest, canonical source span, aggregate byte,
    and import count; and fingerprints the sorted population. Portable logical
    paths and credential-free relative URI paths resolve only within that
    closed set—there is no root discovery, file read, loader fallback, or
    mutation capability. Import strings and aliases, imported/nested graph
    references, and subgraph boundary ports project to real UTF-16 graph and
    boundary ranges while preserving the exact target path, URI, version, and
    digest. Missing, stale, recovered, noncanonical, duplicate-path/URI,
    duplicate-alias, wrong-graph, absent-boundary, absolute-URI, over-byte,
    over-document, and over-import inputs fail closed; import namespaces cannot
    silently fall back to similarly named descriptors. The LSP adapter enforces
    the same aggregate bounds transactionally, rebuilds the index from leased
    immutable open documents for definition requests, returns standard
    LocationLinks only after whole-workspace lease validation, and
    preserves ordinary descriptor and same-document definitions. Logical/URI,
    default/explicit alias, nested, deterministic reorder/version fingerprint,
    caller-mutation isolation, target-change race, ten shuffled runs, vet, and
    allocation/performance gates are green without adding filesystem or process
    authority.
  - [x] Mediate authorized creates and updates with rooted path containment,
    stale-content checks, atomic/no-replace publication, and audit receipts.
    Filesystem authority is a separate optional `SourcePublication` contract,
    never inferred by the pure authoring engine, LSP adapter, document path, or
    session bearer. Its operator API requires distinct
    `authoring.source.create` and `authoring.source.update` grants scoped to an
    opaque configured root identity. The publisher accepts only canonical
    bounded relative `.ortg`/YAML/JSON paths, opens an absolute non-root
    directory through a retained `os.Root`, rejects symlinked or identity-swapped
    parents/targets and hard-linked files, creates no directories, and returns
    no root locator or source payload. Same-parent private stages are bounded,
    mode-preserving, single-link, and synced before commit. Linux uses
    `RENAME_NOREPLACE` for create and `RENAME_EXCHANGE` for update; Darwin uses
    `RENAME_EXCL` and `RENAME_SWAP`; platforms lacking both guarantees reject
    publisher construction. Updates independently read a stable predecessor
    before staging and immediately before commit, require the exact expected
    SHA-256 digest, recheck parent, target, stage, and link identities, and
    re-read and verify both sides of the atomic exchange before removing the
    superseded name. Each success returns a
    deterministic payload-free receipt binding root identity, operation, path,
    previous/new source digests, byte count, and any cleanup-pending state.
    The optional plugin route and headless client revalidate that receipt;
    malformed JSON, wrong-root/wrong-operation capabilities, traversal,
    external/internal symlinks, hard links, directories, missing/swapped
    parents, existing creates, stale/raced updates, cancellation, forged
    receipts, concurrent create/update winners, root-directory rename, and
    whole-file atomic reader visibility are covered by ten shuffled runs, the
    race detector, vet, and an approximately 1.10 microsecond receipt-validation
    benchmark. The effects-enabled browser profile now adds an independently
    locked source-publication client and explicit create/update controls; its
    network ceiling includes publication only in that profile, while observer
    profiles have neither the service nor the grant. The client, host relay,
    and server independently bind each operation to the opaque root identity,
    exact path/source/predecessor, and recomputed receipt digest. The same
    editor renders the strictly revalidated bounded source-mapped diagnostic
    report only through text nodes. Invalid paths, roots, Unicode, stale or
    unchanged updates, forged receipts/host evidence, markup-shaped messages,
    provider loss, and disposal fail closed in JavaScript, Go relay, and real
    Chromium coverage; the real browser create/update path crosses the mounted
    publisher and is verified against the final rooted file bytes. Broader
    graphical topology editing stays under the separate visual-editor item
    rather than granting implicit file authority.
- [ ] Implement the composable presentation and observability design before
  treating any current UI as the production inspector.
  - [x] Inventory the former standalone presentation clients, the macOS client,
    gateway UI coupling, duplicated client protocol/media logic, and existing
    browser/native test evidence.
  - [x] Specify the shared server/host/client plugin contract, clean realtime
    and management APIs, client profiles, trust boundaries, rollout/retirement stages,
    cross-client E2E matrix, and presentation performance gates.
  - [x] Bind standalone companion readiness to the exact immutable browser
    manifest before announcing the profile. A successful HTTP response with a
    wrong media type, malformed manifest, or mismatched manifest/plan identity
    now fails immediately as permanently incompatible; transport and warming
    failures remain retryable. Fifty repeated focused runs, shuffled coverage,
    race, vet, Windows/amd64 and Darwin/arm64 production builds, and the
    low-parallelism full repository gate are green.
  - [ ] Implement descriptor-locked server, presentation-host, and client
    plugin realms with dependencies, permission ceilings, scoped effects, and
    independently inspectable runtime identities. The common descriptor/lock,
    host mount, permission, scoped-service, and browser lifecycle foundations
    exist; the exact server-realm assembly is covered by the following subgate,
    while remaining host/client replacement and signed-native lifecycle/leak
    evidence remain open.
  - [x] Assemble the clean Realtime server as an exact descriptor-locked server
    realm before opening a listener. The immutable profile, lock, plan, runtime
    artifact identities, dependency graph, permission-free server scope, and
    single HTTP export bind the session provider, inspection authority, gateway,
    realtime/observability routes, and canonical session management API. Missing
    dependencies, mutable artifacts, duplicate mounts, and conflicting routes
    fail before service; exact live evidence names all seven selected entries.
    A server-specific replacement test proves that a conflicting observability
    implementation restores the prior routes, while a valid replacement keeps
    the stable router/export identity and advances only its implementation and
    runtime evidence. Closing the actual bundle now proves every entry closed
    with zero workers, effects, and services, makes the export unavailable,
    removes `/healthz`, and remains idempotent. Focused server assembly,
    replacement/rollback, and close-ownership tests are green; topology/session
    resources outside this realm and cross-realm leak evidence remain under the
    open parent.
  - [x] Prove dependency-ordered cleanup across the unchanged clean-server
    browser/macOS-profile composition. After the real Chromium client and the
    shipped native-manifest wire probe each close their provider session, the
    eight-entry presentation-host realm closes first with zero workers,
    effects, and services, makes every export unavailable, and removes its
    manifest and realtime routes. The independently mounted seven-entry server
    remains exact, active, healthy, and metric-stable through that host loss;
    its later close reaches the same zero-ownership state and removes health
    and metrics. The focused real-browser cross-client gate is green. Host and
    client replacement plus the signed native lifecycle/leak matrix remain
    open.
  - [x] Replace and remove a descriptor-locked presentation-host route
    capability without replacing its router or HTTP export. A conflicting
    implementation is atomically refused and the prior route is restored; a
    valid implementation advances only the selected entry's implementation
    and runtime evidence while the stable router, independent route, and export
    identity remain unchanged. Explicit removal withdraws only the selected
    routes, and final idempotent close leaves every host entry with zero
    workers, effects, and services and no available export. Focused normal,
    race, and vet coverage is green. Replacement of the remaining host and
    client capabilities remains under the open parent.
  - [x] Reconcile multiple descriptor-locked presentation-host route
    capabilities as one candidate while preserving the stable router and HTTP
    export. Both candidate rows complete effect-free pre-mount before the old
    routes retire; a cross-candidate route collision activates neither final
    row, returns no receipt, removes the candidate surface, and restores both
    predecessor implementations and endpoints. A valid two-row transition
    returns sorted implementation/artifact evidence plus exact retirement
    audits with every worker, effect, child scope, and service count at zero.
    The unchanged router/export identity survives both rollback and success,
    and final host close removes every route with zero realm ownership. Ten
    shuffled focused runs, focused and package-wide race, full repository
    tests, and vet are green.
  - [x] Reconcile a stateful presentation-host route through its exact schema,
    snapshot/restore lifecycle, and explicit candidate migrator. A candidate
    without migration support is refused after effect-restricted pre-mount but
    before snapshot or route teardown. A migrated candidate that fails after
    registering its route is completely disposed and restores the predecessor
    implementation, state, endpoint, and runtime evidence with no receipt. A
    valid transition serves the migrated state and returns only schema,
    before/after state digests, migrator identity, transition identity, and
    zero-retained-ownership retirement evidence—never state bytes. Final host
    close removes the route and disposes the adopted pre-mount scope exactly
    once. Ten shuffled focused runs, focused and package-wide race, full
    repository tests, and vet are green.
  - [x] Reconcile the shipped WebSocket presentation-host relay while a real
    proxied session is active. The candidate pre-mount resolves the exact
    endpoint and credential services and requires its network grant before any
    live teardown; a permissionless implementation candidate returns no
    receipt, leaves the predecessor sequence and implementation unchanged, and
    continues relaying on the original socket. Every admitted session is now a
    named plugin-lifecycle worker. A valid replacement cancels and joins the
    active predecessor before candidate activation, advances exact runtime
    artifact evidence, preserves the stable router/export identity, and emits
    a retirement audit with zero workers, effects, child scopes, and services.
    The replacement serves a fresh bidirectional socket and final host close
    retains no ownership. Ten shuffled focused runs, focused and package-wide
    host/browser race and integration gates, full repository tests, and vet
    are green. WebRTC and management-relay replacement remain open.
  - [x] Remove and recover a descriptor-locked browser client provider through
    its real dependency closure. In the 28-entry WebRTC developer profile,
    deactivating media quiesces transport, reducer, session, authoring, effect,
    and resource dependents while the independent operator branch stays
    active; reactivation remounts the desired closure, reconnects, renegotiates
    effects, and executes without stale session state. Final disposal leaves
    no mounted plug-in and every row undesired and inactive with zero scoped
    effects, services, or lifecycle errors. The focused real-Chromium gate is
    green.
  - [x] Replace authenticated stateless browser client implementation bytes
    inside one immutable descriptor and plan. A composed descriptor may admit
    multiple exact content-addressed assets while its manifest selects only
    one implementation. The browser verifies the complete candidate manifest,
    unchanged plan/assets/endpoints/grants, exactly one changed row, selected
    asset digest, module identity, and stateless boundary before teardown; it
    then quiesces only that row's dependency closure. Activation failure
    disposes the candidate, restores the exact predecessor module and
    dependents, leaves the sequence and manifest identity unchanged, and
    returns no receipt. Success advances a payload-free plan/manifest/
    sequence/artifact receipt and live identity. Unsealed and even correctly
    re-sealed payload-bearing candidates fail before disposal. Repeated real-
    Chromium rollback/success and final zero-effect/service/DOM ownership,
    focused race, broader presentation, full repository test/vet, and exact
    Go/JavaScript manifest checks are green. Topology-changing client
    replacement remains under the open parent.
  - [x] Replace multiple authenticated stateless browser client implementations
    as one atomic transition. The browser validates that requested entries are
    distinct, active, desired, and stateless; verifies that the sealed candidate
    changes exactly that set; and authenticates every selected module before
    teardown. It then quiesces the union of their dependency closures and either
    activates the complete set or disposes the candidate closure and restores
    every predecessor implementation without advancing live identity. A
    payload-free format-v2 receipt binds the exact per-entry transitions on
    success. Real-Chromium activation failure, rollback, success, exact
    disposal/remount ownership, and final zero-ownership checks pass under ten
    shuffled repetitions, focused and package-wide race coverage, full
    repository tests, and vet.
  - [x] Migrate stateful browser client implementations through an explicit
    descriptor-locked state lifecycle. Opted-in modules must register one live
    snapshot callback, consume restored state before publishing services, and
    provide an explicit candidate migrator for every changed stateful row. The
    browser captures and canonicalizes bounded strict-JSON object state before
    teardown, retires the dependency closure, migrates changed rows, and passes
    unchanged stateful dependents their exact predecessor snapshots. Missing
    migration support and snapshot failure refuse before teardown; invalid
    migrated state and candidate activation failure restore every predecessor
    state, implementation, manifest identity, and sequence. A payload-free
    format-v3 receipt records only schemas, state digests, migrator identities,
    implementation transitions, and sequences. Real-Chromium refusal,
    migration, rollback, unchanged-dependent preservation, exact disposal, and
    final zero-ownership checks pass under ten shuffled repetitions, focused
    and package-wide race coverage, full repository tests, and vet.
  - [x] Replace the shipped effects and artifact-reference browser providers
    together through their real capability subtree. The developer composition
    API admits deterministic, content-addressed alternatives only for existing
    entries and refuses unknown entries, empty sources, invalid names, and
    primary or alternative entrypoint collisions without widening endpoints,
    grants, or the selected implementations. In real Chromium an authenticated
    format-v2 transition changes exactly `effects` and `artifact-references`,
    quiesces their union dependency closure, renegotiates the signed effect
    catalog, and executes the replacement path to render and verify the sealed
    artifact. Final disposal leaves every browser row inactive with zero scoped
    effects and exactly two started/zero active effect sockets. The focused
    test passes five shuffled repetitions; the complete browser package, its
    race gate, full repository tests, JavaScript syntax, and vet are green.
  - [x] Remove and recover providers through the portable native client
    composition. Media loss and effect loss quiesce only their exact transitive
    consumers in reverse order, preserve unrelated services, and remount in
    manifest order; a failed remount restores the same degraded service set and
    remains retryable. Final shutdown after recovery disposes the complete
    provider set exactly once in reverse dependency order, makes every service
    unavailable, is idempotent, and refuses a later remount.
    Strict-concurrency Swift conformance is green. The signed Darwin
    application lifecycle remains a separate open gate.
  - [x] Complete the browser presentation server and browser client as
    manifest-composed plugins over the public server APIs. Locked
    minimal, observer, developer-WebSocket, and developer-WebRTC profiles now
    exercise text, media, effects, artifacts, inspection, trace, and authoring
    against a standalone host in real Chromium, and gateway-owned UI is
    retired. The duplicate standalone forks are deleted rather than retained as
    compatibility paths. A descriptor-supplied unpredictable-tool challenge
    replaces their real-model browser gate; its hermetic real-Chromium form is
    green and its provisioned real-model form remains an external release gate,
    not a locally claimed provider result.
  - [ ] Compose the macOS application from the same logical client services and
    public server APIs with native implementations. Native manifests,
    provider registry/factory seams, reducer, transport, media, effects,
    artifact, inspection, and view boundaries exist. The unchanged-server
    browser-to-macOS-profile comparison passes; a signed Darwin application run
    remains required.
  - [ ] Pass the shared browser/macOS/headless protocol, media, tools,
    inspection, reconnect, reconciliation, leak, and performance gates. Real
    Chromium developer/observer WebSocket and WebRTC profiles, the unchanged
    clean-server browser/macOS protocol probe, JavaScript conformance, portable
    strict-concurrency Swift tests, shuffled race gates, and focused
    presentation benchmarks are green. The 2026-08-30 candidate rerun retained
    passing release reports for the complete real-Chromium profile suite, the
    unchanged-server browser/macOS-profile probe, JavaScript conformance, and
    strict-concurrency Swift-Linux conformance under
    `.runtime/release-validation/*-candidate-01`; focused normal, three
    shuffled runs, race, vet, and presentation/client benchmarks also passed.
    The unchanged-server cross-client gate now also proves dependency-ordered
    presentation-host/server shutdown with zero retained plugin ownership and
    independently disappearing HTTP surfaces. A signed native Darwin
    application run, remaining topology-changing and capability-specific
    host/client replacement, native lifecycle/leak evidence, and the complete
    release-matrix rerun remain open.

- [x] **Exit gate:** an operator can understand a running graph and trace an
  output to its causes without reading binding source.
  - [x] Join immutable static graph semantics to redacted live node and channel
    evidence in the production browser view, including trigger-relative node
    timing, cancellation/outcome state, queue depth and occupancy, loss,
    backpressure, queue wait, exact pseudonymous cross-node flow stages, and the
    latest categorical authority decision with irreversible-boundary status.
  - [x] Retain each accepted internal flow traversal's exact direct envelope
    parents beside its edge and mount-clock timestamp under an independently
    bounded causal-stage contract. Invalid, self-parented, duplicate, excess,
    partial, or rewritten assertions truncate rather than fabricate lineage.
    Public management views replace raw item and parent identities with stable
    snapshot-local pseudonyms; recordings first apply a session-secret HMAC
    and then a fixed-width domain-separated trace identity. Recursive cloning,
    management validation/redaction, deterministic trace encoding, compaction,
    exact replay, legacy absence, cross-flow consistency, browser validation,
    text-only rendering, and real Chromium coverage are green.
  - [x] Classify retained standard graph-native causal stages with the closed,
    payload-free `observation`, `state_revision`, `policy`, and `model_run`
    vocabulary. Only typed payloads can opt in; an absent projection remains
    explicitly unclassified for legacy or third-party values, while invalid or
    rewritten classifications truncate or fail closed. Standard perception,
    trajectory, generation/semantic/action-policy, and cognition payloads
    supply the exact classification. The mounted four-stage causal-chain test,
    same-item and cross-flow consistency checks, management redaction, session-
    keyed recording, deterministic trace encoding/replay, legacy absence,
    exact Go/JavaScript vocabulary parity, text-only rendering, and real
    Chromium coverage are green.

### Phase 7: dynamic reconciliation

- [ ] Implement scoped dependencies and reversible lifecycle effects across
  graph/session reconciliation; that wider ownership is incomplete.
  - [x] Compile exact required and optional service dependencies through nested
    visibility/isolation scopes, expose only descriptor-bound services and
    deployment grants, and own every publication, supervised worker, and
    disposer through one mount lifecycle. Required-provider loss and worker
    failure remove services and quiesce the transitive consumer closure;
    activation remounts desired consumers, failed mounts unwind, close is
    idempotent and dependency-ordered, and unresponsive workers are named while
    registered effects are still disposed. Normal, ten shuffled, race, and
    adversarial permission/config/typed-nil coverage is green.
- [ ] Add candidate validation, pre-mount, safe-point swap, state migration,
  rollback/refusal, and leak detection.
  - [x] Validate immutable-plan implementation, canonical config, and narrowed
    permission candidates completely before teardown; bind them to the exact
    plan fingerprint and observed lifecycle sequence; refuse stale, no-op,
    inactive, unknown, duplicate, over-ceiling, invalid-config, canceled, and
    stateful-without-migrator requests; quiesce the affected dependency closure;
    and either return a deterministic transition receipt or clean the failed
    candidate and restore the complete prior composition under the realm-owned
    lifecycle. Multi-entry success, mount failure, cancellation before/during
    transition, request-context detachment, canceled-realm refusal, rollback
    disposal counts, ten shuffled runs, race, and the 64-entry leaf benchmark
    at approximately 11.0–11.2 microseconds with pre-mount and the retirement
    audit are green.
    Graph-routing safe-point publication and the full leak gate remain open.
  - [x] Require and execute an effect-restricted pre-mount for every changed
    reconciliation row before live teardown. Candidate factories receive exact
    immutable identity, canonical values, current read-only dependency
    bindings, narrowed permissions, and a disposer-only lifecycle—no service
    publisher or worker authority—and return a single-use activation. Missing
    support, preparation failure, and cancellation refuse without changing the
    live sequence or cycling the old composition; earlier preparations unwind
    in reverse order. Successful preparation is adopted by the new entry and
    disposed exactly once with its live lifecycle, while activation failure
    still restores the complete prior dependency closure. Focused normal,
    race, and broader plugin/server/presentation integration gates are green.
  - [x] Gate replacement activation on an exact payload-free retirement audit
    at the existing immutable-plan safe point. The runtime captures every
    affected entry lifecycle and adopted pre-mount child before teardown, then
    requires all captured scopes closed, zero remaining supervised workers,
    registered effects, child-scope links, and service publications before a
    candidate can activate. Receipt format v3 records the sorted per-entry
    proof; a second-generation replacement covers nested adopted ownership,
    while an adversarial unresponsive worker refuses activation, disposes the
    prepared candidates, restores the previous composition, and returns no
    success receipt. Graph queues,
    subscriptions, authority registrations outside this plugin lifecycle, and
    topology publication remain in the wider leak gate.
    Focused normal, ten shuffled, and race gates are green.
  - [x] Migrate every affected stateful row through its exact immutable state
    schema and explicit lifecycle contract. Snapshot-capable mounts register
    one bounded strict-JSON snapshot callback; changed rows require an explicit
    effect-restricted candidate migrator, while unchanged stateful dependents
    retain their exact canonical snapshot. Migration runs only after the
    affected predecessor closure retires and before candidate activation.
    Restoring mounts must consume transferred state before becoming active;
    snapshot, migration, cancellation, or activation failure restores the
    exact predecessor snapshots under realm ownership and returns no success
    receipt. The payload-free receipt records only the schema, before/after
    digests, and optional migrator identity. Missing lifecycle or migrator
    support, duplicate-key/non-object/over-1-MiB state, unconsumed restores,
    and legacy replacement fail closed. Direct migration, unchanged-dependent
    preservation, rollback, normal, ten shuffled, race, and full repository
    test and vet gates are green. State-schema-changing replacement remains
    part of the topology work.
- [ ] Begin with leaf config/implementation swaps, then expand to bounded topology
  changes.
  - [x] Apply leaf and bounded multi-entry implementation, config, and
    permission replacements within one immutable plan, including exact runtime
    artifact identities and dependency rebinding. Descriptor/topology-plan
    replacement remains deliberately unavailable through this API.

- [ ] **Exit gate:** supported updates have deterministic lifecycle behavior
  and leave no workers, subscriptions, queues, or authority registrations behind.

### Phase 8: remove obsolete constraints

- [ ] Delete old binding-only launch paths once the corresponding explicit
  profiles are live; no production compatibility window is required.
  - [x] Delete the historical benchmark migration/parity runner, paired-arm and
    baseline-registry paths, gateway-owned browser UI, and duplicate standalone
    presentation forks after their direct replacements landed. The legacy
    flag-only server and remaining voice/sidecar/upstream binding launches keep
    the broader deletion parent open.
- [x] Remove kernel assumptions about slow ownership/speech and mandatory audio.
  Generic ownership validation now accepts engine, model, or remote slow
  cognition and leaves realizable combinations to each adapter contract; the
  legacy architecture definition boundary no longer requires either audio
  direction. Speech authority is validated independently of fast/slow phase,
  the paired cognition seam makes silent slow an explicit opt-in, and locked
  slow-only/both-speaking Graph IR references execute direct deliberative
  speech. Adapter-specific voice profiles may still require audio or a silent
  background provider without turning that reference choice back into a shared
  invariant. Focused acceptance tests cover remote/model slow ownership,
  audio-free definition validation, the retained turn-generation contract, and
  arrangement-specific slow speech.
- [ ] Reduce the architecture catalog and status model to graph-derived facts.
  - [x] Remove legacy topology and role projection from graph-native session
    adapters. `NativeRuntime.Status` now exposes only the exact mounted Graph
    IR identity and its graph-bound adapter name/profile fingerprint; exact
    deployment, node, capability, provider, and route facts remain in
    authenticated live inspection and execution evidence. Graph-native
    benchmark reportability consequently matches that exact evidence instead
    of also requiring lossy ownership, stack, policy, tool, observer, and
    fast/slow role labels. Legacy bindings retain their status fields until
    their separate launch paths and catalog projections are deleted. Normal,
    three shuffled, race, vet, Windows/amd64 and Darwin/arm64 production
    builds, and the low-parallelism full repository gate are green.
- [x] Amend or supersede ADRs whose statements became reference-graph choices.
  ADR-0015 separates retained safety/reproducibility invariants from historical
  topology mandates, and the status blocks of ADR-0004 through ADR-0014 now
  identify whether each affected decision is superseded or merely represented
  by graph/profile contracts. Historical evidence keeps its original meaning;
  legacy launch-path deletion remains a separate unchecked implementation gate.

## 26. Testing and production gates

### 26.1 Graph and type tests

- Parse/format/reparse and source-location tests for `.ortg`.
- Golden equivalence among `.ortg`, normalized YAML/JSON, visual-editor AST,
  and SDK output.
- Lockfile determinism, deliberate-update, stale-lock, and descriptor-digest
  verification tests.
- Type-unification and inferred-arity tests for variadic connector ports.
- Positive fixtures for valid native, sidecar, and remote compositions.
- Negative fixtures for syntax, port direction, protocol, and payload type
  mismatch, with stable diagnostics.
- Optional SDK compile-negative fixtures for errors its host type system can
  catch earlier.
- Graph-build failures for implicit fan-out, multiple writers, missing ports,
  invalid cycles, incompatible transports, and authority bypass.
- Canonical encoding, fingerprint, lineage, and config-redaction tests.
- Golden diagrams generated from IR.

### 26.2 Runtime tests

- Deterministic virtual-clock tests for cadence, deadline, retry, and wake-up.
- Property and fuzz tests for channel ordering, depth, drop, `Tee`, `Mux`, and
  cancellation races.
- Race-detector and leak tests for mount/unmount and graph replacement.
- Crash, timeout, malformed output, slow consumer, missing dependency, and
  sidecar reconnect tests.
- Trace replay equivalence for current reference behavior.
- State-schema-changing topology migration compatibility and refusal tests.

### 26.3 Safety and protocol gates

- Preserve OpenAI/OpenRealtime protocol conformance at the gateway.
- Preserve prompt-injection, target-fence, confirmation, idempotency, and
  irreversibility tests.
- Prove that no proposal reaches an effect sink without the typed authority
  path.
- Stress independent fast/deliberative activations without provider-ordering
  gates, and reconstruct an exact committed prefix after a later State value
  overtakes its trigger; a sibling branch must then fail only at the explicit
  compare-and-append safe point, never by sampling unrelated context.
- Test cancellation before and after the irreversible boundary.
- Verify secrets and private payloads are absent from Graph IR, diagrams, and
  default telemetry.
- Verify a presentation or renderer plugin cannot acquire effect authority,
  widen a tool declaration, bypass confirmation, or access a session/inspection
  capability outside its declared scope.

### 26.3.1 Presentation and client gates

- Compile browser, macOS, and headless client profiles from the same immutable
  descriptor/lock semantics and attest exact runtime implementations.
- Run a language-neutral client reducer/conformance corpus in JavaScript,
  Swift, and Go.
- Run real Chromium end-to-end tests for WebSocket, WebRTC, audio, video,
  tools, interruption, artifacts, inspection, and reconnect.
- Run actual Swift tests and a native app smoke/end-to-end test on macOS; source
  inspection alone is never release evidence.
- Drive browser and macOS clients sequentially against one unchanged server and
  verify the same server Graph IR plus each exact client graph identity.
- Mount, unmount, replace, and fail client/host plugins under race/leak tests;
  assert no worker, media track, socket, listener, timer, authority grant, or
  temporary resource survives its scope.

### 26.4 Performance gates

- Measure per-edge enqueue/dequeue overhead and memory.
- Measure scheduler throughput and fairness under mixed media/control load.
- Compare end-to-first-action and deadline success against current baselines.
- Measure cancellation propagation and graph shutdown.
- Verify inspection can be sampled or disabled without changing semantics.
- Measure endpoint-to-first-played-audio, playout gaps, capture/frame drops,
  client transport queues, reconnect recovery, reducer-to-view latency, memory
  growth, and inspection/render overhead in both browser and macOS clients.
- Run complete system benchmarks using immutable graph/config/profile
  identities rather than model-family labels.

### 26.5 Benchmark and behavioral non-regression gates

Repository tests answer whether local contracts and deliberately constructed
examples behave as implemented. They do not answer whether the refactored
agent still succeeds on real conversations, meeting work, computer use, tool
calling, overlap, or latency. Project completion therefore requires complete
new-architecture benchmark evidence and a review against the trusted original
numbers in addition to all preceding test gates.

Each required comparison must follow this protocol:

1. Record the benchmark owner's accepted original baseline number and available
   per-case trail before observing the candidate. Historical per-attempt
   reconstruction is not required. Pin the complete fixture/scorer and
   deployment configuration for the new run so its result is reproducible and
   cannot be silently relabeled.
2. Run the candidate through the graph-native launch path and exact new values,
   deployment, and policy artifacts. Every completed task must attest the
   immutable Graph IR fingerprint and live element/config/capability identities;
   a candidate that silently falls back to an obsolete binding is invalid.
3. Report the new aggregate pass rate, every per-task/per-condition result,
   safety and authority failures, deadline success, interaction errors, and
   relevant latency distributions including median and tail behavior. Place
   those results beside every historical aggregate or per-case number that
   actually survived. Do not fabricate matched historical attempts,
   transitions, confidence intervals, or media. An aggregate can never erase a
   severe scenario or safety regression.
4. Require the complete predeclared candidate population and, for statistically
   comparable gates, at least the accepted historical quality target. A
   merely non-significant difference is not proof of parity, and remaining
   above 80% is not acceptable when the trusted result was materially higher.
   The current operator-scoped scenario checkpoint uses one retained attempt
   per case and reports it descriptively, without a statistical parity claim;
   it is not automatically repeated after code changes. Other suites declare
   their own complete candidate population before the run.
5. Treat any material correctness, interaction, safety, deadline, or latency
   regression as an implementation blocker. Preserve the failed artifact, use
   graph diff plus runtime/trace evidence to locate the changed path, fix it,
   rerun only the smallest affected diagnostic slice when explicitly requested.
   Do not start another complete eleven-scenario run automatically.

The required final matrix is one retained attempt for each of the eleven
interaction scenarios, all four Meeting Assistant v1 tasks, all sixteen
Realtime-CU v1 cases, all 498 FDB v1.5 tasks, all 100 FDB v3 examples, all
6,147 FD-Bench conversations in all 21 conditions, and all 278 tau2-bench/
tau-Voice tasks in both control and regular conditions. DynaCU-Bench remains an
independent optional validation. No phase exit gate or project-level completion
claim may substitute a test suite, protocol-conformance run, or synthetic
integration test for this matrix.

## 27. Compatibility and versioning

- The external Realtime protocol remains backward compatible.
- `api/v1` remains stable; graph APIs begin under a new version or package.
- Graph IR, element descriptors, state schemas, and sidecar graph protocol are
  independently versioned.
- `.ortg` uses symbolic names without inline version suffixes. A generated,
  reviewable lockfile pins exact descriptor and subgraph digests; Graph IR
  records those resolved identities.
- An element revision is immutable. Compatible configuration additions do not
  change port identity; semantic port or reaction changes do.
- Architecture references continue to be exact and immutable, but their
  fingerprint is derived from Graph IR plus non-secret config/deployment
  evidence selected by the catalog.
- No legacy flag, named binding, migration shim, or historical-result loader is
  part of the new runtime or benchmark design. Obsolete switches are deleted as
  direct profiles take over.
- Trusted historical numbers remain write-up-only quality targets. Only new
  candidate attempts require and produce retained per-attempt artifacts; old
  graph artifacts are never runtime inputs and are not reconstructed.

## 28. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Too many tiny elements make graphs unreadable or slow. | Use measured granularity, typed subgraphs, collapse in the UI, and avoid splitting code that has no useful replacement boundary. |
| A project-owned graph language creates tooling burden. | Keep `.ortg` to netlist constructs, forbid embedded configuration and general evaluation, and ship parsing, canonical formatting, source maps, completion, and golden diagnostics as part of the first release. |
| Several authoring forms develop different semantics. | Lower `.ortg`, normalized YAML/JSON, the visual editor, and SDKs into one frontend-neutral AST and run one elaborator; maintain cross-frontend golden-equivalence tests. |
| YAML interchange has surprising syntax. | Use a strict YAML 1.2 subset, reject ambiguous features, generate JSON Schema, and offer equivalent JSON. |
| Declarative graph source is mistaken for weak or duck typing. | Make the language-neutral elaborator the normative pre-mount type checker and publish precise port-level diagnostics. |
| Code builders and declarative sources diverge. | Treat builders as pure frontends to the same descriptors, elaborator, and Graph IR; deployment signs the emitted artifact rather than source code. |
| Graph validation becomes another policy engine. | Keep core errors mechanical and put opinionated completeness checks in named lint profiles. |
| Dynamic rewiring corrupts state or duplicates effects. | Defer it until static graphs are mature; require safe points, explicit migration, scoped disposers, and refusal when uncertain. |
| Queue settings hide large latency. | Show depth and estimated media time on every edge; trace queue wait separately. |
| A generic mux corrupts segmented output. | Restrict generic mux to atomic events and provide stream-aware arbiters with explicit boundaries. |
| Capability descriptors drift from providers. | Resolve at startup, record live concrete types/capabilities, and test handshakes. |
| Visual editing destroys hand-authored structure. | Use the canonical `.ortg` formatter or normalized graph model, keep non-semantic layout separate, preserve stable IDs, and never try to rewrite arbitrary builder source. |
| Cordis terminology is copied without its guarantee. | Track dependencies and disposers concretely, test unload/reconciliation, and never call physical actions reversible. |

## 29. Settled decisions and open questions

### 29.1 Settled by this proposal

- The target is a general multimodal real-time agent runtime.
- A small ClickNP/Verilog-inspired `.ortg` netlist is the default
  hand-authored and documented graph format.
- Strict normalized YAML/JSON remains a supported interchange and GitOps/API
  representation.
- Human graph source contains no port types, connector arities, channel-profile
  names, or inline element versions.
- Element values, channel-depth overrides, deployment bindings, secrets,
  resolution locks, and evidence are separate artifacts; topology retains
  only symbolic instances, edges, boundaries, and `->`/`=>` delivery.
- The language-neutral graph elaborator is the normative type checker.
- Go and future TypeScript builders are optional pure frontends, not the
  source of runtime soundness.
- Graph IR is the canonical runtime and inspection representation.
- Programmatic wiring uses explicit `Connect`; raw assignment is not graph
  wiring.
- Port types include temporal protocol, not only payload modality.
- Trigger and interrupt paths are explicit.
- `Tee` and merge/arbitration elements are explicit.
- `Tee` outputs and homogeneous `Mux`/arbiter inputs are variadic; the
  elaborator infers their lane counts and generic types from connections.
- `->` means lossless/backpressured and `=>` means lossy/drop-on-full.
- Every channel has a bounded default depth; rare overrides are separate
  edge-keyed configuration and the effective depth is frozen into Graph IR.
- Latency and cost are empirical profiles, not static type promises.
- Fast/slow and native/external roles are graph choices.
- A deliberative model may speak or act if the graph routes it through the
  required policy and authority boundaries.
- Mermaid is generated, not canonical.
- Dynamic lifecycle follows declared dependencies and scoped reversible
  runtime effects.

### 29.2 Remaining implementation questions

Canonical Graph IR encoding and type identities, the `.ortg` grammar, strict
normalized YAML/JSON schema, edge-keyed depth overrides, descriptor media
profiles, the Go builder, bounded reaction-task scheduler, and the current
element/subgraph boundaries are implemented decisions rather than open
questions. The remaining choices are:

- Whether TypeScript is the first supported external builder SDK and how its
  generated port handles are versioned.
- How much topology editing the first visual UI supports versus read-only
  inspection.
- Whether visual layout is a sidecar artifact or a non-semantic graph
  annotation.
- Which topology and state-schema changes, and which routing safe points,
  extend the implemented immutable-plan
  configuration/implementation/permission/state swaps.
- The dependency-ordered deletion sequence for old flags and binding packages.

These remaining questions affect implementation shape, not the architectural
separation between typed authoring, Graph IR, execution, configuration, and
evidence.

## 30. Definition of done

The refactoring is complete when all of the following are checked. None is
checked from foundation work alone; each requires end-to-end release evidence.

- [ ] A developer can construct and type-check a graph from arbitrary native,
   sidecar, and remote elements through `.ortg`, normalized YAML/JSON, a
   visual editor, or an optional SDK without adding a binding species or
   rebuilding the runtime.
  - [x] `.ortg`, strict normalized YAML/JSON, and the optional Go builder lower
    through the same compiler and produce fingerprint-equivalent Graph IR;
    native and descriptor-attested protocol-v4 sidecar elements mount through
    the common factory registry. The browser workspace also applies exact
    compiler-produced canonical formatter edits and compiler-backed,
    fingerprint-CAS canvas node renames plus exact edge removal and
    compiler-validated, predecessor-fingerprint-bound edge creation across
    canonical `.ortg` and normalized YAML/JSON without file authority. Complete
    arbitrary-remote release evidence remains a requirement of the parent gate.
- [x] The graph can expose any supported combination of text, audio, video,
  images, files, tools, computer use, and control ports; audio is optional. The
  common descriptor and typed-port model admits those capabilities
  independently. Executed text/file cognition and silent computer-use graphs
  prove audio-free operation, while the adaptive-video, Meeting Assistant, and
  reviewed Realtime-CU paths exercise streaming visual, audio, tool, action,
  and control compositions through the same runtime.
- [x] Trigger, interrupt, timeout, failure, arbitration, authority, and terminal
  paths are statically inspectable. The exact static model and generated views
  expose reaction triggers, interrupts, success/cancellation/refusal/timeout/
  failure outcome ports, explicit arbiter topology, and effect authority and
  reversibility. Validation diagnoses missing terminal, interrupt, and typed
  authority paths, while the fingerprint-bound projection passes the real
  Chromium developer-profile gate.
- [x] Slow/deliberative and fast/foreground roles can be rewired, forked, merged,
  or removed without kernel changes. Locked fast-only, slow-only, and
  both-speaking graphs execute complete turns through the same graph runtime;
  direct deliberative speech plus explicit `Tee`/`Mux` topology supplies the
  fork/merge cases without a kernel-owned slow-to-fast handoff.
- [x] The same full-duplex foreground can be run with native or external
   interaction as a graph change.
  - [x] Locked native- and external-interaction references select the same
    `model.External` element through topology and capability changes, and the
    omni, duplex-native, and upstream-native forms mount and negotiate exact
    protocol-v4 sessions. The executed cross-mode regression proves the common
    typed audio/output behavior and the equivalent `answer` selection through
    the external input versus native output control ports. Live provider quality
    and performance remain benchmark gates rather than being inferred from this
    architecture proof.
- [x] Channel depth, loss, queue occupancy, and latency contribution are
  visible. The joined operator channel cards bind immutable delivery/depth to
  live occupancy/high-water, drop/backpressure, and queue-wait evidence; the
  exact projection passes fail-closed Go/JavaScript validation and a real
  Chromium rendering gate.
- [ ] Every running session reports an immutable Graph IR fingerprint and exact
  live-resolved element/config/capability identities.
  - [x] Production graph-native mounts pass exact live/static joins, runtime
    identity and capability handshakes, and reviewed benchmark attestation;
    mismatched or incomplete evidence cannot cross the public management
    boundary. An incomplete legacy runtime intentionally receives no inspection
    capability, so removing or upgrading the remaining legacy launch paths is
    still required by the universal parent gate.
- [x] Mermaid/DOT and the live canvas are generated from that exact graph.
  Mermaid and DOT derive through the same immutable inspection model and carry
  its fingerprint; the session model endpoint rejects static/live identity or
  population drift, and real Chromium proves the compiled canvas remains bound
  to that fingerprint.
- [x] The action authority, target, confirmation, audit, and irreversibility
  guarantees remain intact or become stronger through typing. The locked
  silent computer-use graph requires proposal admission, confirmation, target
  fencing, canonical commit, ledger commit, and dispatch through distinct typed
  values; bypasses fail compilation or adversarial refusal gates. Its stable
  endpoint two-effect regression and reviewed real-Chromium Realtime-CU
  candidate exercise the complete feedback path with exact live graph evidence.
- [ ] Static reference graphs match current protocol behavior, and new
    non-audio and alternative speech compositions are covered by release tests.
  - [x] Locked audio-free text/file cognition and silent computer-use agents
    have executed regressions, and the fast-only, slow-only, and both-speaking
    graphs execute complete turns with retained artifacts. The reviewed
    Realtime-CU campaign supplies live release evidence for the silent action
    composition; reference safe-point/protocol parity and the remaining live
    benchmark evidence keep the parent gate open.
- [ ] Every required benchmark has complete, clean, exactly attested
    pre-refactor and graph-native candidate artifacts; the candidate satisfies
    the predeclared aggregate and per-case non-regression gates, and every
    discovered regression has been fixed and followed by a complete-suite
    rerun.
  - [x] Complete, independently reopened graph-native candidate populations now
    exist for Meeting Assistant (4/4), Realtime-CU (16/16), and FDB v3
    (100/100), with exact live graph evidence, retained media, deterministic
    outcomes, exact-model advisory review, and create-only source/evaluation
    receipts. The one-pass scenario 11×1, FD-Bench, both tau-Voice conditions,
    and the resulting full non-regression cycle keep the universal parent gate
    open.
- [ ] Supported graph changes mount and unmount without leaked resources or
    silently lost committed work.
  - [x] Leaf and bounded multi-entry implementation, configuration, and
    permission replacements within one immutable plan quiesce their exact
    dependency closure, return deterministic receipts, and either commit under
    realm ownership or dispose the candidate and restore the previous
    composition. Effect-restricted candidate pre-mount now fails before live
    teardown and transfers its cleanup into the activated entry. Activation is
    also gated on an exact audit that all retired plugin lifecycles, workers,
    effects, child scopes, and service publications reached zero. Exact state
    migration within one immutable descriptor and state schema is explicit and
    rollback-safe. State-schema-changing topology publication, graph-routing
    safe points, and the cross-system leak gate remain requirements of the
    parent.
- [x] Operators can trace an externally visible action back through its policy,
    trigger, observation, state revision, model run, queues, and authority
    decision.
  - [x] The joined browser operator view already binds the immutable static
    graph to redacted live nodes and channels, renders trigger-relative node
    timing and exact queue depth/counters/wait, and can export and replay a
    fingerprinted payload-free trace. The joined live view now renders bounded
    pseudonymous cross-node edge sequences, including repeated feedback-loop
    stages and per-stage timing, plus the latest closed categorical authority
    decision and irreversible-boundary status. Every retained stage now also
    carries its bounded pseudonymous direct-parent assertion through redaction,
    recording, and replay. Standard graph-native stages now also retain a
    closed observation/state-revision/policy/model-run classification through
    the same privacy, consistency, trace, and browser boundaries, completing
    the semantic join without retaining payload, provider, request, or model
    content.
- [ ] The browser presentation host/client and macOS app are composable plugin
    profiles over the same clean realtime and observability APIs; the gateway
    contains no privileged UI, and real cross-client end-to-end tests pass.
  - [x] The standalone browser host/client is manifest-composed over the clean
    public APIs, the gateway-owned UI and duplicate standalone forks are
    deleted, and real Chromium plus the unchanged-server macOS-profile probe
    pass. Native manifests and shared Swift services exist; cross-realm
    host/server shutdown now has zero-ownership evidence. The signed Darwin
    application run, remaining topology-changing and capability-specific
    host/client replacement, and the native lifecycle matrix keep the parent
    open.
- [ ] Every server, host, and client presentation capability can be replaced or
    removed through a descriptor-locked plugin composition with scoped cleanup,
    explicit permissions, and no implicit effect authority.
  - [x] Descriptor-locked minimal, observer, developer-WebSocket, and
    developer-WebRTC browser profiles independently compose or omit media,
    effects, artifacts, inspection, trace, and authoring providers under exact
    permission ceilings and scoped disposal. Complete server-realm assembly and
    its replacement/rollback are covered, and the unchanged-server cross-client
    gate proves zero-ownership host/server shutdown. A host-route replacement,
    rollback, and removal preserves the stable host router and export; real
    Chromium client-provider removal/recovery proves exact dependent cleanup
    and a zero-ownership close, while the portable native composition proves
    exact provider-loss/remount and idempotent reverse-order shutdown.
    Authenticated browser implementations can also swap one or multiple
    descriptor-declared content-addressed assets with exact dependency cleanup,
    rollback, payload-free identity evidence, and explicit state migration.
    The shipped effects and artifact-reference providers additionally pass an
    atomic real-Chromium swap, signed-catalog renegotiation, real artifact
    execution, and zero-active-socket final cleanup.
    Host route capabilities likewise have atomic multi-row and stateful
    migration/rollback evidence over the stable router and export, and the
    shipped WebSocket relay now passes real active-session replacement with an
    exact zero-ownership retirement audit. Complete topology-changing, WebRTC/
    management-relay, and remaining capability-specific host/client replacement
    plus signed-native lifecycle/leak evidence remain open.

## References

- Eddie Kohler et al., [The Click Modular Router](https://pdos.csail.mit.edu/papers/click:tocs00/paper.pdf).
- Bojie Li et al., [ClickNP: Highly Flexible and High Performance Network Processing with Reconfigurable Hardware](https://www.microsoft.com/en-us/research/publication/clicknp-highly-flexible-high-performance-network-processing-reconfigurable-hardware/), ACM SIGCOMM 2016.
- [OpenClickNP language reference](https://github.com/bojieli/OpenClickNP/blob/main/docs/language.md).
- [n8n workflow connections](https://docs.n8n.io/workflows/components/connections/), [splitting](https://docs.n8n.io/flow-logic/splitting/), [merging](https://docs.n8n.io/flow-logic/merging/), and [sub-workflows](https://docs.n8n.io/flow-logic/subworkflows/).
- [DeepSeek Harness architecture](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/architecture.md) and [web client architecture](https://github.com/deepseek-ai/deepseek-harness/blob/master/.agents/notes/implemented/architecture/2026-07-19-gui-web-client-architecture.md).
- Yifan Shi, Wei Zhang, and Tianyi Cui, [A Programming Paradigm for Spatiotemporal Composability](https://arxiv.org/abs/2608.25512).
- [YAML 1.2.2 specification](https://yaml.org/spec/1.2.2/).
- [JSON Schema 2020-12 specification](https://json-schema.org/draft/2020-12).
