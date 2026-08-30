// Requests the narrow, read-only inspection capability for a developer
// profile. This is deliberately separate from the reducer and the view: a
// minimal client can omit it, and removing it revokes no unrelated service.
export default {
  name: "openrealtime.presentation.client.debug-session",
  revision: 1,
  async mount(context) {
    const configuration = context.services.get("presentation.client.session_configuration");
    if (!configuration) throw new Error("session configuration service is unavailable");
    const remove = configuration.contribute({
      debug: { enabled: true, categories: ["session"] },
    });
    context.lifecycle.defer("debug-session", remove);
  },
};
