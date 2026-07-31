// SPDX-License-Identifier: AGPL-3.0-only

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";

import { listObjects, logout, type MetaObject } from "../api";
import { useT } from "../i18n";
import { Alert, Busy, Button } from "../ui";

/** useObjects fetches the tenant's renderable schemas. Navigation is built from
 * this, so a disabled module simply has no nav entry — no dead links. */
export function useObjects() {
  return useQuery({ queryKey: ["meta", "objects"], queryFn: listObjects });
}

/** The authenticated shell: skip link, nav built from metadata, and the routed
 * content. */
export function Shell({ onSignedOut }: { onSignedOut: () => void }) {
  const t = useT();
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
          <Button onClick={() => signOut.mutate()} disabled={signOut.isPending}>
            {t("nav.signOut")}
          </Button>
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

function ObjectNav({ objects }: { objects: MetaObject[] }) {
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
            {o.name}
          </Link>
        </li>
      ))}
    </ul>
  );
}
