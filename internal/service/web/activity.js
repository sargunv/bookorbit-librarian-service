function localizeTimes() {
  for (const node of document.querySelectorAll("[data-time]")) {
    const date = new Date(node.dataset.time);
    if (!Number.isNaN(date.getTime())) {
      node.textContent = date.toLocaleString(undefined, {
        month: "short",
        day: "numeric",
        hour: "numeric",
        minute: "2-digit",
      });
      node.title = date.toLocaleString();
    }
  }
}
localizeTimes();
if (!new URLSearchParams(location.search).has("before")) {
  setInterval(() => {
    if (!document.hidden && !document.querySelector("details[open]"))
      location.reload();
  }, 30000);
}
