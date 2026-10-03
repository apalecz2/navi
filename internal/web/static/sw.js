// Navi service worker. It has one job, making the shell installable, and a
// short list of things it refuses to do.
//
// CACHES, and only these: the content-versioned /app/static/* assets and the
// static /app/offline page, precached at install and served cache-first. Every
// one of them is addressed by ?v=<hash of the build>, so a cached copy is by
// construction the copy this build ships.
//
// REFUSES, by never calling respondWith for them: every non-GET request, every
// /api/* request (resolutions included), /app/ itself and /app/today. Those go
// straight to the network, so the browser's own behaviour applies and a failure
// is a visible failure. A navigation that cannot reach the network gets the
// offline page, not yesterday's list: a stale day view that looks live is worse
// than an offline one.
//
// __VERSION__ and __PRECACHE__ are filled in by internal/web when it serves
// this file.
const CACHE = 'navi-shell-__VERSION__';
const PRECACHE = [__PRECACHE__];

function cacheable(res) {
  return res.status === 200 && res.type === 'basic' && !res.redirected;
}

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE)
      .then((c) => Promise.all(PRECACHE.map(async (url) => {
        // Behind Access an expired session answers with a redirect to a login
        // page. Caching that under an asset URL would poison the shell.
        const res = await fetch(url, { credentials: 'same-origin' });
        if (!cacheable(res)) throw new Error('not cacheable: ' + url);
        await c.put(url, res);
      })))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  if (url.pathname.startsWith('/app/static/')) {
    event.respondWith(
      caches.open(CACHE).then(async (c) => {
        const hit = await c.match(req);
        if (hit) return hit;
        const res = await fetch(req);
        if (cacheable(res)) c.put(req, res.clone());
        return res;
      })
    );
    return;
  }

  if (req.mode === 'navigate' && url.pathname.startsWith('/app')) {
    event.respondWith(
      fetch(req).catch(() => caches.match('/app/offline').then((r) => r || Response.error()))
    );
  }
  // Everything else, /api/* and /app/today included: not intercepted.
});
