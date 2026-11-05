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
function openLinkedEvent() {
  const hash = location.hash;
  if (!/^#event-\d+$/.test(hash)) return;
  const entry = document.getElementById(hash.slice(1));
  if (entry instanceof HTMLDetailsElement) entry.open = true;
}
openLinkedEvent();
addEventListener("hashchange", openLinkedEvent);
document.addEventListener(
  "error",
  (event) => {
    if (event.target instanceof HTMLImageElement) event.target.hidden = true;
  },
  true,
);
if (!new URLSearchParams(location.search).has("before")) {
  setInterval(() => {
    if (!document.hidden && !document.querySelector("details[open]"))
      location.reload();
  }, 30000);
}
