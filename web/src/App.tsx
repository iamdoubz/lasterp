// SPDX-License-Identifier: AGPL-3.0-only
import { useEffect, useState } from "react";

export default function App() {
  const [message, setMessage] = useState("loading...");

  useEffect(() => {
    fetch("/api/v1/hello")
      .then((r) => r.json())
      .then((body) => setMessage(body.message))
      .catch(() => setMessage("kernel API unreachable"));
  }, []);

  return (
    <main>
      <h1>LastERP</h1>
      <p>{message}</p>
    </main>
  );
}
