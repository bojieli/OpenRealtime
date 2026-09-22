# Transport backlog diagnosis

This smoke attempt was stopped after reproducing the WebSocket keepalive failure.
It is not a completed campaign. The per-tick traces show real-time PCM delivery
continuing while the delivery buffer reaches its 30-second cap. The first
session emitted 439 words across its initial answer and interrupted follow-up;
the initial interruption correctly cleared queued audio. The model later
entered thinking while synthesis still had a substantial backlog.

The synthesis callback waits for delivery capacity inside the WebSocket reader.
A long response therefore fills both the application audio buffer and the
WebSocket receive queue; control frames behind that backlog cannot be processed
promptly. The service logged `keepalive ping timeout`. This is transport
backpressure, not evidence that model inference stopped.

The next implementation adds negotiated byte credits to the synthesis transport.
It limits audio in flight while the server continues reading cancel and credit
messages. This does not bound the synthesis backend's internal generation queue,
and does not establish rendered playback or full end-to-end acceptance.
