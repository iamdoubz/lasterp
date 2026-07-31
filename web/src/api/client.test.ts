// SPDX-License-Identifier: AGPL-3.0-only
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ApiError, request } from "./client";

let fetchMock: ReturnType<typeof vi.fn>;

function jsonResponse(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "",
    json: () => Promise.resolve(body),
  };
}

beforeEach(() => {
  fetchMock = vi.fn(() => Promise.resolve(jsonResponse(200, { data: [] })));
  vi.stubGlobal("fetch", fetchMock as unknown as typeof fetch);
  vi.stubGlobal("crypto", { randomUUID: () => "fixed-key" });
});

afterEach(() => vi.unstubAllGlobals());

function lastInit(): RequestInit {
  return fetchMock.mock.calls.at(-1)![1] as RequestInit;
}

function header(name: string): string | undefined {
  return (lastInit().headers as Record<string, string>)[name];
}

test("credentials ride along so the HttpOnly session cookie is sent", () => {
  // The client holds no token by design; if this ever stops being "include",
  // every authenticated call silently starts failing.
  void request("/api/v1/contact");
  expect(lastInit().credentials).toBe("include");
});

test("no request carries a token header the client would have had to store", () => {
  void request("/api/v1/contact");
  const headers = lastInit().headers as Record<string, string>;
  expect(headers.Authorization).toBeUndefined();
});

test("every write carries an Idempotency-Key (ADR-009)", () => {
  for (const method of ["POST", "PATCH", "PUT", "DELETE"]) {
    void request("/api/v1/contact", { method, body: {} });
    expect(header("Idempotency-Key"), `${method} sent no Idempotency-Key`).toBeTruthy();
  }
});

test("reads do not carry an Idempotency-Key", () => {
  void request("/api/v1/contact");
  expect(header("Idempotency-Key")).toBeUndefined();
});

test("a caller-supplied key is reused so a retry is the same logical write", () => {
  void request("/api/v1/contact", { method: "POST", body: {}, idempotencyKey: "stable-key" });
  expect(header("Idempotency-Key")).toBe("stable-key");
});

test("a problem+json error surfaces as ApiError carrying the document", async () => {
  fetchMock.mockResolvedValueOnce(
    jsonResponse(422, {
      type: "about:blank",
      title: "unprocessable",
      status: 422,
      detail: "entry does not balance",
    }),
  );

  await expect(request("/api/v1/journalentries/x")).rejects.toMatchObject({
    name: "ApiError",
    problem: { status: 422, detail: "entry does not balance" },
  });
});

test("a 401 is flagged so the app can drop to the login screen", async () => {
  fetchMock.mockResolvedValueOnce(
    jsonResponse(401, { type: "about:blank", title: "authentication required", status: 401 }),
  );

  const err = await request("/api/v1/contact").catch((e: unknown) => e);
  expect(err).toBeInstanceOf(ApiError);
  expect((err as ApiError).isUnauthenticated).toBe(true);
});

test("a capability-disabled problem is distinguishable from a plain 403", async () => {
  fetchMock.mockResolvedValueOnce(
    jsonResponse(403, {
      type: "capability-disabled",
      title: "capability disabled",
      status: 403,
    }),
  );

  const err = (await request("/api/v1/contact").catch((e: unknown) => e)) as ApiError;
  expect(err.isCapabilityDisabled).toBe(true);
});

test("a non-JSON failure still becomes a usable problem", async () => {
  // A proxy 502 is HTML, not problem+json — the UI must still render something.
  fetchMock.mockResolvedValueOnce({
    ok: false,
    status: 502,
    statusText: "Bad Gateway",
    json: () => Promise.reject(new Error("not json")),
  });

  const err = (await request("/api/v1/contact").catch((e: unknown) => e)) as ApiError;
  expect(err.problem.status).toBe(502);
  expect(err.problem.title).toBe("Bad Gateway");
});

test("a 204 resolves without trying to parse a body", async () => {
  fetchMock.mockResolvedValueOnce({ ok: true, status: 204, statusText: "" });
  await expect(request("/api/v1/sessions/current", { method: "DELETE" })).resolves.toBeUndefined();
});
