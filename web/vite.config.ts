/// <reference types="vitest/config" />
import { writeFileSync } from "node:fs";
import path from "node:path";

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";

// dist/.gitkeep (.gitignore's narrowed `web/dist/*` / `!web/dist/.gitkeep`)
// keeps web/embed.go's `//go:embed all:dist` satisfied on a checkout that
// has never run this build. Vite's default `emptyOutDir` wipes outDir
// before writing, which would delete a committed .gitkeep too — closeBundle
// runs once per build, after every real output file is already written, so
// rewriting it here always leaves dist/ non-empty even when the build
// itself produced no committed placeholder of its own.
function keepDistPlaceholder(): Plugin {
  return {
    name: "hoserva-keep-dist-placeholder",
    apply: "build",
    closeBundle() {
      writeFileSync(path.resolve(__dirname, "dist/.gitkeep"), "");
    },
  };
}

// MOCK_ADDR (default 127.0.0.1:8090, Makefile's mock target) names the mock
// API server (doc 06 §8) this dev server proxies /api to. Never a public
// host: this file only ever runs as `vite dev`, never in the production
// build (Q8, Q49 — no outbound requests from the built app).
const mockAddr = process.env.MOCK_ADDR ?? "127.0.0.1:8090";

export default defineConfig({
  plugins: [react(), tailwindcss(), keepDistPlaceholder()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
      "@api": path.resolve(__dirname, "../api/gen/ts"),
      // api/gen/ts/client.ts imports "openapi-fetch" as a bare specifier;
      // that file lives outside web/, so Node-style resolution from its own
      // location would never reach web/node_modules — this alias, matching
      // tsconfig.app.json's "paths" entry, makes both the dev server and
      // the build resolve it to the copy installed here.
      "openapi-fetch": path.resolve(__dirname, "node_modules/openapi-fetch"),
    },
  },
  server: {
    host: "127.0.0.1",
    fs: {
      // The generated TypeScript client lives in ../api/gen/ts (D18) — a
      // sibling of web/, outside Vite's default project-root file boundary.
      allow: [path.resolve(__dirname, "..")],
    },
    proxy: {
      // The mock's secured endpoints require any credential (a bearer
      // token or a hoserva_session cookie, cmd/mockapi) — attaching one
      // here only affects proxied dev-server requests, never the built
      // app, so the SPA reaches fixtures without a login screen while
      // developing (doc 06 §8).
      "/api": {
        target: `http://${mockAddr}`,
        changeOrigin: true,
        // /api/v1/events is SSE (doc 01 §5): node-http-proxy streams by
        // default, but proxyTimeout: 0 makes sure a long-lived stream is
        // never closed for sitting idle between events.
        proxyTimeout: 0,
        configure(proxy) {
          proxy.on("proxyReq", (proxyReq) => {
            if (!proxyReq.getHeader("cookie") && !proxyReq.getHeader("authorization")) {
              proxyReq.setHeader("cookie", "hoserva_session=dev");
            }
          });
        },
      },
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: true,
  },
});
