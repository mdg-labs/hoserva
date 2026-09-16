import js from "@eslint/js";
import i18next from "eslint-plugin-i18next";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import { globalIgnores } from "eslint/config";
import tseslint from "typescript-eslint";

// D18: the generated TypeScript client (imported only through
// src/lib/api/) is the sole way this app talks to the API — no page or
// component may reach for fetch/XMLHttpRequest/EventSource/WebSocket,
// their window/globalThis/self-qualified spellings (dot notation
// `window.fetch` and bracket notation `window["fetch"]` alike),
// navigator.sendBeacon (either spelling), or an HTTP client library,
// itself — including a CommonJS `require()` of one. `no-restricted-globals`
// only matches a bare identifier reference, and `no-restricted-imports`'
// path list doesn't reach a dynamic `import()` on every version this repo
// has exercised — `no-restricted-syntax` below closes both gaps with
// explicit AST selectors rather than trusting either rule's own reach.
// `require("react-toastify")`-style calls to a future toast/notification
// library are not restricted here — this guard only owns network access,
// not every third-party dependency — so the next issue that adds toasts
// should decide separately whether that call site needs its own review.
const HTTP_LIBRARIES = ["axios", "ky", "got", "node-fetch", "superagent", "openapi-fetch"];
const NETWORK_GLOBAL_NAMES = "fetch|XMLHttpRequest|EventSource|WebSocket";
const noDirectHttp = {
  "no-restricted-globals": [
    "error",
    { name: "fetch", message: "Call the API through hoservaClient (src/lib/api), not fetch() directly (D18)." },
    { name: "XMLHttpRequest", message: "Call the API through hoservaClient (src/lib/api), not XMLHttpRequest (D18)." },
    { name: "EventSource", message: "Consume /api/v1/events through src/lib/api, not a raw EventSource (D18)." },
    { name: "WebSocket", message: "Call the API through hoservaClient (src/lib/api), not WebSocket directly (D18)." },
  ],
  "no-restricted-imports": [
    "error",
    {
      paths: HTTP_LIBRARIES.map((name) => ({
        name,
        message: `Call the API through hoservaClient (src/lib/api), not ${name} (D18).`,
      })),
    },
  ],
  "no-restricted-syntax": [
    "error",
    {
      // Matches both `window.fetch` (non-computed: property is an
      // Identifier, checked via property.name) and `window["fetch"]`
      // (computed: property is a string Literal, checked via
      // property.value) — a plain `property.name` selector only ever sees
      // the former.
      selector: `MemberExpression[object.name=/^(window|globalThis|self)$/]:matches([property.name=/^(${NETWORK_GLOBAL_NAMES})$/], [property.value=/^(${NETWORK_GLOBAL_NAMES})$/])`,
      message:
        "Call the API through hoservaClient (src/lib/api), not window/globalThis/self's networking globals (D18).",
    },
    {
      selector:
        "MemberExpression[object.name='navigator']:matches([property.name='sendBeacon'], [property.value='sendBeacon'])",
      message: "Call the API through hoservaClient (src/lib/api), not navigator.sendBeacon (D18).",
    },
    {
      selector: `ImportExpression[source.value=/^(${HTTP_LIBRARIES.join("|")})$/]`,
      message: "Call the API through hoservaClient (src/lib/api); a dynamic import of an HTTP library bypasses D18.",
    },
    {
      selector: `CallExpression[callee.name='require'][arguments.0.value=/^(${HTTP_LIBRARIES.join("|")})$/]`,
      message: "Call the API through hoservaClient (src/lib/api); a require() of an HTTP library bypasses D18.",
    },
  ],
};

// Q48/doc 03 "Cross-cutting rules": every string a page or pattern renders,
// including a JSX attribute a user can see or hear (aria-label,
// aria-description, title, placeholder, alt, a coss component's `label`
// prop), comes from the i18n catalog. `mode: "jsx-only"` (not the plugin's
// default `jsx-text-only`) is required for that: `jsx-text-only` skips any
// literal whose *direct* parent isn't a JSXElement/JSXFragment — which is
// every JSXAttribute value, whatever `jsx-attributes` says — so it never
// actually inspects an attribute regardless of include/exclude. `jsx-only`
// only demands the literal appear *somewhere* inside JSX, which reaches
// attributes too. `jsx-attributes.include` names the user-visible
// attributes to always check (overriding everything else, so a future
// careless exclude can't quietly re-open one of them); `exclude` names the
// technical, non-user-visible ones this app actually uses so the rule
// doesn't fight route paths, ARIA machinery and coss's own style/variant
// props. `should-validate-template: true` extends the same check to a
// template-literal text child or attribute value (`` <div>{`Hello`}</div> ``
// or `aria-label={\`Section\`}\`), which the plugin otherwise leaves
// unchecked. This rule only reaches JSX (`mode: "jsx-only"`); a future
// non-JSX call site — a `toast("text")`, a hardcoded label array — is
// outside its reach and needs its own review when that code lands.
const noLiteralStringOptions = {
  mode: "jsx-only",
  "should-validate-template": true,
  "jsx-attributes": {
    include: ["aria-label", "aria-description", "title", "placeholder", "alt", "label"],
    exclude: [
      "className",
      "styleName",
      "style",
      "type",
      "key",
      "id",
      "width",
      "height",
      "data-.*",
      "href",
      "variant",
      "size",
      "tone",
      "role",
      "path",
      "titleKey",
      "aria-hidden",
      "aria-current",
    ],
  },
};

export default tseslint.config(
  globalIgnores(["dist", "node_modules", "src/components/ui", "src/hooks/use-media-query.ts", "src/lib/segmented-control.ts"]),
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat["recommended-latest"],
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      ecmaVersion: 2023,
      globals: { window: "readonly", document: "readonly", navigator: "readonly" },
    },
    rules: noDirectHttp,
  },
  // Every string a page or pattern renders — text and the user-visible
  // attributes above — comes from the i18n catalog (Q48) from the first
  // component. Scoped to all of src/ except: tests (which deliberately
  // exercise raw strings against this very config), the vendored coss
  // registry output (src/components/ui/ — copy-and-owned library code, not
  // Hoserva's product copy, and already fully excluded above), and
  // src/lib/api/ (the generated client wrapper; no UI strings). Every
  // other directory under src/ either renders JSX (routes, patterns,
  // App.tsx, main.tsx) or, for the ones that don't (lib/theme.ts,
  // lib/utils.ts, lib/i18n/ — DOM/theme/catalog plumbing with no JSX and no
  // user copy, hooks/ — already excluded above as vendored), is a no-op
  // under jsx-only mode rather than needing its own carve-out.
  {
    files: ["src/**/*.{ts,tsx}"],
    ignores: ["src/lib/api/**", "src/test/**", "**/*.test.{ts,tsx}"],
    extends: [i18next.configs["flat/recommended"]],
    rules: {
      "i18next/no-literal-string": ["error", noLiteralStringOptions],
    },
  },
  // The one file allowed to reach for the generated client's own
  // transport (openapi-fetch, which calls fetch internally) and the dev
  // proxy's own Node config, which never ships to the browser.
  {
    files: ["src/lib/api/**/*.{ts,tsx}", "vite.config.ts"],
    rules: {
      "no-restricted-globals": "off",
      "no-restricted-imports": "off",
      "no-restricted-syntax": "off",
    },
  },
  {
    files: ["src/test/**/*.{ts,tsx}", "**/*.test.{ts,tsx}"],
    rules: {
      "no-restricted-globals": "off",
      "no-restricted-imports": "off",
      "no-restricted-syntax": "off",
    },
  },
);
