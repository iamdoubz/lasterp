// SPDX-License-Identifier: AGPL-3.0-only
import { QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { ApiError, listCapabilities } from "./api";
import { buildRouter } from "./router";
import { Login } from "./routes/Login";

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
  if (signedIn === null) {
    void listCapabilities().then(
      () => setSignedIn(true),
      () => setSignedIn(false),
    );
    return null;
  }

  if (!signedIn) {
    return <Login onSignedIn={() => setSignedIn(true)} />;
  }

  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
}
