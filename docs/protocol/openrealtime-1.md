# The OpenRealtime Protocol, version 1

**Status:** proposed. Implemented by OpenRealtime v1.0.
**Wire namespace:** `openrealtime.`
**Base protocol:** the OpenAI Realtime API (GA), unmodified.

This document is normative. It specifies a strict, backward-compatible superset
of the OpenAI Realtime API that adds realtime video input, observation
reporting, and computer use. The total addition is three events and two object
extensions; no existing event changes shape and no existing field changes
meaning.

The protocol has no abbreviation. Written in full on first use, "the protocol"
thereafter.

The key words MUST, MUST NOT, REQUIRED, SHOULD, SHOULD NOT, and MAY are to be
interpreted as described in RFC 2119.

## 1. Design principles

These five constrain everything below, and an addition that violates one of
them does not belong in this protocol.

1. **Strict superset.** Every valid Realtime GA session is a valid OpenRealtime
   session. A voice-only client written against the base API MUST work against
   an OpenRealtime server with no changes and with no awareness that the
   extension exists.
2. **Namespaced.** New events are prefixed `openrealtime.`; new fields live
   under an `openrealtime` key inside existing objects.
3. **Negotiated, never assumed.** Extensions activate only after the client
   declares support and the server confirms. Absent negotiation, behaviour is
   exactly the base protocol. Backward compatibility is therefore a property of
   the negotiation, not of client discipline.
4. **Minimal.** Two client-to-server events, one server-to-client event, two
   extended objects, zero changed base events.
5. **Actions are not new protocol.** Computer use rides existing function
   calling.

## 2. Negotiation

A client declares support inside `session.update`:

```jsonc
{ "type": "session.update", "session": {
    "openrealtime": {
      "version": 1,
      "supports": ["video.input", "observations", "computer_use"],
      "observers": ["audio", "video"] }}}
```

A server that implements this protocol confirms inside `session.updated`:

```jsonc
{ "type": "session.updated", "session": {
    "openrealtime": {
      "version": 1,
      "enabled": ["video.input", "observations"],
      "observers": ["audio", "video"],
      "available_observers": ["audio", "video"],
      "video": { "format": "jpeg", "fps_cap": 3, "max_dimension": 1280 } }}}
```

### 2.1 Rules

- The version lives inside the `openrealtime` key and carries the number and
  nothing else. Repeating the name inside a field already named for it would be
  noise.
- A server that does not implement this protocol MUST ignore the unknown key
  and MUST NOT echo it. The client then sees no `enabled` list and remains
  voice-only.
- A client that sends no `openrealtime` key gets an ordinary session. A server
  MUST NOT volunteer the key to a client that never mentioned it.
- A server MUST reject a `version` it does not implement, with an `error` event.
  It MUST NOT silently negotiate a different version.
- A server MUST reject an unrecognised capability name with an `error` event.
  A name it does not recognise may mean something in a version it does not
  speak, and guessing is worse than refusing.
- A capability the server cannot provide MUST be absent from `enabled`. This is
  not an error: the session works, and the client can see what it did not get.
  A server SHOULD determine this from what it can actually do for this session
  rather than from a static list.
- Capabilities MAY be re-negotiated by a later `session.update`. The most
  recent `session.updated` is authoritative.

### 2.2 Capabilities

| Name | Enables |
| --- | --- |
| `video.input` | §3, the two video events |
| `observations` | §4, the observation event |
| `computer_use` | §5, the `computer.*` tool namespace. Adds no events; negotiated so a client can discover whether the server honours the namespace before declaring the tools. |

### 2.3 Observers

Perception is selected per session, by observer name, inside the same object.
It is a field rather than a fourth event, so the wire surface is unchanged.

| Field | Direction | Meaning |
| --- | --- | --- |
| `observers` | client → server | the observer set this session wants |
| `observers` | server → client | the observer set this session will actually run |
| `available_observers` | server → client | every observer this deployment could select from |

- A client that sends no `observers` gets the server's default set, and the
  server MUST state what that turned out to be. A client cannot choose a set it
  cannot see, so a server SHOULD send `available_observers` whenever it has
  more than one.
- An observer the server does not have MUST be dropped from the answer rather
  than failing the session, exactly as an unsupported capability is.
- A selection naming *nothing* the server has MUST be rejected with an `error`
  event. Silently substituting the default would give the client a session
  running perception it did not ask for and did not know about.
