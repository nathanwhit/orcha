import { defineConfig, devices } from "@playwright/test";

// End-to-end tests drive the full stack against a real orcha server.
//
// The webServer below builds the UI (which writes into internal/webui/dist) and
// then runs the Go server with `go run`, so the binary embeds the freshly built
// dashboard and serves both the SPA and the /api endpoints on one port. It uses
// fake agents and an in-memory SQLite DB, so each run starts from a clean slate
// and needs no external services.
//
// Run with: npm run test:e2e (from ui/). Requires a Go toolchain on PATH and
// browsers installed via `npx playwright install chromium`.
const PORT = 8137;
const BASE_URL = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: "list",
  use: {
    baseURL: BASE_URL,
    trace: "on-first-retry",
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
  ],
  webServer: {
    // Build the UI then run the Go server from the repo root (one dir up).
    command: `npm run build --prefix ui && go run ./cmd/orcha -fake-agents -addr 127.0.0.1:${PORT} -db ':memory:'`,
    cwd: "..",
    url: BASE_URL,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
  },
});
