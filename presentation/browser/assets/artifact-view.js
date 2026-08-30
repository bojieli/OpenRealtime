export default {
  name: "openrealtime.presentation.client.artifact-view",
  revision: 1,
  async mount(context) {
    const slots = context.services.get("presentation.client.slots");
    const resources = context.services.get("presentation.client.artifacts");
    if (!slots || !resources) throw new Error("artifact view dependencies are unavailable");
    const section = document.createElement("section");
    section.dataset.view = "artifacts";
    const heading = document.createElement("h2");
    heading.textContent = "Artifacts and downloads";
    const diagnostic = document.createElement("p");
    diagnostic.setAttribute("role", "alert");
    const artifacts = document.createElement("div");
    const downloads = document.createElement("ul");
    section.append(heading, diagnostic, artifacts, downloads);

    const render = (snapshot) => {
      diagnostic.textContent = snapshot.diagnostic;
      artifacts.replaceChildren(...snapshot.artifacts.map((reference) => {
        const article = document.createElement("article");
        const title = document.createElement("h3");
        title.textContent = reference.title;
        const frame = document.createElement("iframe");
        frame.title = reference.title;
        // The response also supplies a CSP sandbox. The iframe attribute is a
        // second, client-owned boundary and deliberately omits same-origin,
        // forms, navigation, popups, downloads, media, and device authority.
        frame.setAttribute("sandbox", "allow-scripts");
        frame.referrerPolicy = "no-referrer";
        frame.src = `${reference.path}?version=${reference.version}&digest=${encodeURIComponent(reference.digest)}`;
        article.append(title, frame);
        return article;
      }));
      downloads.replaceChildren(...snapshot.downloads.map((reference) => {
        const item = document.createElement("li");
        const anchor = document.createElement("a");
        anchor.href = `${reference.path}?version=${reference.version}&digest=${encodeURIComponent(reference.digest)}`;
        anchor.download = reference.filename;
        anchor.referrerPolicy = "no-referrer";
        anchor.textContent = `${reference.filename} (${reference.bytes} bytes)`;
        item.append(anchor);
        return item;
      }));
    };
    const unregister = slots.register("artifact.viewer", section, 40);
    const unsubscribe = resources.subscribe(render);
    context.lifecycle.defer("artifact-view-state", unsubscribe);
    context.lifecycle.defer("artifact-view-slot", unregister);
  },
};
