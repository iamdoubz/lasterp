// SPDX-License-Identifier: AGPL-3.0-only

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";

import { listObjects, logout, type MetaObject } from "../api";
import { LOCALE_NAMES, useI18n, type LocaleId } from "../i18n";
import { objectLabel } from "../meta/render";
import { Alert, Busy, Button, Select } from "../ui";

/** useObjects fetches the tenant's renderable schemas. Navigation is built from
 * this, so a disabled module simply has no nav entry — no dead links. */
export function useObjects() {
  return useQuery({ queryKey: ["meta", "objects"], queryFn: listObjects });
}

/** The authenticated shell: skip link, nav built from metadata, and the routed
 * content. */
export function Shell({ onSignedOut }: { onSignedOut: () => void }) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const { data, isPending, error } = useObjects();

  const signOut = useMutation({
    mutationFn: logout,
    onSuccess: () => {
      queryClient.clear();
      onSignedOut();
    },
  });

  return (
    <div className="min-h-screen bg-slate-50 text-slate-900 dark:bg-slate-900 dark:text-slate-100">
      {/* A keyboard user must be able to jump past the nav on every page. */}
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:m-2 focus:rounded focus:bg-white focus:px-3 focus:py-2 focus:text-slate-900"
      >
        {t("nav.skipToContent")}
      </a>

      <header className="border-b border-slate-200 bg-white dark:border-slate-700 dark:bg-slate-800">
        <div className="mx-auto flex max-w-5xl items-center justify-between gap-4 px-4 py-3">
          {/* i18n-ignore: brand name, intentionally untranslated */}
          <Link to="/" className="text-lg font-semibold">
            LastERP
          </Link>
          <div className="flex items-center gap-3">
            <LocaleSwitcher />
            <Button onClick={() => signOut.mutate()} disabled={signOut.isPending}>
              {t("nav.signOut")}
            </Button>
          </div>
        </div>

        <nav aria-label={t("nav.label")} className="mx-auto max-w-5xl px-4 pb-3">
          {isPending && <Busy label={t("status.loading")} />}
          {error && <Alert>{t("status.error")}</Alert>}
          {data && <ObjectNav objects={data} />}
        </nav>
      </header>

      <main id="main" className="mx-auto max-w-5xl px-4 py-6">
        <Outlet />
      </main>
    </div>
  );
}

/** LocaleSwitcher is a plain labelled <select>: a native control is keyboard-
 * and screen-reader-correct without a line of ARIA, and the choice is
 * remembered (see i18n.tsx's resolution chain). Language names are endonyms,
 * so they read the same whatever the UI is currently in. */
function LocaleSwitcher() {
  const { t, locale, setLocale } = useI18n();
  return (
    <label className="flex items-center gap-2 text-sm">
      <span className="text-slate-600 dark:text-slate-300">{t("nav.language")}</span>
      <Select
        value={locale.id}
        onChange={(e) => setLocale(e.target.value as LocaleId)}
      >
        {(Object.keys(LOCALE_NAMES) as LocaleId[]).map((id) => (
          <option key={id} value={id}>
            {LOCALE_NAMES[id]}
          </option>
        ))}
      </Select>
    </label>
  );
}

function ObjectNav({ objects }: { objects: MetaObject[] }) {
  const { label } = useI18n();
  return (
    <ul className="flex flex-wrap gap-4">
      {objects.map((o) => (
        <li key={o.name}>
          <Link
            to="/o/$resource"
            params={{ resource: o.resource }}
            activeProps={{ className: "font-semibold underline" }}
            className="text-sm text-sky-700 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-sky-600 dark:text-sky-400"
          >
            {objectLabel(o.name, label)}
          </Link>
        </li>
      ))}
    </ul>
  );
}
