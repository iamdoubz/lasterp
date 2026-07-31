// SPDX-License-Identifier: AGPL-3.0-only

import { useState, type FormEvent } from "react";

import { ApiError, login } from "../api";
import { useT } from "../i18n";
import { Alert, Button, Field, Input } from "../ui";

/**
 * The login screen. It holds no token: a successful POST leaves the session in
 * HttpOnly cookies the browser sends automatically from then on
 * (WP-1.5-decisions.md §5), so "logged in" here is just "the call succeeded".
 */
export function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const t = useT();
  const [tenant, setTenant] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState("");
  const [pending, setPending] = useState(false);
  const [failed, setFailed] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setPending(true);
    setFailed(false);
    try {
      await login({ tenant, email, password, totp: totp || undefined });
      onSignedIn();
    } catch (err) {
      // Every failure renders the same message on purpose — the server already
      // refuses to say whether the user exists, and the UI must not undo that.
      if (!(err instanceof ApiError)) {
        console.error(err);
      }
      setFailed(true);
    } finally {
      setPending(false);
    }
  }

  return (
    <main className="mx-auto mt-16 w-full max-w-sm px-4">
      <h1 className="mb-6 text-2xl font-semibold text-slate-900 dark:text-slate-100">
        {t("login.title")}
      </h1>

      {failed && <Alert>{t("login.failed")}</Alert>}

      <form onSubmit={submit} noValidate>
        <Field id="tenant" label={t("login.tenant")} required>
          <Input
            id="tenant"
            name="tenant"
            autoComplete="organization"
            required
            value={tenant}
            onChange={(e) => setTenant(e.target.value)}
          />
        </Field>

        <Field id="email" label={t("login.email")} required>
          <Input
            id="email"
            name="email"
            type="email"
            autoComplete="username"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </Field>

        <Field id="password" label={t("login.password")} required>
          <Input
            id="password"
            name="password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>

        <Field id="totp" label={t("login.totp")} description={t("login.totpHint")}>
          <Input
            id="totp"
            name="totp"
            inputMode="numeric"
            autoComplete="one-time-code"
            value={totp}
            onChange={(e) => setTotp(e.target.value)}
          />
        </Field>

        <Button type="submit" variant="primary" disabled={pending} className="w-full justify-center">
          {pending ? t("login.pending") : t("login.submit")}
        </Button>
      </form>
    </main>
  );
}
