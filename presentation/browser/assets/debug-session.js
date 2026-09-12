// Requests the narrow, read-only inspection capability for a developer
// profile, and the turn timeline the room draws. This is deliberately
// separate from the reducer and the view: a minimal client can omit it, and
// removing it revokes no unrelated service.
//
// The timeline category is asked for on its own rather than the graph
// category it is projected from: the graph stream is every element's
// bookkeeping, tens of events a second, and the reducer logs each inbound
// event into a bounded protocol log. Payloads are asked for because a
// timeline without the words - what was heard, what the policy was shown,
// what the model said - cannot answer why the agent spoke where it did.
export default {
  name: "openrealtime.presentation.client.debug-session",
  revision: 1,
  async mount(context) {
    const configuration = context.services.get("presentation.client.session_configuration");
    if (!configuration) throw new Error("session configuration service is unavailable");
    const remove = configuration.contribute({
      debug: { enabled: true, categories: ["session", "timeline"], include_payloads: true },
    });
    context.lifecycle.defer("debug-session", remove);
  },
};
