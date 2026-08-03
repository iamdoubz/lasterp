// SPDX-License-Identifier: AGPL-3.0-only
/// <reference types="vitest/config" />
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  // SQLite-WASM ships a .wasm next to its JS and resolves it by relative URL.
  // Pre-bundling rewrites the JS into .vite/deps and leaves the .wasm behind,
  // so the fetch falls through to the SPA handler and the runtime tries to
  // instantiate index.html as WebAssembly ("expected magic word 00 61 73 6d,
  // found 3c 21 64 6f" — that is `<!do`). Excluding it keeps the pair together.
  optimizeDeps: {
    exclude: ["@sqlite.org/sqlite-wasm"],
  },
  server: {
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: "./src/setupTests.ts",
    // Unit tests only. e2e/*.spec.ts matches vitest's default glob but is
    // Playwright's — running it here would fail on a missing browser.
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
  },
});
