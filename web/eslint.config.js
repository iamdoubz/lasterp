// SPDX-License-Identifier: AGPL-3.0-only
import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.{ts,tsx}"],
    plugins: { "react-hooks": reactHooks },
    rules: reactHooks.configs.recommended.rules,
  },
  {
    // public/sw.js runs in a ServiceWorkerGlobalScope, not a window: `self`,
    // `caches` and `clients` are ambient there and undefined everywhere else.
    // Declared rather than the rule switched off, so a genuine typo in that
    // file is still caught.
    files: ["public/sw.js"],
    languageOptions: {
      globals: { self: "readonly", caches: "readonly", clients: "readonly", fetch: "readonly", URL: "readonly", Response: "readonly" },
    },
  },
);
