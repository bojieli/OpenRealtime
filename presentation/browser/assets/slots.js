export default {
  name: "openrealtime.presentation.client.slots",
  revision: 1,
  async mount(context) {
    context.root.replaceChildren();
    const slots = new Map();
    const register = (name, node, order = 0) => {
      if (!name || !(node instanceof Node)) throw new Error("invalid slot registration");
      const rows = slots.get(name) ?? [];
      const row = { node, order };
      rows.push(row);
      rows.sort((left, right) => left.order - right.order);
      slots.set(name, rows);
      context.root.replaceChildren(...[...slots.values()].flat().map((entry) => entry.node));
      return () => {
        const current = slots.get(name) ?? [];
        const index = current.indexOf(row);
        if (index >= 0) current.splice(index, 1);
        if (!current.length) slots.delete(name);
        context.root.replaceChildren(...[...slots.values()].flat().map((entry) => entry.node));
      };
    };
    context.publish("presentation.client.slots", Object.freeze({ register }));
    context.lifecycle.defer("clear-root", () => context.root.replaceChildren());
  },
};
