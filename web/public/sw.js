// SPDX-License-Identifier: AGPL-3.0-only

// The app shell cache (WP-2.7).
//
// The replica holds the *data* offline. Until this file existed, the
// *application* was still fetched from the server on every load, so reloading
// the tab offline failed at the document request with ERR_INTERNET_DISCONNECTED
// — found by running M2's airplane-mode script, whose steps 1 and 8 both reload.
// A replica you can only use until you press F5 is not "work all day offline".
//
// Hand-written rather than vite-plugin-pwa/Workbox: this is ~40 lines of
// fetch-with-fallback, and CLAUDE.md wants an ADR for a new runtime dependency.
// If the caching story ever grows past "serve the last shell we saw", that trade
// changes and the ADR is the right place to make it.

const CACHE = "lasterp-shell-v1";

// **/api/v1 is never cached, and that is load-bearing.** A cached API response
// would be a second read path for data the replica already owns — precisely the
// fallback WP-2.7-decisions.md §2 refuses, arriving through the back door. The
// replica is the read path; this file only makes the application itself
// available. Same for the sync worker's own traffic, which is API traffic.
function cacheable(request) {
  if (request.method !== "GET") return false;
  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return false;
  return !url.pathname.startsWith("/api/v1/");
}

self.addEventListener("install", (event) => {
  // Precache the entry document only. Vite content-hashes asset filenames, so
  // there is no stable list to enumerate here; the assets arrive through the
  // runtime cache below on first load, which is the load that has a network.
  event.waitUntil(
    caches.open(CACHE).then((cache) => cache.addAll(["/"])).then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

// Network-first, cache-fallback — not cache-first.
//
// Cache-first would serve stale JavaScript to an online user until the cache
// name changed, which is how a deploy silently fails to reach anybody. This way
// a connected user always runs current code and a disconnected one runs the last
// code they saw, which is the honest ordering of those two risks.
self.addEventListener("fetch", (event) => {
  const { request } = event;
  if (!cacheable(request)) return;

  event.respondWith(
    fetch(request)
      .then((response) => {
        if (response.ok) {
          const copy = response.clone();
          void caches.open(CACHE).then((cache) => cache.put(request, copy));
        }
        return response;
      })
      .catch(async () => {
        const hit = await caches.match(request);
        if (hit) return hit;
        // A navigation to a route this device has never visited still has to
        // render: the SPA router resolves the path client-side, so the entry
        // document is the right answer for any navigation.
        if (request.mode === "navigate") {
          const shell = await caches.match("/");
          if (shell) return shell;
        }
        throw new Error("offline and not cached");
      }),
  );
});
