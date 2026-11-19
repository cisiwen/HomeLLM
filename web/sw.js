'use strict';
// Service worker for /dubbing/. The dubbing studio's page (/dubbing/app/) is static and comes
// from CloudFront, but its api/ and media/ requests are for the studio server on the user's PC.
// This worker hands each of them to the /dubbing/ bridge page, which holds the WebRTC
// connection to the PC and relays them (see bridge.js and HomeLLM's protocol/httprelay.go).
// Responses stream back in pieces, pulled as the page reads them, so a long video plays and
// seeks without being downloaded whole.

const APP_PREFIX = '/dubbing/app/';
const RELAYED = /^(api|media)\//;
const PULL_BYTES = 256 * 1024;

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin || !url.pathname.startsWith(APP_PREFIX)) return;
  const rest = url.pathname.slice(APP_PREFIX.length);
  if (!RELAYED.test(rest)) return; // the page's own files: CloudFront
  event.respondWith(relay(event.request, '/' + rest + url.search));
});

async function bridgeClient() {
  const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  return all.find((c) => {
    const p = new URL(c.url).pathname;
    return p === '/dubbing/' || p === '/dubbing/index.html';
  });
}

const FORWARD = ['accept', 'content-type', 'range', 'if-none-match', 'if-modified-since', 'if-range'];

async function relay(request, path) {
  const bridge = await bridgeClient();
  if (!bridge) return new Response('Open the Dubbing page from HomeLLM to reach your PC.', { status: 503 });
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
    bridge.postMessage({ kind: 'http', method: request.method, path, headers, body }, body ? [port2, body] : [port2]);
  });
}
