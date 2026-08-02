// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState, type FormEvent } from "react";

import { ApiError, login, startSso } from "../api";
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
  const [recoveryCode, setRecoveryCode] = useState("");
  // Which second factor the user is offering. A deliberate choice rather than
  // the server guessing from the shape of the input (WP-1.12-decisions.md §5).
  const [useRecovery, setUseRecovery] = useState(false);
  const [pending, setPending] = useState(false);
  const [failed, setFailed] = useState(false);
  // The provider's authorization URL, or null when this deployment has no
  // identity provider — the route 404s and no button is offered.
  const [ssoUrl, setSsoUrl] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    startSso()
      .then(({ authorization_url }) => {
        if (live) setSsoUrl(authorization_url);
      })
      .catch(() => {
        // No provider configured, or the provider is unreachable. Either way
        // the password form still works, so this is not an error to show.
      });
    return () => {
      live = false;
    };
  }, []);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setPending(true);
    setFailed(false);
    try {
      await login({
        tenant,
        email,
        password,
        totp: useRecovery ? undefined : totp || undefined,
        recovery_code: useRecovery ? recoveryCode || undefined : undefined,
      });
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

      {ssoUrl && (
        <Button
          variant="primary"
          className="mb-6 w-full justify-center"
          // A full navigation, not a fetch: the provider answers with its own
          // login page, and the callback comes back as a navigation too. The
          // flow state is already in the cookie the start call set.
          onClick={() => window.location.assign(ssoUrl)}
        >
          {t("login.sso")}
        </Button>
      )}

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

        {useRecovery ? (
          <Field
            id="recovery-code"
            label={t("login.recoveryCode")}
            description={t("login.recoveryHint")}
          >
            <Input
              id="recovery-code"
              name="recovery-code"
              className="font-mono"
              value={recoveryCode}
              onChange={(e) => setRecoveryCode(e.target.value)}
            />
          </Field>
        ) : (
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
        )}

        <Button
          type="button"
          className="mb-4"
          onClick={() => {
            // Clear the field being abandoned, so a stale value cannot be sent
            // in a field the user is no longer looking at.
            setTotp("");
            setRecoveryCode("");
            setUseRecovery((on) => !on);
          }}
        >
          {useRecovery ? t("login.useAuthenticator") : t("login.useRecovery")}
        </Button>

        <Button type="submit" variant="primary" disabled={pending} className="w-full justify-center">
          {pending ? t("login.pending") : t("login.submit")}
        </Button>
      </form>
    </main>
  );
}
