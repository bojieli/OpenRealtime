# Audio-free text/file cognition

This component commits participant text, images, files, and retained attachments
to canonical conversation history before model activation. It resolves retained
bytes through the explicit media lease path and commits sanitized model output.
It has no audio dependency or complete tool dispatch/result path.

**Status:** executable component reference. The production Realtime gateway
adapter and file-upload operation remain unfinished. For ordinary text/image
sessions, use the current [client API](../../../docs/openai-realtime-compatibility.md).

## Configure and invoke

Install settings through `invocation_update` before requesting a response or
submitting content with automatic activation enabled:

```json
{
  "revision": 1,
  "invocation": {
    "instruction": "Answer from the committed conversation and attached files.",
    "max_output_tokens": 1024
  }
}
```

Wait for the correlated `updated` event on `activation_outcome`. Revisions must
increase. Each subsequent model invocation receives a snapshot of the accepted
settings; an update cannot rewrite an invocation already emitted.

The supplied values enable `activation.generate_on_commit`. Set it to `false`
for explicit response creation. Content still commits, and the policy reports
`explicit_response_required` without invoking the model. Send `response_create`
with a unique response ID and the exact `CommittedContext` from a canonical
snapshot or successful observation commit. Its expected version and State item
ID must agree with that context. Await the correlated `emitted` outcome. The
model waits for the matching canonical prefix even when snapshot and activation
lanes are scheduled independently. Manual responses have no observation-derived
tool authority or trusted semantic purpose.

## Cancellation

`activation_cancel` withdraws future matching activation while its bounded
cancellation memory retains the address. `model_cancel` interrupts an already
emitted model run. `content_cancel` withdraws pending participant content.
Canceling one of these stages does not implicitly cancel the others.

## Read and commit output

Consumers must concatenate text carried by both delta and end events on
`prepared_text`: the serialization filter can release a final buffered suffix
on the end event. A `model_result` is prepared output; await `model_commit_outcome`
to determine whether it entered canonical history.

This is an executable component reference. A production adapter for the shared
Realtime gateway remains unfinished, including multipart translation and
response presentation. The shared session API currently accepts text and images;
it does not yet expose this graph's file-upload operation.
