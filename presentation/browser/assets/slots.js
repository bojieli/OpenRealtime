export default {
  name: "openrealtime.presentation.client.slots",
  revision: 1,
  async mount(context) {
    context.root.replaceChildren();
    const shell = document.createElement("main");
    shell.className = "room-shell";
    const stage = document.createElement("div"); stage.className = "room-stage";
    const conversation = document.createElement("div"); conversation.className = "room-conversation";
    const tools = document.createElement("details"); tools.className = "room-tools";
    const summary = document.createElement("summary"); summary.textContent = "Session details & developer tools";
    tools.append(summary);
    shell.append(stage, conversation, tools);
    context.root.append(shell);
    const slots = new Map();
    const render = () => {
      for (const [name, rows] of slots) {
        const target = name === "root" ? conversation : name === "session.media" ? stage : tools;
        for (const row of rows) target.append(row.node);
      }
      stage.hidden = !slots.has("session.media");
      shell.classList.toggle("room-text-only", stage.hidden);
    };
    const register = (name, node, order = 0) => {
      if (!name || !(node instanceof Node)) throw new Error("invalid slot registration");
      const rows = slots.get(name) ?? [];
      const row = { node, order };
      rows.push(row);
      rows.sort((left, right) => left.order - right.order);
      slots.set(name, rows);
      render();
      return () => {
        const current = slots.get(name) ?? [];
        const index = current.indexOf(row);
        if (index >= 0) current.splice(index, 1);
        node.remove();
        if (!current.length) slots.delete(name);
        render();
      };
    };
    context.publish("presentation.client.slots", Object.freeze({ register }));
    context.lifecycle.defer("clear-root", () => context.root.replaceChildren());
  },
};
