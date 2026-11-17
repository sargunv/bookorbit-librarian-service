const form = document.getElementById("import-form");
if (form) {
  const message = document.getElementById("import-message");
  const list = document.getElementById("import-jobs");
  let busy = false;
  async function refreshImports() {
    try {
      const response = await fetch("/imports/jobs");
      if (!response.ok) throw new Error("Import status unavailable");
      const jobs = await response.json();
      list.replaceChildren();
      for (const job of jobs.slice(0, 30)) {
        const row = document.createElement("li");
        const label = document.createElement("strong");
        label.textContent = job.name;
        row.append(
          label,
          ` — ${job.status}. ${job.message || "Waiting to process"} `,
        );
        if (job.status === "failed") {
          const retry = document.createElement("button");
          retry.type = "button";
          retry.textContent = "Retry";
          retry.addEventListener("click", async () => {
            retry.disabled = true;
            try {
              const response = await fetch(`/imports/${job.id}/retry`, {
                method: "POST",
                headers: { "X-Librarian-Import": "1" },
              });
              if (!response.ok) throw new Error("Could not retry import");
              await refreshImports();
            } catch (error) {
              message.textContent = error.message;
              retry.disabled = false;
            }
          });
          row.append(retry);
        }
        list.append(row);
      }
    } catch (error) {
      message.textContent = error.message;
    }
  }
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    if (busy) return;
    busy = true;
    const button = form.querySelector("button");
    button.disabled = true;
    message.textContent = "Uploading…";
    try {
      const files = document.getElementById("import-files").files;
      for (const file of files) {
        if (file.size > 250 * 1024 * 1024)
          throw new Error("Each file must be at most 250 MiB");
        const body = new FormData();
        body.append("file", file);
        const response = await fetch("/imports/upload", {
          method: "POST",
          headers: { "X-Librarian-Import": "1" },
          body,
        });
        if (!response.ok)
          throw new Error(
            "Upload rejected. Check the file type and size, then try again.",
          );
      }
      message.textContent = "Uploads received. Progress appears below.";
      form.reset();
      await refreshImports();
    } catch (error) {
      message.textContent = error.message;
    } finally {
      busy = false;
      button.disabled = false;
    }
  });
  refreshImports();
  setInterval(refreshImports, 5000);
  // Preserve uploads and file selections across the activity page's refresh.
  window.librarianImportActive = () =>
    busy || document.getElementById("import-files").files.length > 0;
}
