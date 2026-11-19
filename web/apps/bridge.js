'use strict';
// The /dubbing/ page: connects to the user's PC like the main HomeLLM page, then shows the
// dubbing studio (/dubbing/app/) in a frame. The studio's server runs on the PC; sw.js sends
// its api/ and media/ requests here, and this page relays them over the connection's "http"
// data channel (HomeLLM's protocol/httprelay.go).

const APP = 'dubbing';
let conn = null;
let nextN = 1;
const reqs = new Map(); // n -> { port }
let readyWaiters = [];

function setStatus(kind, text) {
  $('status').querySelector('.dot').className = 'dot ' + kind;
  $('statusText').textContent = text;
}

function showNote(html) {
  $('note').innerHTML = html;
  $('note').hidden = false;
  $('frame').hidden = true;
}

function pickDevice() {
  const keys = store.get('keys', {});
  const id = store.get('selected', '') in keys ? store.get('selected', '') : Object.keys(keys)[0];
  return id ? { device_id: id, ...keys[id] } : null;
}

function connected() {
  return conn?.ready && conn.hello && conn.httpCh?.readyState === 'open';
}

function ensureConnected() {
  if (connected()) return Promise.resolve();
  if (!conn || conn.closed) connect();
  return new Promise((resolve, reject) => readyWaiters.push({ resolve, reject }));
}

function connect() {
  const d = pickDevice();
  if (!d) {
    showNote('This browser isn\'t paired with a PC yet. <a href="/">Open HomeLLM</a> and choose <b>Pair a PC</b> first.');
    return;
  }
  conn?.close();
  const c = conn = new Connection(d, {
    status: (k, t) => { if (conn === c) setStatus(k, t); },
    connected: (hello) => {
      if (conn !== c) return;
      if (!(hello.apps || []).includes(APP)) {
        showNote(`Dubbing isn't switched on on ${esc(hello.name || d.name)}. Start HomeLLM there with <code>-app dubbing=http://127.0.0.1:8000</code>.`);
        readyWaiters.forEach((w) => w.reject(new Error('dubbing is off'))); readyWaiters = [];
        return;
      }
      const go = () => { readyWaiters.forEach((w) => w.resolve()); readyWaiters = []; showStudio(); };
      if (c.httpCh.readyState === 'open') go(); else c.httpCh.addEventListener('open', go, { once: true });
    },
    disconnected: (reason) => {
      if (conn !== c) return;
      setStatus('bad', reason + ' · click to retry');
      readyWaiters.forEach((w) => w.reject(new Error(reason))); readyWaiters = [];
      for (const [n, r] of reqs) { r.port.postMessage({ t: 'error', message: 'connection to the PC lost' }); reqs.delete(n); }
    },
    http: (data) => onRelay(data),
  }, { http: true });
  c.connect();
}

// Responses from the PC.
function onRelay(data) {
  if (typeof data === 'string') {
    let m; try { m = JSON.parse(data); } catch { return; }
    const r = reqs.get(m.n); if (!r) return;
    if (m.type === 'res') r.port.postMessage({ t: 'head', status: m.status, headers: m.headers || {} });
    else if (m.type === 'res_end') { r.port.postMessage({ t: 'end' }); reqs.delete(m.n); }
    else if (m.type === 'res_error') { r.port.postMessage({ t: 'error', message: m.message }); reqs.delete(m.n); }
    return;
  }
  const n = new DataView(data).getUint32(0);
  const r = reqs.get(n); if (!r) return;
  const buf = data.slice(4);
  r.port.postMessage({ t: 'data', buf }, [buf]);
}

function sendJson(m) { conn.httpCh.send(JSON.stringify(m)); }

// A request from the service worker.
async function onWorkerRequest(m, port) {
  try { await ensureConnected(); } catch (e) { port.postMessage({ t: 'error', message: 'not connected to your PC: ' + e.message }); return; }
  const n = nextN++;
  reqs.set(n, { port });
  port.onmessage = ({ data: x }) => {
    if (!reqs.has(n) || !connected()) return;
    if (x.t === 'pull') sendJson({ type: 'credit', n, credit: x.credit });
    else if (x.t === 'cancel') { sendJson({ type: 'cancel', n }); reqs.delete(n); }
  };
  sendJson({ type: 'req', n, app: APP, method: m.method, path: m.path, headers: m.headers, body: !!m.body });
  if (m.body) {
    const bytes = new Uint8Array(m.body), ch = conn.httpCh;
    for (let off = 0; off < bytes.length; off += CHUNK) {
      if (ch.bufferedAmount > 1024 * 1024) await new Promise((r) => { ch.onbufferedamountlow = r; });
      if (!connected()) return;
      const part = bytes.subarray(off, off + CHUNK);
      const frame = new Uint8Array(4 + part.length);
      new DataView(frame.buffer).setUint32(0, n);
      frame.set(part, 4);
      ch.send(frame);
    }
    sendJson({ type: 'req_end', n });
  }
}

function showStudio() {
  $('note').hidden = true;
  const f = $('frame');
  f.hidden = false;
  if (!f.src) f.src = 'app/';
}

async function start() {
  if (!('serviceWorker' in navigator)) { showNote('This browser can\'t run the dubbing studio (no service workers).'); return; }
  try { await idToken(); } catch {
    showNote('You\'re signed out. <a href="/">Sign in on HomeLLM</a>, then come back here.');
    return;
  }
  navigator.serviceWorker.addEventListener('message', (e) => {
    if (e.data?.kind === 'http' && e.ports[0]) onWorkerRequest(e.data, e.ports[0]);
  });
  await navigator.serviceWorker.register('sw.js', { scope: '/dubbing/' });
  await navigator.serviceWorker.ready;
  connect();
}

$('status').onclick = () => { if (!conn || conn.closed) connect(); };
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && conn?.closed) connect();
});
start();
