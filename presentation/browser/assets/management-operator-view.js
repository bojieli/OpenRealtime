function node(name, text = "") {
  const value = document.createElement(name);
  value.textContent = text;
  return value;
}

export default {
  name: "openrealtime.presentation.client.management-operator-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const control = context.services.get("presentation.client.management_operator_control");
    if (!slots || !control) throw new Error("operator configuration view dependencies are unavailable");

    const section = node("section");
    section.dataset.view = "management-operator";
    section.append(node("h2", "Operator management capability"));
    const status = node("p", "Not configured");
    status.dataset.role = "status";
    const form = node("form");
    const label = node("label", "Capability ");
    const input = node("input");
    input.type = "password";
    input.name = "operator-capability";
    input.autocomplete = "off";
    input.maxLength = 512;
    label.append(input);
    const expiryLabel = node("label", " Client expiry (ms) ");
    const expiry = node("input");
    expiry.type = "number";
    expiry.name = "operator-expiry-ms";
    expiry.min = "0";
    expiry.max = "86400000";
    expiry.value = "0";
    expiryLabel.append(expiry);
    const configure = node("button", "Use capability");
    configure.type = "submit";
    const clear = node("button", "Clear");
    clear.type = "button";
    form.append(label, expiryLabel, configure, clear);
    section.append(status, form);

    const changed = control.subscribe((projection) => {
      status.textContent = projection.available
        ? `Configured · generation ${projection.generation}${projection.expires_at_ms
          ? ` · expires ${new Date(projection.expires_at_ms).toISOString()}` : ""}`
        : "Not configured";
    });
    const submit = (event) => {
      event.preventDefault();
      const capability = input.value;
      input.value = "";
      const lifetime = Number(expiry.value || 0);
      expiry.value = "0";
      try {
        if (!Number.isSafeInteger(lifetime) || lifetime < 0 || lifetime > 86_400_000) {
          throw new Error("invalid expiry");
        }
        control.replace(capability, lifetime === 0 ? 0 : Date.now() + lifetime);
      }
      catch { status.textContent = "Capability was rejected"; }
    };
    const revoke = () => {
      input.value = "";
      expiry.value = "0";
      try { control.clear(); } catch {}
    };
    form.addEventListener("submit", submit);
    clear.addEventListener("click", revoke);
    const unregister = slots.register("management.authority", section, 10);
    context.lifecycle.defer("management-operator-view-events", () => {
      form.removeEventListener("submit", submit);
      clear.removeEventListener("click", revoke);
      input.value = "";
      expiry.value = "0";
    });
    context.lifecycle.defer("management-operator-view-status", changed);
    context.lifecycle.defer("management-operator-view-slot", unregister);
  },
};
