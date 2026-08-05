// SPDX-License-Identifier: AGPL-3.0-only
import { QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { ApiError, listCapabilities } from "./api";
import { buildRouter } from "./router";
import { Login } from "./routes/Login";
import { ReplicaProvider } from "./sync/ReplicaContext";

/**
 * App decides between the login screen and the authenticated shell.
 *
 * "Signed in" is not a token this code holds — the session lives in HttpOnly
 * cookies (WP-1.5-decisions.md §5), so the only honest test is whether the
 * server accepts a call. A 401 from anywhere drops back to login, which also
 * covers expiry mid-session without polling for it.
 */
export default function App() {
  const [signedIn, setSignedIn] = useState<boolean | null>(null);

  const queryClient = useMemo(
    () =>
      new QueryClient({
        queryCache: new QueryCache({
          onError: (error) => {
            if (error instanceof ApiError && error.isUnauthenticated) {
              setSignedIn(false);
            }
          },
        }),
        defaultOptions: {
          queries: {
            // A 401 is not a transient failure; retrying it just delays the
            // redirect to login.
            retry: (count, error) =>
              !(error instanceof ApiError && error.isUnauthenticated) && count < 2,
            staleTime: 30_000,
          },
        },
      }),
    [],
  );

  const router = useMemo(
    () => buildRouter({ onSignedOut: () => setSignedIn(false) }),
    [],
  );

  // Probe an authenticated route once on mount: a live cookie means the user is
  // already signed in and should not see the login form again.
  //
  // **A failure to reach the server is not a failure to authenticate.** Only a
  // real 401 drops to login; a transport error keeps the user in the shell. That
  // is the same distinction `kernel/api/gateway.go` draws server-side with
  // ErrAuthUnavailable — "could not check" answers 503, never 401 — and it has
  // to hold on both ends or the offline client signs itself out the moment it
  // loses the network, which is precisely when it is most useful (WP-2.7).
  //
  // The cost, accepted: someone who has never signed in and opens the app
  // offline sees the shell rather than a login form. That is the better of the
  // two lies — the screens then say this device has no local copy yet, which is
  // true and actionable, where a login form they cannot submit is neither.
  if (signedIn === null) {
    void listCapabilities().then(
      () => setSignedIn(true),
      (error: unknown) => setSignedIn(!(error instanceof ApiError && error.isUnauthenticated)),
    );
    return null;
  }

  if (!signedIn) {
    return <Login onSignedIn={() => setSignedIn(true)} />;
  }

  return (
    <QueryClientProvider client={queryClient}>
      {/* Inside the signed-in branch only: the replica holds one tenant's data
          and there is no session to bind it to before login. */}
      <ReplicaProvider>
        <RouterProvider router={router} />
      </ReplicaProvider>
    </QueryClientProvider>
  );
}