- Observer names are a server's own vocabulary. `audio` and `video` are the
  conventional names for speech recognition and screen narration; nothing here
  reserves them.

### 2.4 Video limits

When `video.input` is enabled the server MUST state its limits, so a client can
conform rather than discover them by being rejected.

| Field | Meaning |
| --- | --- |
| `format` | encoded image format the server accepts: `jpeg`, `png`, or `webp` |
| `fps_cap` | frames per second per source above which frames MAY be discarded. A server that states a cap SHOULD enforce it: a limit a client is told about and the server does not apply is not a limit. |
| `max_dimension` | maximum pixels on either edge of a declared source |
| `max_frame_bytes` | maximum decoded size of one frame, optional |

## 3. Video input

Two client-to-server events.

### 3.1 `openrealtime.input_video_source.update`

Declares or updates a source. REQUIRED before any frame from that source.

```jsonc
{ "type": "openrealtime.input_video_source.update",
  "source": "screen",              // "screen" | "camera" | opaque id
  "state": "active",               // "active" | "paused" | "closed"
  "width": 1920, "height": 1080 }
```

- `source` is REQUIRED. Screen and camera are simultaneously live and
  semantically different — you act on the screen, you observe the camera — so a
  source is never implied.
- `width` and `height` are REQUIRED when `state` is `active` or `paused`. This
  event exists mainly for geometry: a click at `(x, y)` is meaningless without
  the coordinate space the model saw, and declaring the space once and on every
  change is what makes computer-use coordinates well-defined rather than a
  convention.
- A source MUST be re-declared whenever its geometry changes.
- `closed` releases the source. A later frame from it is an error until it is
  declared again.
- A server MUST reject a frame from an undeclared source. It MUST silently
  ignore frames from a `paused` source.

### 3.2 `openrealtime.input_video_frame.append`

Carries one frame.

```jsonc
{ "type": "openrealtime.input_video_frame.append",
  "source": "screen",
  "frame": "<base64 jpeg>",
  "timestamp_ms": 1724236800123 }
```

- `frame` is base64 of the encoded image, in the negotiated `format`.
- `timestamp_ms` is capture time, OPTIONAL. It is source time and MAY precede
  the time the server commits anything derived from it.
- A server MUST reject a frame exceeding `max_frame_bytes`.

### 3.3 The gate runs server-side, always

The client sends video; it makes no decisions about which frames matter.

This is not a performance trade. Selective perception is what this protocol is
for, and a client that must implement pixel-change gating at a specific
threshold is a client nobody writes. Push the gate into the client and the
server no longer has it: every client reimplements it, differently or not at
all, and the observation a session produces stops being a property of the
server. A dumb client — send frames, receive observations — is what makes the
protocol adoptable.

Bandwidth is a transport problem, not a client-intelligence problem. Base64 at
a capped rate is adequate at the frame rates that matter, and a transport that
carries video on a media track removes exactly the redundancy a client-side
gate would have removed, because inter-frame prediction already does it.

### 3.4 One entrance

This is how video enters, for every client. A transport adapter that terminates
a WebRTC track decodes it and emits these events on the client's behalf; it
does not bypass them. One entrance means one thing to specify and one thing to
test.

## 4. Observations

One server-to-client event.

```jsonc
{ "type": "openrealtime.observation.added",
  "event_id": "event_...",
  "observation_id": "obs_...",
  "observer": "video",             // "video" | "audio" | opaque id
  "source": "screen",
  "text": "A confirmation dialog appeared: 'Confirm payment of $40.'",
  "authority": "observer",         // "observer" | "user"
  "item_id": "item_...",
  "timestamp_ms": 1724236800456 }
```

Observations are already committed to the session's own record; this event
exists so a client can display and audit what the agent perceived. It is
OPTIONAL and purely outbound. A client MUST NOT send it.

`authority` states what the text is permitted to do. Content an observer
extracted from the world carries `observer` authority: it is data, and it can
never be promoted to an instruction (§6). Speech attributed to the human
participant carries `user` authority. A client that displays observations
SHOULD make the distinction visible, because a user looking at narrated screen
text is looking at something the agent read, not something anyone said.

## 5. Computer use

Zero new events.

Actions are ordinary function calls: `response.function_call_arguments.done`
already exists, and the client already returns `conversation.item.create` with
a `function_call_output`.

The result of an action is the next screen, and the screen already arrives
through the video stream. So a tool output stays text — `"clicked"` — and the
visual consequence flows back through the perception path exactly as it does
for a human. No image is ever carried in a function result, which is what keeps
the addition small.

