# Core concepts

OpenRealtime separates model selection from conversation timing and action
execution. This page introduces the terms used in the architecture and API
references. Start with the [quickstart](quickstart.md) to run a session.

## Follow one conversation

1. **Perception receives input.** Speech recognition turns audio into
   transcript revisions. A visual observer can turn selected frames into
   observations. A revision can change as more input arrives.
2. **The session records an observation.** Accepted input enters the shared
   history, called the trajectory. Later work identifies the version it used.
3. **Interaction policy decides when to act.** A pause may end a turn, or the
   user may have explicitly asked the assistant to keep waiting. Timing and
   conversation context determine which action is appropriate.
4. **Cognition produces a response or tool request.** In the default voice
   arrangement, a foreground model speaks and a background model reasons and
   uses tools. Other graphs can connect these roles differently.
5. **The action boundary checks and emits output.** Speech is paced for
   playback. Tool execution checks authority, confirmation, and its target.
6. **New input can interrupt the work.** Canceled work must stop publishing
   results. Playback history records what the listener heard so a later answer
   can continue from the right place.

A user turn can produce several protocol responses: an initial answer, a tool
call, and a follow-up after the result. `response.done` closes one response;
it does not necessarily mean all work for the user is finished.

## Terms used in the references

| Term | Meaning |
| --- | --- |
| ASR | Automatic speech recognition: audio to text. |
| TTS | Text-to-speech synthesis: text to audio. |
| VAD | Voice activity detection: whether someone is audibly speaking. |
| Endpoint | In turn-taking, the detected end of an utterance. In configuration, a service URL. |
| Floor | Who controls the current speaking turn and its boundaries. |
| Barge-in | User speech that interrupts the assistant's output. |
| Backchannel | A short acknowledgement such as “mhm” that may allow the current speaker to continue. |
| Trajectory | The session's append-only history of observations, generated content, actions, and results. |
| Continuation | One provider's generation from a particular conversation context. |
| Rollout | The policy that schedules cognition roles and follow-up work. |
| Preparation | Speculative work started before an observation is final; it can be adopted or discarded. |
| Commit | Accepting an item or effect at the runtime's controlled boundary. Generated output is not automatically committed. |
| Safe point | A boundary where work checks whether its context is still valid before publishing or acting. |
| Binding | An adapter that connects a voice stack and declares which subsystem owns each role. |
| Capability | Something the stack can provide, such as concurrent input/output or transcription. |
| Ownership | Which available component is selected to control a role in this session. |
| Graph | An explicit composition of processing elements connected by typed ports. |
| Graph IR | The compiled intermediate representation of a graph, used for validation and execution. |
| Descriptor | A machine-readable declaration of a component's ports, configuration, and dependencies. |
| Launch profile | Configuration selecting an application and its prepared runtime composition. |
| Sidecar | A separate model process connected through a versioned protocol. |
| Attestation | A checked record tying a running configuration or result to exact identities and artifacts. |

## Three different version boundaries

The binary version, Go component API version, and wire protocol versions are
independent. `api/v1` identifies the stable Go component contract. The
OpenRealtime protocol version selects client-visible extensions. Sidecar
versions select the contract with an external model process. A change to one
does not automatically change the others.

Continue with [Architecture](architecture.md), [model selection](providers.md),
or the [sidecar integration guide](sidecars.md).
