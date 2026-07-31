// SPDX-License-Identifier: AGPL-3.0-only
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { I18nProvider } from "./i18n";
import "./index.css";

// No locale prop: the provider resolves ?locale= → the remembered choice → the
// browser's languages → English, and the shell's switcher can change it.
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <I18nProvider>
      <App />
    </I18nProvider>
  </StrictMode>,
);
