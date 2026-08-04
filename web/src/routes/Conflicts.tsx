// SPDX-License-Identifier: AGPL-3.0-only

// The "needs attention" tray (docs/04 §Upstream 3, ADR-004 §Conflict UX).
//
// Everything here renders the *server's* words. A rejected command comes back
// as problem+json with a title and a detail the server already wrote for the
// online path, so the tray shows that rather than a client-side approximation
// of it — which is the practical payoff of WP-2.3-decisions.md §1: replaying an
// ordinary request means the rejection is already an explanation.
//
// What the screen owes INV-S4 is that a rejected command is *here*, visibly,
// until a person decides otherwise. Discarding is the only way one leaves the
// system unsent, and it is a deliberate act by the user whose work it is.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { useI18n } from "../i18n";
import { SYNC_CONFLICTS_KEY, useReplica } from "../sync/ReplicaContext";
import type { Conflict } from "../sync/outbox";
import { Alert, Busy, Button, Table } from "../ui";

export function Conflicts() {
  const { t } = useI18n();
  const client = useReplica();
  const queryClient = useQueryClient();

  const { data, isPending, error } = useQuery({
    queryKey: SYNC_CONFLICTS_KEY,
    queryFn: () => client.conflicts(),
    retry: false,
  });

  const discard = useMutation({
    mutationFn: (commandId: string) => client.discard(commandId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["sync"] }),
  });

  if (isPending) {
    return <Busy label={t("status.loading")} />;
  }
  if (error) {
    return <Alert>{t("sync.unavailable")}</Alert>;
  }
  if (data.length === 0) {
    return (
      <section>
        <h1 className="text-xl font-semibold">{t("sync.conflicts.title")}</h1>
        <p className="mt-2 text-slate-600 dark:text-slate-300">{t("sync.conflicts.none")}</p>
      </section>
    );
  }

  return (
    <section>
      <h1 className="text-xl font-semibold">{t("sync.conflicts.title")}</h1>
      <p className="mt-2 text-slate-600 dark:text-slate-300">
        {t("sync.conflicts.intro", { count: data.length })}
      </p>

      <Table
        caption={t("sync.conflicts.title")}
        columns={[
          t("sync.conflicts.column.change"),
          t("sync.conflicts.column.reason"),
          t("sync.conflicts.column.action"),
        ]}
      >
        {data.map((conflict) => (
          <ConflictRow
            key={conflict.commandId}
            conflict={conflict}
            onDiscard={() => discard.mutate(conflict.commandId)}
            discarding={discard.isPending}
          />
        ))}
      </Table>
    </section>
  );
}

function ConflictRow({
  conflict,
  onDiscard,
  discarding,
}: {
  conflict: Conflict;
  onDiscard: () => void;
  discarding: boolean;
}) {
  const { t } = useI18n();

  return (
    <tr className="border-t border-slate-200 dark:border-slate-700">
      <td className="px-3 py-2 align-top">
        <div className="font-medium">{describeChange(conflict, t)}</div>
        {/* The values the user actually entered, so "edit and resubmit" is a
            decision they can make rather than a guess. i18n-ignore: user data. */}
        <pre className="mt-1 overflow-x-auto text-xs text-slate-600 dark:text-slate-300">
          {JSON.stringify(conflict.body ?? {}, null, 1)}
        </pre>
      </td>
      <td className="px-3 py-2 align-top">
        {/* i18n-ignore: the server's own problem+json, already localized by the
            server for this request — re-translating it here would replace what
            actually happened with a client-side guess at it. */}
        <div className="font-medium">{conflict.title}</div>
        {conflict.detail !== "" && (
          <div className="mt-1 text-sm text-slate-600 dark:text-slate-300">{conflict.detail}</div>
        )}
      </td>
      <td className="px-3 py-2 align-top">
        <Button variant="danger" onClick={onDiscard} disabled={discarding}>
          {t("sync.conflicts.discard")}
        </Button>
      </td>
    </tr>
  );
}

/** describeChange names what was attempted, in the user's language. The method
 * is the only part of a command that is ours to phrase — the rest is either the
 * user's data or the server's explanation. */
function describeChange(
  conflict: Conflict,
  t: ReturnType<typeof useI18n>["t"],
): string {
  switch (conflict.method) {
    case "POST":
      return t("sync.conflicts.change.create", { object: conflict.object });
    case "PATCH":
      return t("sync.conflicts.change.update", { object: conflict.object });
    default:
      return t("sync.conflicts.change.delete", { object: conflict.object });
  }
}
