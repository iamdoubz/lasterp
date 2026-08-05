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

// The app shell cache (WP-2.7, public/sw.js). Registered after render so it
// never delays first paint, and failures are swallowed: a browser without
// service workers, or a context that forbids them, must still run the app —
// it simply cannot survive a reload offline, which is a degradation rather
// than a fault.
if ("serviceWorker" in navigator) {
  addEventListener("load", () => {
    void navigator.serviceWorker.register("/sw.js").catch(() => undefined);
  });
}
