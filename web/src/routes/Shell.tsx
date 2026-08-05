// SPDX-License-Identifier: AGPL-3.0-only

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";

import { ApiError, listObjects, logout, type MetaObject } from "../api";
import { LOCALE_NAMES, useI18n, type LocaleId } from "../i18n";
import { objectLabel } from "../meta/render";
import { useDrainOnReconnect, useSyncStatus } from "../sync/ReplicaContext";
import { useReplica } from "../sync/ReplicaContext";
import { Alert, Busy, Button, Select } from "../ui";

/** useObjects fetches the tenant's renderable schemas. Navigation is built from
 * this, so a disabled module simply has no nav entry — no dead links.
 *
 * Offline it falls back to the copy the replica cached. That is **not** the API
 * fallback WP-2.7-decisions.md §2 refuses: `_meta` holds exactly what this
 * endpoint last returned (core.ts `cacheMeta`), so there is still one source —
 * the server — and one cache of it, which is the arrangement ADR-006 and
 * `replica.open()` already rely on. A nav built from stale *schema* shows the
 * wrong menu; a list built from stale *data* shows the wrong money, which is why
 * only one of the two gets a fallback. */
export function useObjects() {
  const replica = useReplica();
  return useQuery({
    queryKey: ["meta", "objects"],
    queryFn: async () => {
      try {
        return await listObjects();
      } catch (err) {
        // A 401 must still sign the user out; only a transport failure falls
        // back, for the same reason App.tsx's probe does.
        if (err instanceof ApiError) throw err;
        const cached = await replica.meta();
        if (cached.length === 0) throw err;
        return cached as MetaObject[];
      }
    },
  });
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
            <OfflineIndicator />
            <LocaleSwitcher />
            <Button onClick={() => signOut.mutate()} disabled={signOut.isPending}>
              {t("nav.signOut")}
            </Button>
          </div>
        </div>

        <UnpersistedWarning />

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

/** OfflineIndicator shows how much unsent work exists and how much of it is at
 * risk, and links to the tray when anything needs a decision.
 *
 * It renders nothing when there is nothing to say. A permanently-visible sync
 * badge trains people to ignore it, and the two things worth interrupting for —
 * work that has not reached the server, and work the server refused — are both
 * countable rather than perpetual. */
function OfflineIndicator() {
  const { t } = useI18n();
  const status = useSyncStatus();
  useDrainOnReconnect();

  if (status === null || (status.pending === 0 && status.conflicts === 0)) {
    return null;
  }

  return (
    <div className="flex items-center gap-3 text-sm">
      {status.pending > 0 && (
        <span className="text-slate-600 dark:text-slate-300">
          {status.limit === null
            ? t("sync.pending", { count: status.pending })
            : t("sync.pendingCapped", { count: status.pending, limit: status.limit })}
        </span>
      )}
      {status.conflicts > 0 && (
        <Link to="/sync" className="font-medium text-red-700 underline dark:text-red-300">
          {t("nav.needsAttention")}
        </Link>
      )}
    </div>
  );
}

/** UnpersistedWarning is WP-2.3-decisions.md §6.1: when the browser refuses
 * persistent storage, say so *before* the first offline write rather than after
 * an eviction.
 *
 * Not a toast, and not dismissable. Eviction clears the whole origin, so there
 * is no afterwards in which to apologise — the only moment this can be true is
 * before anything depends on it. */
function UnpersistedWarning() {
  const { t } = useI18n();
  const status = useSyncStatus();

  if (status === null || status.persisted) {
    return null;
  }
  return (
    <div className="mx-auto max-w-5xl px-4 pb-2">
      <Alert tone="info">{t("sync.unpersisted")}</Alert>
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
  const { t, label } = useI18n();
  return (
    <ul className="flex flex-wrap gap-4">
      <li>
        <Link
          to="/"
          activeProps={{ className: "font-semibold underline" }}
          activeOptions={{ exact: true }}
          className="text-sm text-sky-700 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-sky-600 dark:text-sky-400"
        >
          {t("nav.dashboard")}
        </Link>
      </li>
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
      {/* Account security sits with the object links rather than in a user menu
          there is no design for yet. It is not an object — no capability gates
          it and every signed-in user has one. */}
      <li>
        <Link
          to="/account"
          activeProps={{ className: "font-semibold underline" }}
          className="text-sm text-sky-700 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-sky-600 dark:text-sky-400"
        >
          {t("nav.account")}
        </Link>
      </li>
    </ul>
  );
}
