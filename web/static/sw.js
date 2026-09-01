// Splitpayments service worker.
//
// Scope is "/" (served from the site root) and it does three things:
//   1. Precaches the offline fallback page so navigations work offline.
//   2. Serves navigations network-first, falling back to the offline page.
//   3. Serves /static/ assets stale-while-revalidate (they are content-hashed
//      via ?v=, so cached copies never go stale within a build).
//
// Authenticated pages and APIs are never cached: every response that is not
// a navigation or a /static/ asset passes through untouched.
//
// The worker is registered with the app's asset version as its query
// (?v=...), so a rebuild changes the script URL, the browser installs the
// new worker, and activation drops the previous version's cache.

const VERSION = new URLSearchParams(self.location.search).get('v') || 'dev';
const CACHE = `splitpayments-${VERSION}`;
const OFFLINE_PAGE = `/static/offline.html?v=${VERSION}`;
const OFFLINE_CSS = `/static/css/offline.css?v=${VERSION}`;
const PRECACHE = [OFFLINE_PAGE, OFFLINE_CSS];

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(CACHE);
    await Promise.all(PRECACHE.map(async (url) => {
      const res = await fetch(url);
      if (cacheable(res)) await cache.put(url, res);
    }));
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(
      names.filter((name) => name !== CACHE).map((name) => caches.delete(name)),
    );
    await self.clients.claim();
  })());
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  if (req.mode === 'navigate') {
    event.respondWith(networkFirst(req));
    return;
  }
  if (url.pathname.startsWith('/static/')) {
    event.respondWith(staleWhileRevalidate(req));
  }
});

// Responses worth keeping: successful and not marked uncacheable. Dev mode
// (-static-dir) serves everything with no-store so local edits are never
// trapped in the cache.
function cacheable(res) {
  return res.ok && !(res.headers.get('Cache-Control') || '').includes('no-store');
}

async function networkFirst(req) {
  try {
    return await fetch(req);
  } catch (err) {
    const cache = await caches.open(CACHE);
    return (await cache.match(OFFLINE_PAGE)) || Response.error();
  }
}

async function staleWhileRevalidate(req) {
  const cache = await caches.open(CACHE);
  const cached = await cache.match(req);
  const refresh = fetch(req)
    .then((res) => {
      if (cacheable(res)) cache.put(req, res.clone());
      return res;
    })
    .catch(() => undefined);
  return cached || (await refresh) || Response.error();
}
