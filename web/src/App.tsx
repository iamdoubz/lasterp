// SPDX-License-Identifier: AGPL-3.0-only
import { useEffect, useState } from "react";
import { useI18n } from "./i18n";

export default function App() {
  const { t } = useI18n();
  const [message, setMessage] = useState(() => t("app.status.loading"));

  useEffect(() => {
    fetch("/api/v1/hello")
      .then((r) => r.json())
      .then((body) => setMessage(body.message))
      .catch(() => setMessage(t("app.status.unreachable")));
  }, [t]);

  return (
    <main>
      <h1>{t("app.title")}</h1>
      <p>{t("app.tagline")}</p>
      <p>{message}</p>
    </main>
  );
}
