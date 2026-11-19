'use strict';
// Service worker for the web apps on the user's PC (HomeLLM's protocol/httprelay.go). Each app
// has a bridge page (/<app>/) that holds the WebRTC connection to the PC and shows the app's
// own page (/<app>/app/, static, from CloudFront) in a frame. That page's server requests are
// for the app's server on the PC, so this worker hands them to the bridge page, which relays
// them (apps/bridge.js). Responses stream back in pieces, pulled as the page reads them, so a
// long video plays and seeks without being downloaded whole. Everything else on the site
// passes straight through.

const APPS = {
  dubbing: /^(api|media)\//,               // video-dubbing's studio
  studio: /^(api|comfy|ollama|runs)\//,     // HomeLLM Studio (cmd/studio)
};
const PULL_BYTES = 256 * 1024;

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin) return;
  const m = url.pathname.match(/^\/([a-z]+)\/app\/(.*)$/);
  if (!m || !APPS[m[1]] || !APPS[m[1]].test(m[2])) return; // the page's own files: CloudFront
  event.respondWith(relay(m[1], event.request, '/' + m[2] + url.search));
});

async function bridgeClient(app) {
  const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  return all.find((c) => {
    const p = new URL(c.url).pathname;
    return p === `/${app}/` || p === `/${app}/index.html`;
  });
}

const FORWARD = ['accept', 'content-type', 'range', 'if-none-match', 'if-modified-since', 'if-range'];

async function relay(app, request, path) {
  const bridge = await bridgeClient(app);
  if (!bridge) return new Response(`Open /${app}/ on HomeLLM to reach your PC.`, { status: 503 });
  const headers = {};
  for (const h of FORWARD) { const v = request.headers.get(h); if (v) headers[h] = v; }
  const body = ['GET', 'HEAD'].includes(request.method) ? null : await request.arrayBuffer();
  if (body) headers['content-length'] = String(body.byteLength);

  const { port1, port2 } = new MessageChannel();
  return new Promise((resolve) => {
    let ctrl = null, waiter = null, done = false;
    const wake = () => { const w = waiter; waiter = null; w?.(); };
    port1.onmessage = ({ data: m }) => {
      if (m.t === 'head') {
        const noBody = request.method === 'HEAD' || [204, 205, 304].includes(m.status);
        const stream = noBody ? null : new ReadableStream({
          start(c) { ctrl = c; },
          // Ask the PC for more only as the page reads: the stream stays about 2 MB ahead.
          pull() {
            if (done) return;
            port1.postMessage({ t: 'pull', credit: PULL_BYTES });
            return new Promise((r) => { waiter = r; });
          },
          cancel() { done = true; port1.postMessage({ t: 'cancel' }); port1.close(); },
        }, new ByteLengthQueuingStrategy({ highWaterMark: 2 * 1024 * 1024 }));
        resolve(new Response(stream, { status: m.status, headers: m.headers }));
        if (noBody) port1.postMessage({ t: 'cancel' });
      } else if (m.t === 'data') {
        if (!done) ctrl?.enqueue(new Uint8Array(m.buf));
        wake();
      } else if (m.t === 'end') {
        done = true; ctrl?.close(); wake(); port1.close();
      } else if (m.t === 'error') {
        if (!ctrl) resolve(new Response(m.message || 'failed', { status: 502 }));
        else if (!done) ctrl.error(new Error(m.message));
        done = true; wake(); port1.close();
      }
    };
    bridge.postMessage({ kind: 'http', app, method: request.method, path, headers, body }, body ? [port2, body] : [port2]);
  });
}
