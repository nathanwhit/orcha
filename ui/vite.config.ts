import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const outDir = fileURLToPath(
  new URL("../internal/webui/dist", import.meta.url),
);

// dist/ is gitignored except .gitkeep (which keeps the go:embed pattern valid
// on a fresh checkout); emptyOutDir wipes it, so put it back after each build.
const keepGitkeep: Plugin = {
  name: "keep-gitkeep",
  closeBundle() {
    writeFileSync(`${outDir}/.gitkeep`, "");
  },
};

// The build output lands inside the Go module (internal/webui/dist) so it can
// be embedded into the orcha binary via go:embed. `npm run dev` proxies API
// calls to a locally running orcha server.
export default defineConfig({
  plugins: [react(), tailwindcss(), keepGitkeep],
  build: { outDir, emptyOutDir: true },
  server: {
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
});
