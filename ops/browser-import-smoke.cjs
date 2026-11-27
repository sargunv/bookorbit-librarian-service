// Requires Playwright and its Chromium browser; exercises the real dashboard.
const { chromium } = require("playwright");
(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const page = await browser.newPage();
    const violations = [];
    page.on("console", (message) => {
      if (
        /Content Security Policy|violates.*directive|Refused to connect/.test(
          message.text(),
        )
      )
        violations.push(message.text());
    });
    const poll = page.waitForResponse(
      (response) =>
        response.url().endsWith("/imports/jobs") && response.status() === 200,
    );
    await page.goto(process.argv[2]);
    await poll;
    await page.locator("#import-files").setInputFiles({
      name: "Example.epub",
      mimeType: "application/epub+zip",
      buffer: Buffer.from("browser upload fixture"),
    });
    const upload = page.waitForResponse(
      (response) =>
        response.url().endsWith("/imports/upload") && response.status() === 202,
    );
    await page
      .getByRole("button", { name: "Import books", exact: true })
      .click();
    await upload;
    await page
      .locator("#import-jobs li")
      .filter({ hasText: "Example.epub" })
      .waitFor();
    if (violations.length) throw new Error("CSP blocked dashboard requests");
    console.log(
      "Chromium: polling 200, form upload 202, queued job visible, no CSP violations",
    );
  } finally {
    await browser.close();
  }
})().catch((error) => {
  console.error(error.message);
  process.exitCode = 1;
});
