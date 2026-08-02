// SPDX-License-Identifier: AGPL-3.0-only

// The account-security screen (WP-1.12). It is the only surface that can turn a
// second factor on, which is what makes M1's "password+TOTP" true rather than
// aspirational — ValidateTOTP has shipped since WP-0.3 with nothing to enable it.

import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  confirmTotpEnrollment,
  disableTotp,
  getTotpStatus,
  startTotpEnrollment,
  type TotpEnrollment,
} from "../api";
import { useT } from "../i18n";
import { Alert, Busy, Button, Field, Input } from "../ui";

const STATUS_KEY = ["totp-status"];

export function Account() {
  const t = useT();
  const queryClient = useQueryClient();
  const { data: status, isPending, error } = useQuery({
    queryKey: STATUS_KEY,
    queryFn: getTotpStatus,
  });

  // The enrollment in flight, and the codes minted by confirming it. Both live
  // here rather than in the cache: they are shown once and must not survive a
  // refetch, a remount or a back navigation.
  const [enrollment, setEnrollment] = useState<TotpEnrollment | null>(null);
  const [codes, setCodes] = useState<string[] | null>(null);

  const refresh = () => queryClient.invalidateQueries({ queryKey: STATUS_KEY });

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error || !status) {
    return <Alert>{t("status.error")}</Alert>;
  }

  return (
    <main className="mx-auto w-full max-w-2xl px-4 py-8">
      <h1 className="mb-6 text-2xl font-semibold text-slate-900 dark:text-slate-100">
        {t("account.title")}
      </h1>

      <section aria-labelledby="totp-heading" className="mb-10">
        <h2
          id="totp-heading"
          className="mb-2 text-lg font-medium text-slate-900 dark:text-slate-100"
        >
          {t("account.totp.heading")}
        </h2>

        {codes ? (
          <RecoveryCodes
            codes={codes}
            onDone={() => {
              setCodes(null);
              setEnrollment(null);
              void refresh();
            }}
          />
        ) : enrollment ? (
          <ConfirmEnrollment
            enrollment={enrollment}
            onCancel={() => setEnrollment(null)}
            onConfirmed={(issued) => setCodes(issued)}
          />
        ) : status.enabled ? (
          <>
            <p className="mb-4 text-slate-700 dark:text-slate-300">{t("account.totp.on")}</p>
            <RecoveryCount remaining={status.recovery_codes_remaining} />
            <DisableForm onDisabled={refresh} />
          </>
        ) : (
          <StartEnrollment onStarted={setEnrollment} />
        )}
      </section>
    </main>
  );
}

