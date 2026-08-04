// SPDX-License-Identifier: AGPL-3.0-only

// The host side of the worker boundary. What is under test is not the worker —
// it is the correlation: replies must reach the caller that asked, a worker-side
// failure must arrive as the right *type*, and closing must not leave a screen
// awaiting a promise that can never settle.

import { describe, expect, it } from "vitest";

import { ReplicaLocked, startReplica, type WorkerLike } from "./host.ts";
import type { SyncResponse } from "./protocol.ts";

/** fakeWorker records what it was sent and lets a test reply out of order. */
function fakeWorker() {
  const sent: { id: number; kind: string; object?: string }[] = [];
  const worker: WorkerLike = {
    postMessage(message: unknown) {
      sent.push(message as { id: number; kind: string });
    },
    terminate() {},
    onmessage: null,
  };
  const reply = (response: SyncResponse) =>
    worker.onmessage?.({ data: response } as MessageEvent<SyncResponse>);
  return { worker, sent, reply };
}

describe("replica host", () => {
  it("carries the command payload, not just its kind", () => {
    // Regression guard: the protocol's id is an intersection rather than a
    // union arm precisely because Omit<Union, "id"> collapses to the common
    // keys, which silently dropped `object` from a list command.
    const { worker, sent } = fakeWorker();
    const client = startReplica(() => worker);
    void client.list("Contact");
    expect(sent[0]).toMatchObject({ kind: "list", object: "Contact" });
  });

  it("routes each reply to the caller that asked, out of order", async () => {
    const { worker, sent, reply } = fakeWorker();
    const client = startReplica(() => worker);

    const first = client.list("Contact");
    const second = client.list("Account");
    expect(sent).toHaveLength(2);

    reply({ id: sent[1].id, ok: true, value: [{ id: "a" }] });
    reply({ id: sent[0].id, ok: true, value: [{ id: "c" }] });

    await expect(second).resolves.toEqual([{ id: "a" }]);
    await expect(first).resolves.toEqual([{ id: "c" }]);
  });

  it("re-types a locked replica so the shell can render it as a state", async () => {
    // Structured clone drops the prototype, so the worker sends the class name
    // and the host rebuilds the type. Without this a second tab would surface
    // as a generic failure and the shell could not tell it apart.
    const { worker, sent, reply } = fakeWorker();
    const client = startReplica(() => worker);

    const pending = client.sync();
    reply({ id: sent[0].id, ok: false, name: "ReplicaLockedError", message: "already open" });

    await expect(pending).rejects.toBeInstanceOf(ReplicaLocked);
  });

  it("surfaces any other worker failure as a plain error", async () => {
    const { worker, sent, reply } = fakeWorker();
    const client = startReplica(() => worker);

    const pending = client.sync();
    reply({ id: sent[0].id, ok: false, name: "Error", message: "network down" });

    await expect(pending).rejects.toThrow("network down");
    await expect(pending).rejects.not.toBeInstanceOf(ReplicaLocked);
  });

  it("rejects outstanding work on close instead of hanging", async () => {
    const { worker, reply, sent } = fakeWorker();
    const client = startReplica(() => worker);

    const pending = client.sync();
    client.close();
    await expect(pending).rejects.toThrow(/closed/);

    // A late reply for a request already rejected must not throw.
    expect(() => reply({ id: sent[0].id, ok: true, value: 1 })).not.toThrow();
  });
});
