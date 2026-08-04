// SPDX-License-Identifier: AGPL-3.0-only

// The tray's half of INV-S4: a rejected command is visible, it says what the
// server said, and the only way it leaves is a person deciding so.
//
// The SyncClient is a fake rather than a worker. What is under test is what the
// screen does with a conflict, not postMessage — and the drain that produces
// one is proven against a live server in the simulation harness, which is where
// the invariant is actually carried.

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, test, vi } from "vitest";

import { I18nProvider, resolveLocale } from "../i18n";
import { ReplicaProvider } from "../sync/ReplicaContext";
import type { SyncClient } from "../sync/host";
import type { Conflict } from "../sync/outbox";
import { Conflicts } from "./Conflicts";

function conflict(overrides: Partial<Conflict> = {}): Conflict {
  return {
    commandId: "018f0000-0000-7000-8000-000000000001",
    method: "PATCH",
    path: "/api/v1/contact/c1",
    object: "Contact",
    rowId: "c1",
    body: { name: "Edited offline" },
    status: 422,
    type: "about:blank",
    title: "validation failed",
    detail: 'field "email" is not a valid email address',
    filedAt: "2026-08-04T10:00:00Z",
    ...overrides,
  };
}

function fakeClient(conflicts: Conflict[]): SyncClient {
  const remaining = [...conflicts];
  return {
    sync: vi.fn(async () => 0),
    status: vi.fn(async () => ({
      hydrated: true,
      cursor: 0,
      persisted: true,
      pending: 0,
      conflicts: remaining.length,
      limit: null,
    })),
    list: vi.fn(async () => []),
    write: vi.fn(async () => undefined),
    conflicts: vi.fn(async () => [...remaining]),
    discard: vi.fn(async (commandId: string) => {
      const at = remaining.findIndex((c) => c.commandId === commandId);
      if (at >= 0) remaining.splice(at, 1);
    }),
    close: vi.fn(),
  };
}

function renderTray(client: SyncClient) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale={resolveLocale("en")}>
        <ReplicaProvider client={client}>
          <Conflicts />
        </ReplicaProvider>
      </I18nProvider>
    </QueryClientProvider>,
  );
}

test("a rejected command is shown with the server's own explanation", async () => {
  renderTray(fakeClient([conflict()]));

  // The server's title and detail, not a client-side paraphrase: replaying an
  // ordinary request means the rejection arrives already explained
  // (WP-2.3-decisions.md §1).
  expect(await screen.findByText("validation failed")).toBeInTheDocument();
  expect(
    screen.getByText('field "email" is not a valid email address'),
  ).toBeInTheDocument();

  // And what was attempted, so "edit and resubmit" is a decision rather than a
  // guess.
  expect(screen.getByText(/Edit to a Contact/)).toBeInTheDocument();
  expect(screen.getByText(/Edited offline/)).toBeInTheDocument();
});

test("nothing is discarded until the user says so", async () => {
  const client = fakeClient([conflict()]);
  renderTray(client);

  await screen.findByText("validation failed");
  expect(client.discard).not.toHaveBeenCalled();

  await userEvent.click(screen.getByRole("button", { name: /discard/i }));

  await waitFor(() => expect(client.discard).toHaveBeenCalledWith(conflict().commandId));
  // INV-S4 is not "conflicts can be cleared" — it is that clearing one is an
  // act, and the tray empties only because a person performed it.
  await waitFor(() => expect(screen.queryByText("validation failed")).not.toBeInTheDocument());
});

test("an empty tray says every offline change was accepted", async () => {
  renderTray(fakeClient([]));
  expect(await screen.findByText(/Every change you made offline was accepted/)).toBeInTheDocument();
});

test("a replica this session cannot open renders a state, not a crash", async () => {
  const client = fakeClient([]);
  client.conflicts = vi.fn(async () => {
    throw new Error("sync: this replica is already open in another tab");
  });

  renderTray(client);

  // A locked replica is ordinary for an ERP user with two tabs
  // (WP-2.2b-decisions.md §5). The tray is one of the screens that has to say
  // so rather than showing an empty list, which would read as "no conflicts".
  expect(await screen.findByRole("alert")).toHaveTextContent(/not available/i);
});