function StartEnrollment({ onStarted }: { onStarted: (e: TotpEnrollment) => void }) {
  const t = useT();
  const [password, setPassword] = useState("");
  const start = useMutation({
    mutationFn: () => startTotpEnrollment(password),
    onSuccess: onStarted,
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    start.mutate();
  }

  return (
    <>
      <p className="mb-4 text-slate-700 dark:text-slate-300">{t("account.totp.off")}</p>
      <form onSubmit={submit} noValidate>
        {start.isError && <Alert>{t("account.enroll.failed")}</Alert>}
        <Field
          id="enroll-password"
          label={t("account.enroll.password")}
          description={t("account.enroll.why")}
          required
        >
          <Input
            id="enroll-password"
            name="enroll-password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
        <Button type="submit" variant="primary" disabled={start.isPending}>
          {start.isPending ? t("account.totp.starting") : t("account.totp.enable")}
        </Button>
      </form>
    </>
  );
}

function ConfirmEnrollment({
  enrollment,
  onCancel,
  onConfirmed,
}: {
  enrollment: TotpEnrollment;
  onCancel: () => void;
  onConfirmed: (codes: string[]) => void;
}) {
  const t = useT();
  const [code, setCode] = useState("");
  const confirm = useMutation({
    mutationFn: () => confirmTotpEnrollment(code),
    onSuccess: (result) => onConfirmed(result.recovery_codes),
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    confirm.mutate();
  }

  return (
    <>
      <p className="mb-4 text-slate-700 dark:text-slate-300">{t("account.totp.scan")}</p>

      {/* Decorative: the setup key below is the accessible equivalent, and it
          has to be there anyway for manual entry. Describing a bitmap would be
          worse for a screen reader than pointing at the string it encodes. */}
      <img
        src={enrollment.qr_png}
        alt=""
        width={256}
        height={256}
        className="mb-4 rounded border border-slate-200 bg-white p-2 dark:border-slate-700"
      />

      <Field id="totp-secret" label={t("account.totp.secret")}>
        <Input
          id="totp-secret"
          readOnly
          value={enrollment.secret}
          className="font-mono"
          onFocus={(e) => e.currentTarget.select()}
        />
      </Field>

      <form onSubmit={submit} noValidate>
        {confirm.isError && <Alert>{t("account.totp.confirmFailed")}</Alert>}
        <Field
          id="totp-confirm"
          label={t("account.totp.confirmLabel")}
          description={t("account.totp.confirmHint")}
          required
        >
          <Input
            id="totp-confirm"
            name="totp-confirm"
            inputMode="numeric"
            autoComplete="one-time-code"
            required
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
        </Field>
        <div className="flex gap-2">
          <Button type="submit" variant="primary" disabled={confirm.isPending}>
            {confirm.isPending ? t("account.totp.confirming") : t("account.totp.confirm")}
          </Button>
          <Button type="button" onClick={onCancel}>
            {t("account.totp.cancel")}
          </Button>
        </div>
      </form>
    </>
  );
}

function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const text = codes.join("\n");

  return (
    <>
      <h3 className="mb-2 text-base font-medium text-slate-900 dark:text-slate-100">
        {t("account.recovery.heading")}
      </h3>
      <Alert tone="info">{t("account.recovery.saveNow")}</Alert>

      <ul
        data-testid="recovery-codes"
        className="my-4 grid grid-cols-2 gap-2 rounded border border-slate-200 p-4 font-mono text-sm dark:border-slate-700"
      >
        {codes.map((code) => (
          <li key={code}>{code}</li>
        ))}
      </ul>

      <div className="flex flex-wrap gap-2">
        <Button
          onClick={() => {
            void navigator.clipboard.writeText(text).then(() => setCopied(true));
          }}
        >
          {copied ? t("account.recovery.copied") : t("account.recovery.copy")}
        </Button>
        <Button
          onClick={() => {
            // A blob URL rather than a data: URI, so the codes never appear in
            // the URL bar or in history. CSP fetch directives do not govern an
            // <a download>, so this needs no policy change.
            const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
            const a = document.createElement("a");
            a.href = url;
            a.download = "lasterp-recovery-codes.txt";
            a.click();
            // Firefox and Safari read the blob asynchronously after the click;
            // revoking synchronously cancels the download there.
            setTimeout(() => URL.revokeObjectURL(url), 30_000);
          }}
        >
          {t("account.recovery.download")}
        </Button>
        <Button variant="primary" onClick={onDone}>
          {t("account.recovery.done")}
        </Button>
      </div>
    </>
  );
}

/** The count at or below which the screen warns. Purely presentational — the
 * server enforces nothing at this number, which is why it lives here and not as
 * a constant in the kernel that would drift out of step unnoticed. */
const LOW_WATERMARK = 3;

function RecoveryCount({ remaining }: { remaining: number }) {
  const t = useT();
  return (
    <div className="mb-6">
      <p className="text-slate-700 dark:text-slate-300">
        {t("account.recovery.remaining", { count: remaining })}
      </p>
      {remaining === 0 && <Alert>{t("account.recovery.none")}</Alert>}
      {remaining > 0 && remaining <= LOW_WATERMARK && (
        <Alert tone="info">{t("account.recovery.low")}</Alert>
      )}
    </div>
  );
}

/** DisableForm collects the re-authentication the server demands. The password
 * alone will not do it: a session an attacker holds must not be able to strip
 * MFA off the account. */
function DisableForm({ onDisabled }: { onDisabled: () => void }) {
  const t = useT();
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState("");
  const [recoveryCode, setRecoveryCode] = useState("");

  const disable = useMutation({
    mutationFn: () =>
      disableTotp({
        password,
        totp: totp || undefined,
        recovery_code: recoveryCode || undefined,
      }),
    onSuccess: onDisabled,
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    disable.mutate();
  }

  return (
    <section aria-labelledby="disable-heading">
      <h3
        id="disable-heading"
        className="mb-2 text-base font-medium text-slate-900 dark:text-slate-100"
      >
        {t("account.disable.heading")}
      </h3>
      <p className="mb-4 text-slate-700 dark:text-slate-300">{t("account.disable.why")}</p>

      <form onSubmit={submit} noValidate>
        {disable.isError && <Alert>{t("account.disable.failed")}</Alert>}

        <Field id="disable-password" label={t("account.disable.password")} required>
          <Input
            id="disable-password"
            name="disable-password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>

        <Field id="disable-totp" label={t("account.disable.totp")}>
          <Input
            id="disable-totp"
            name="disable-totp"
            inputMode="numeric"
            autoComplete="one-time-code"
            value={totp}
            onChange={(e) => setTotp(e.target.value)}
          />
        </Field>

        <Field id="disable-recovery" label={t("account.disable.recoveryCode")}>
          <Input
            id="disable-recovery"
            name="disable-recovery"
            className="font-mono"
            value={recoveryCode}
            onChange={(e) => setRecoveryCode(e.target.value)}
          />
        </Field>

        <Button type="submit" variant="primary" disabled={disable.isPending}>
          {disable.isPending ? t("account.disable.pending") : t("account.disable.submit")}
        </Button>
      </form>
    </section>
  );
}