### 5.1 The `computer.*` tool namespace

A standard vocabulary, specified so that client and server agree on semantics
without negotiating them per deployment. It depends on nothing in this document
except function calling, so any Realtime implementation can adopt it without
adopting anything else here.

| Tool | Parameters |
| --- | --- |
| `computer.click` | `source`, `x`, `y`, `button` (`left`\|`right`\|`middle`, default `left`) |
| `computer.click_element` | `source`, `element_id` (the visible label in a set-of-mark frame) |
| `computer.double_click` | `source`, `x`, `y` |
| `computer.move` | `source`, `x`, `y` |
| `computer.drag` | `source`, `from_x`, `from_y`, `to_x`, `to_y` |
| `computer.type` | `source`, `text` |
| `computer.key` | `source`, `keys` (array of key names, pressed together) |
| `computer.scroll` | `source`, `x`, `y`, `delta_x`, `delta_y` |
| `computer.screenshot` | `source` |
| `computer.wait` | `duration_ms` |

Every tool takes `source`, naming a declared video source (§3.1), so an action
targets a coordinate space the model actually saw. Coordinates are in that
source's declared `width` × `height`, origin at the top left.

`computer.click_element` is the browser-grounded alternative to
`computer.click`. It is valid only when the current frame visibly labels
interactive elements. The label resolves inside the declared browser target;
it is not a selector, accessibility query, or ambient DOM-reading capability.
Desktop, VM, and Android targets use pixel grounding and SHOULD omit this tool.

The complete JSON Schemas are published with this specification and are
available from an implementation as machine-readable definitions.

### 5.2 Tool definition extension

One additive field on any tool definition:

```jsonc
{ "type": "function", "name": "computer.click",
  "parameters": { "...": "..." },
  "openrealtime": { "confirm": "never", "target": "browser-1" } }
```

| Field | Values | Meaning |
| --- | --- | --- |
| `confirm` | `never` \| `policy` \| `always` | whether dispatch requires explicit authorization |
| `target` | string | the declared context this tool acts on |

There is no reversibility class. Every output — speech, text, tool call, click —
is irreversible once emitted, so the runtime does not grade them; it applies one
commit boundary to all of them. `confirm` is a developer's declaration about
consequence, not an inference the runtime makes, and it applies to any tool
rather than only to `computer.*`.

Servers that do not understand the key MUST ignore it, as JSON Schema requires.
A server that does understand it MUST NOT dispatch an `always` action without
authorization, and MUST treat `policy` as `always` when no policy is configured.
The same rule applies when the tool implementation belongs to the client: a
server MUST apply the declared confirmation before emitting the executable call
over the protocol. Client ownership is an implementation boundary, not an
authority or confirmation bypass.

## 6. Authority and injection

An agent that narrates screen text into its own context is an obvious injection
vector. The defence is provenance, and it MUST be enforced rather than
documented:

- Observations carry `observer` provenance, never `user`.
- On-screen text is data. It MUST NOT be promoted to instruction authority, and
  a tool call MUST NOT be justified solely by observer-authority content when
  that tool declares a confirmation requirement.
- Computer-use tools target a declared source bound to a declared context — a
  browser context or a virtual display — never an ambient desktop by default.
- Every executed action is recorded with its causal parents, so any click
  traces back to the observation and continuation that produced it.

## 7. Compatibility

Four cases, all of which MUST work:

| Client | Server | Result |
| --- | --- | --- |
| base | base | ordinary Realtime session |
| base | extended | ordinary Realtime session; the server never volunteers the extension |
| extended | base | the key is ignored and not echoed; the client sees no `enabled` list and stays voice-only |
| extended | extended | negotiated capabilities, everything else unchanged |

An implementation SHOULD verify all four. A compatibility claim that is not
continuously checked is a compatibility claim that decays.

## 8. Total surface

| Addition | Count |
| --- | --- |
| New client → server events | 2 |
| New server → client events | 1 |
| Extended objects | 2 (`session`, tool definition) |
| Changed existing events | 0 |
| New transports | 0 |

Three events for vision and computer use, on a base protocol with 66 wire
names, is the bar this design was written to.

## 9. Versioning

Version 1 is frozen at the OpenRealtime v1.0 release. Subsequent versions will
be additive under a higher `version` number; a server MAY implement several and
negotiates the highest the client also declares. Removing or changing the
meaning of anything in this document requires a new version number, not an
amendment to this one.
