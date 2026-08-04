// SPDX-License-Identifier: AGPL-3.0-only

// The single place the client talks to the server. Every screen goes through
// here, which is what makes the Phase-2 swap tractable: the sync client
// (ADR-004) replaces the transport in this file with a local SQLite replica
// read, and no screen changes (WP-1.5-decisions.md §1).

/** An RFC 7807 problem document, the server's error shape for every route. */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
}

/** ApiError carries the problem document so screens can render server detail
 * (validation messages, "capability disabled") instead of a generic failure. */
export class ApiError extends Error {
  readonly problem: Problem;

  constructor(problem: Problem) {
    super(problem.detail || problem.title);
    this.name = "ApiError";
    this.problem = problem;
  }

  /** True when the session is missing or expired — the router redirects to
   * login on this rather than showing an error page. */
  get isUnauthenticated(): boolean {
    return this.problem.status === 401;
  }

  /** True when this device has been remotely wiped (WP-2.5, INV-D1).
   *
   * A subset of `isUnauthenticated`, and callers must test it **first**: the
   * ordinary 401 path signs the user out and leaves the replica on disk, which
   * is the one outcome a wipe exists to prevent. This is the only 401 that
   * carries a machine-readable reason, for exactly that purpose
   * (WP-2.5-decisions.md §3). */
  get isDeviceWiped(): boolean {
    return this.problem.status === 401 && this.problem.type === "device-wiped";
  }

  /** True when the object's module is switched off for this tenant (ADR-018). */
  get isCapabilityDisabled(): boolean {
    return this.problem.type === "capability-disabled";
  }
}

/** idempotencyKey generates the key every write must carry (ADR-009). A retry
 * of the *same* user action should reuse its key; a new action gets a new one,
 * which is why this is called per submission and not per request. */
export function idempotencyKey(): string {
  return crypto.randomUUID();
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  /** Reuse a key across retries of one logical write. Generated when omitted. */
  idempotencyKey?: string;
  signal?: AbortSignal;
}

const WRITE_METHODS = new Set(["POST", "PATCH", "PUT", "DELETE"]);

/**
 * request issues one API call and returns the parsed body.
 *
 * Credentials ride as HttpOnly cookies (WP-1.5-decisions.md §5), so this sets
 * `credentials: "include"` and never touches a token — there is no token in JS
 * to touch.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? "GET";
  const headers: Record<string, string> = { Accept: "application/json" };

  if (options.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (WRITE_METHODS.has(method)) {
    headers["Idempotency-Key"] = options.idempotencyKey ?? idempotencyKey();
  }

  const response = await fetch(path, {
    method,
    headers,
    credentials: "include",
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    signal: options.signal,
  });

  if (response.status === 204) {
    return undefined as T;
  }
  if (!response.ok) {
    throw new ApiError(await toProblem(response));
  }
  return (await response.json()) as T;
}

/** toProblem parses a problem+json body, falling back to a synthetic problem
 * when the server (or a proxy in front of it) returns something else — a 502
 * from a load balancer is still an error the UI must render. */
async function toProblem(response: Response): Promise<Problem> {
  try {
    const body = (await response.json()) as Partial<Problem>;
    if (typeof body?.title === "string") {
      return {
        type: body.type ?? "about:blank",
        title: body.title,
        status: body.status ?? response.status,
        detail: body.detail,
        instance: body.instance,
      };
    }
  } catch {
    // fall through to the synthetic problem below
  }
  return {
    type: "about:blank",
    title: response.statusText || "request failed",
    status: response.status,
  };
}

/** A list response from any collection route. */
export interface ListResponse<T> {
  data: T[];
}

/** A record as the generic CRUD engine returns it: the declared fields plus the
 * kernel's own columns. Values are unknown because the shape is metadata-driven
 * — the renderer resolves each one against its field type. */
export type Record_ = Record<string, unknown> & { id?: string };
