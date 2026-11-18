'use strict';
// HomeLLM web client: the browser counterpart of the Android app's Home AI. It signs in to the
// same Cognito user pool, pairs with a home PC the same way (a one-time code the PC's owner
// approves), and talks to it over a direct WebRTC connection set up through the same
// signalling server. Everything after setup (prompts, replies, pictures, clips) goes straight
// between this browser and the PC, never through AWS. See HomeLLM/protocol/protocol.go.

const CFG = window.HOMELLM_CONFIG;
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const uid = (p) => `${p}-${crypto.randomUUID().slice(0, 8)}`;
const CHUNK = 16 * 1024;
const MAX_FILE = 64 * 1024 * 1024;

const store = {
  get(k, d) { try { const v = localStorage.getItem('homellm.' + k); return v == null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem('homellm.' + k, JSON.stringify(v)); } catch { } },
};

// ---------------- auth (Cognito SRP, no hosted UI needed) ----------------

const pool = new AmazonCognitoIdentity.CognitoUserPool({ UserPoolId: CFG.userPoolId, ClientId: CFG.clientId });
let pendingChallenge = null; // { user, attrs } while Cognito wants a new password

function idToken() {
  return new Promise((resolve, reject) => {
    const user = pool.getCurrentUser();
    if (!user) return reject(new Error('signed out'));
    // getSession refreshes expired tokens with the refresh token by itself.
    user.getSession((err, session) => {
      if (err || !session?.isValid()) return reject(err || new Error('session expired'));
      resolve(session.getIdToken().getJwtToken());
    });
  });
}

function currentUserName() {
  const u = pool.getCurrentUser();
  if (!u) return '';
  let name = u.getUsername();
  // Usernames are opaque IDs here; show the email from the ID token when there is one.
  u.getSession((err, session) => { if (!err && session) name = session.getIdToken().payload.email || name; });
  return name;
}

async function signIn(e) {
  e.preventDefault();
  $('signinErr').textContent = '';
  $('signinBtn').disabled = true;
  try {
    if (pendingChallenge) {
      await new Promise((resolve, reject) => pendingChallenge.user.completeNewPasswordChallenge($('newPassword').value, {}, {
        onSuccess: resolve, onFailure: reject }));
      pendingChallenge = null;
    } else {
      const user = new AmazonCognitoIdentity.CognitoUser({ Username: $('username').value.trim(), Pool: pool });
      const details = new AmazonCognitoIdentity.AuthenticationDetails({ Username: $('username').value.trim(), Password: $('password').value });
      const needsNew = await new Promise((resolve, reject) => user.authenticateUser(details, {
        onSuccess: () => resolve(false),
        onFailure: reject,
        newPasswordRequired: (attrs) => { pendingChallenge = { user, attrs }; resolve(true); },
      }));
      if (needsNew) {
        $('newPwBox').hidden = false;
        $('signinErr').textContent = 'Choose a new password to finish signing in.';
        return;
      }
    }
    $('password').value = '';
    showApp();
  } catch (err) {
    $('signinErr').textContent = err.message || String(err);
  } finally {
    $('signinBtn').disabled = false;
  }
}

function signOut() {
  conn?.close();
  conn = null;
  pool.getCurrentUser()?.signOut();
  $('app').hidden = true;
  $('signin').hidden = false;
}

// ---------------- signalling ----------------

// openSignal opens the signalling WebSocket as a "phone". Browsers can't set headers on a
// WebSocket, so the ID token goes in the idToken query parameter, which lambda-homellm accepts.
async function openSignal(url, onMessage) {
  const token = await idToken();
  const ws = new WebSocket(`${url}${url.includes('?') ? '&' : '?'}role=phone&idToken=${encodeURIComponent(token)}`);
  await new Promise((resolve, reject) => {
    ws.onopen = resolve;
    ws.onerror = () => reject(new Error('the signalling server refused the connection (try signing out and in again)'));
  });
  ws.onmessage = (ev) => { let m; try { m = JSON.parse(ev.data); } catch { return; } onMessage(m); };
  return ws;
}

// signalRoundTrip sends one message and waits for the reply action.
async function signalRoundTrip(url, msg, replyAction, timeoutMs) {
  let done;
  const result = new Promise((resolve, reject) => { done = { resolve, reject }; });
  const ws = await openSignal(url, (m) => {
    if (m.action === replyAction) done.resolve(m);
    else if (m.action === 'error') done.reject(new Error(m.message || 'signalling error'));
  });
  ws.onclose = () => done.reject(new Error('signalling connection closed'));
  ws.send(JSON.stringify(msg));
  const timer = setTimeout(() => done.reject(new Error('timed out')), timeoutMs);
  try { return await result; } finally { clearTimeout(timer); ws.onclose = null; ws.close(); }
}

// ---------------- paired PCs ----------------

// Each PC's public key comes only from its pairing text, so it's kept here. The signalling
// server's own device list says which PCs this account may use and whether they're online.
let keys = store.get('keys', {});   // device_id -> { name, pub, signal }
let devices = [];                   // [{ device_id, name, online, pub? }]
let selectedId = store.get('selected', '');

async function refreshDevices() {
  try {
    const r = await signalRoundTrip(CFG.signalUrl, { action: 'devices' }, 'devices', 10000);
    devices = (r.devices || []).map((d) => ({ ...d, pub: keys[d.device_id]?.pub }));
  } catch (e) {
    devices = Object.entries(keys).map(([id, k]) => ({ device_id: id, name: k.name, online: false, pub: k.pub }));
    setStatus('bad', 'Could not list your PCs: ' + e.message);
  }
  // Keys for PCs this account can no longer use are just dropped from the list.
  const usable = devices.filter((d) => d.pub);
  if (!usable.some((d) => d.device_id === selectedId)) selectedId = usable.find((d) => d.online)?.device_id || usable[0]?.device_id || '';
  store.set('selected', selectedId);
  renderDevices();
}

function renderDevices() {
  const sel = $('deviceSelect');
  sel.innerHTML = devices.length
    ? devices.map((d) => `<option value="${esc(d.device_id)}" ${d.pub ? '' : 'disabled'}>${d.online ? '● ' : '○ '}${esc(d.name || d.device_id)}${d.pub ? '' : ' (pair this browser)'}</option>`).join('')
    : '<option value="">no PCs paired</option>';
  sel.value = selectedId;
  renderEmpty();
}

// Typed pairing codes (HomeLLM's protocol/paircode.go): Crockford base32, 10 characters.
function normalizeCode(s) {
  const t = s.toUpperCase().replace(/[\s-]/g, '').replace(/O/g, '0').replace(/[IL]/g, '1');
  return /^[0-9ABCDEFGHJKMNPQRSTVWXYZ]{10}$/.test(t) ? t : '';
}

async function codeLookup(code) {
  return 'pc2-' + hex(await sha256(new TextEncoder().encode('homellm-pair-code-v2\0' + code)));
}

// Checks the PC's public key against the code: HMAC over it keyed by PBKDF2(code). Only the
// real PC (which made the code) can produce it, so signalling can't swap in another key.
async function verifyKeyProof(code, deviceId, proof) {
  const enc = new TextEncoder();
  const pub = unb64(proof.pub || '');
  if (pub.length !== 32) return null;
  const base = await crypto.subtle.importKey('raw', enc.encode(code), 'PBKDF2', false, ['deriveBits']);
  const keyBits = await crypto.subtle.deriveBits({ name: 'PBKDF2', hash: 'SHA-256', salt: enc.encode('homellm-pair-proof-v2|' + deviceId), iterations: 100000 }, base, 256);
  const key = await crypto.subtle.importKey('raw', keyBits, { name: 'HMAC', hash: 'SHA-256' }, false, ['verify']);
  const head = enc.encode('homellm-pair-key-v2\0' + deviceId + '\0');
  const msg = new Uint8Array(head.length + pub.length);
  msg.set(head); msg.set(pub, head.length);
  return (await crypto.subtle.verify('HMAC', key, unb64(proof.proof || ''), msg)) ? proof.pub : null;
}

function parsePairing(text) {
  const t = text.trim();
  const json = t.slice(t.indexOf('{'), t.lastIndexOf('}') + 1);
  let q;
  try { q = JSON.parse(json); } catch { throw new Error("That doesn't look like the pairing text. Copy the whole line starting with {\"v\":"); }
  if (!q.device_id || !q.code || !q.pub || !q.signal) throw new Error('The pairing text is missing something. Run homellm.exe pair again and copy the whole line.');
  try { if (unb64(q.pub).length !== 32) throw 0; } catch { throw new Error('Bad public key in the pairing text.'); }
  return q;
}

async function pairNow() {
  $('pairStatus').textContent = '';
  const raw = $('pairText').value;
  // The QR's JSON still works too (e.g. pasted from `homellm pair`), with its approval prompt.
  if (raw.trim().startsWith('{')) return pairWithQrText(raw);
  const code = normalizeCode(raw);
  if (!code) { $('pairStatus').textContent = 'A pairing code is 10 letters and digits, like 7KQM2-XRD4P.'; return; }
  $('pairGo').disabled = true;
  $('pairStatus').textContent = 'Pairing…';
  try {
    const r = await signalRoundTrip(CFG.signalUrl, { action: 'pair', code: await codeLookup(code) }, 'pair_result', 30000);
    if (!r.ok) throw new Error(r.message === 'invalid or expired pairing code' ? "that code isn't valid (codes work once, for 10 minutes)" : r.message || 'pairing failed');
    const pub = r.payload && await verifyKeyProof(code, r.device_id, r.payload);
    if (!pub) throw new Error("security check failed: the PC's key didn't match the code");
    await paired(r.device_id, r.name, pub, CFG.signalUrl);
  } catch (e) {
    $('pairStatus').textContent = 'Pairing failed: ' + e.message;
  } finally {
    $('pairGo').disabled = false;
  }
}

async function paired(deviceId, name, pub, signal) {
  keys[deviceId] = { name, pub, signal };
  store.set('keys', keys);
  selectedId = deviceId;
  store.set('selected', selectedId);
  $('pairDialog').close();
  $('pairText').value = '';
  await refreshDevices();
  connect();
}

async function pairWithQrText(text) {
  let q;
  try { q = parsePairing(text); } catch (e) { $('pairStatus').textContent = e.message; return; }
  $('pairGo').disabled = true;
  $('pairStatus').textContent = `Waiting for approval: answer “y” in the terminal on ${q.name || 'the PC'}…`;
  try {
    const r = await signalRoundTrip(q.signal, { action: 'pair', device_id: q.device_id, code: q.code }, 'pair_result', 100000);
    if (!r.ok) throw new Error(r.message || 'pairing failed');
    keys[q.device_id] = { name: r.name || q.name, pub: q.pub, signal: q.signal };
    store.set('keys', keys);
    selectedId = q.device_id;
    store.set('selected', selectedId);
    $('pairDialog').close();
    $('pairText').value = '';
    await refreshDevices();
    connect();
  } catch (e) {
    $('pairStatus').textContent = 'Pairing failed: ' + e.message;
  } finally {
    $('pairGo').disabled = false;
  }
}

// ---------------- the WebRTC connection ----------------

function b64(bytes) { let s = ''; for (const b of bytes) s += String.fromCharCode(b); return btoa(s); }
function unb64(s) { return Uint8Array.from(atob(s), (c) => c.charCodeAt(0)); }
async function sha256(data) { return new Uint8Array(await crypto.subtle.digest('SHA-256', data)); }
const hex = (bytes) => [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');

// What the PC signs for its answer: binds it to our offer (and so to our DTLS fingerprint).
async function answerSigningBytes(offerSdp, answerSdp) {
  const enc = new TextEncoder();
  const o = await sha256(enc.encode(offerSdp)), a = await sha256(enc.encode(answerSdp));
  const prefix = enc.encode('homellm-answer-v1\0');
  const out = new Uint8Array(prefix.length + 64);
  out.set(prefix); out.set(o, prefix.length); out.set(a, prefix.length + 32);
  return out;
}

class Connection {
  constructor(device, on) {
    this.device = device;
    this.on = on;           // handlers, see App below
    this.session = crypto.randomUUID();
    this.ready = false;
    this.closed = false;
    this.sentIds = new Set();
    this.fileQueue = Promise.resolve();
    this.incoming = null;   // { start, parts, size }
    this.files = new Map(); // file_id -> Blob
    this.done = new Map();  // file_id -> image_done / video_done
  }

  async connect() {
    this.on.status('busy', `Contacting ${this.device.name}…`);
    const timer = setTimeout(() => { if (!this.ready) this.fail(`timed out connecting to ${this.device.name}`); }, 30000);
    try {
      this.ws = await openSignal(this.device.signal || CFG.signalUrl, (m) => this.onSignal(m));
      this.ws.onclose = () => { if (!this.ready) this.fail('signalling connection closed'); };
      const pc = this.pc = new RTCPeerConnection({ iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] });
      this.chat = pc.createDataChannel('chat');
      this.filesCh = pc.createDataChannel('files');
      this.filesCh.binaryType = 'arraybuffer';
      this.filesCh.bufferedAmountLowThreshold = 256 * 1024;
      this.chat.onopen = () => this.onChatOpen();
      this.chat.onclose = () => { if (this.ready) this.fail('connection closed'); };
      this.chat.onmessage = (ev) => this.onPeer(ev.data);
      this.filesCh.onmessage = (ev) => this.onFile(ev.data);
      pc.onconnectionstatechange = () => {
        const s = pc.connectionState;
        if (s === 'failed') this.fail(this.ready ? 'connection lost' : `no network path to ${this.device.name}`);
        else if (s === 'disconnected' && this.ready) this.on.status('busy', 'Connection interrupted, waiting…');
        else if (s === 'connected' && this.ready && this.path) this.on.status('ok', this.label());
      };
      await pc.setLocalDescription(await pc.createOffer());
      this.on.status('busy', 'Finding a network path…');
      // Non-trickle ICE: one complete offer after gathering (or after 10 s, with what we have).
      await new Promise((resolve) => {
        if (pc.iceGatheringState === 'complete') return resolve();
        pc.onicegatheringstatechange = () => { if (pc.iceGatheringState === 'complete') resolve(); };
        setTimeout(resolve, 10000);
      });
      if (this.closed) return;
      this.offerSdp = pc.localDescription.sdp;
      this.ws.send(JSON.stringify({ action: 'signal', device_id: this.device.device_id, session: this.session,
        payload: { type: 'offer', sdp: this.offerSdp } }));
    } catch (e) {
      this.fail(e.message);
    } finally {
      if (this.closed || this.ready) clearTimeout(timer);
    }
  }

  async onSignal(m) {
    if (this.closed) return;
    if (m.action === 'error' && (!m.session || m.session === this.session)) return this.fail(m.message || 'signalling error');
    if (m.action !== 'signal' || m.session !== this.session || m.payload?.type !== 'answer') return;
    const answer = m.payload.sdp;
    let ok = false;
    try {
      ok = nacl.sign.detached.verify(await answerSigningBytes(this.offerSdp, answer), unb64(m.payload.sig || ''), unb64(this.device.pub));
    } catch { ok = false; }
    // An answer not signed by the PC's key could come from a compromised signalling server
    // trying to sit in the middle. Never connect to it.
    if (!ok) return this.fail(`security check failed: the answer was not signed by ${this.device.name}`);
    this.on.status('busy', 'Connecting directly…');
    try { await this.pc.setRemoteDescription({ type: 'answer', sdp: answer }); } catch (e) { this.fail('bad answer: ' + e.message); }
  }

  onChatOpen() {
    if (this.closed || this.ready) return;
    this.ready = true;
    // Negotiation is over: nothing goes through the signalling server from here on.
    this.ws.onclose = null;
    this.ws.close();
    this.send({ type: 'hello' });
  }

  async detectPath() {
    try {
      const stats = await this.pc.getStats();
      let pair;
      stats.forEach((s) => { if (s.type === 'candidate-pair' && s.nominated && s.state === 'succeeded') pair = s; });
      const types = [stats.get(pair?.localCandidateId)?.candidateType, stats.get(pair?.remoteCandidateId)?.candidateType];
      return types.includes(undefined) ? '' : types.includes('relay') ? 'relayed' : 'direct';
    } catch { return ''; }
  }

  label() { return `${this.hello.name || this.device.name} · ${this.hello.model || ''}${this.path ? ' · ' + this.path : ''}`; }

  async onPeer(text) {
    let m; try { m = JSON.parse(text); } catch { return; }
    const id = m.id;
    switch (m.type) {
      case 'hello':
        this.hello = m;
        this.path = await this.detectPath();
        this.on.connected(m);
        this.on.status('ok', this.label());
        break;
      case 'chunk': this.on.chunk(id, m.delta || ''); break;
      case 'done': this.on.done(id); break;
      case 'error': case 'file_error': this.on.error(id, m.message || 'failed'); break;
      case 'image_prompt': this.on.prompt(id, m.prompt); break;
      case 'image_progress': this.on.progress(id, m.step, m.steps, m.stage || ''); break;
      case 'image_done': case 'video_done':
        this.done.set(m.file_id, m); this.deliver(m.file_id); break;
    }
  }

  async onFile(data) {
    if (typeof data === 'string') {
      let m; try { m = JSON.parse(data); } catch { return; }
      if (m.type === 'file_start') {
        this.incoming = m.size > 0 && m.size <= MAX_FILE ? { start: m, parts: [], size: 0 } : null;
      } else if (m.type === 'file_end') {
        const inc = this.incoming; this.incoming = null;
        if (!inc || inc.start.id !== m.id) return;
        const blob = new Blob(inc.parts, { type: inc.start.mime });
        const sum = hex(await sha256(await blob.arrayBuffer()));
        if (blob.size !== inc.start.size || sum !== String(inc.start.sha256).toLowerCase()) return; // corrupted: drop
        this.files.set(m.id, blob);
        this.deliver(m.id);
      } else if (m.type === 'file_error') {
        this.on.error(m.id, m.message || 'file failed');
      }
      return;
    }
    const inc = this.incoming;
    if (!inc) return;
    inc.size += data.byteLength;
    if (inc.size > MAX_FILE) { this.incoming = null; return; }
    inc.parts.push(data);
  }

  // A result is complete once both its *_done message and its bytes are here, in either order.
  deliver(fileId) {
    const d = this.done.get(fileId), blob = this.files.get(fileId);
    if (!d || !blob) return;
    this.done.delete(fileId); this.files.delete(fileId);
    if (d.type === 'video_done') this.on.video(d.id, blob, d);
    else this.on.image(d.id, blob, d);
  }

  send(obj) {
    if (!this.ready || this.chat.readyState !== 'open') return false;
    this.chat.send(JSON.stringify(obj));
    return true;
  }

  // sendFile queues a picture or clip on the files channel and returns its id at once: the PC
  // waits for the file when a request refers to it.
  sendFile(blob) {
    const id = uid('img');
    this.sentIds.add(id);
    this.fileQueue = this.fileQueue.then(async () => {
      const buf = new Uint8Array(await blob.arrayBuffer());
      const ch = this.filesCh;
      if (this.closed) return;
      ch.send(JSON.stringify({ type: 'file_start', id, mime: blob.type || 'image/jpeg', size: buf.length, sha256: hex(await sha256(buf)) }));
      for (let off = 0; off < buf.length; off += CHUNK) {
        if (ch.bufferedAmount > 1024 * 1024) await new Promise((r) => { ch.onbufferedamountlow = r; });
        if (this.closed) return;
        ch.send(buf.subarray(off, off + CHUNK));
      }
      ch.send(JSON.stringify({ type: 'file_end', id }));
    }).catch(() => { });
    return id;
  }

  fail(reason) {
    if (this.closed) return;
    this.closed = true;
    this.teardown();
    this.on.disconnected(reason);
  }

  close() { this.closed = true; this.teardown(); }

  teardown() {
    try { this.ws && (this.ws.onclose = null, this.ws.close()); } catch { }
    try { this.pc?.close(); } catch { }
  }
}

// ---------------- app ----------------

let conn = null;
let caps = null;                 // the PC's hello
let mode = store.get('mode', 'chat');
let attachment = null;           // { blob, url }
let history = [];                // chat turns: { role, content, imageIds }
const jobs = new Map();          // request id -> { el, kind, ... }
let chatReq = null;              // the chat reply being streamed: { id, el, text }
let waiters = [];                // resolve fns waiting for a connection

function setStatus(kind, text) {
  $('status').querySelector('.dot').className = 'dot ' + kind;
  $('statusText').textContent = text;
  $('status').title = kind === 'bad' ? 'Click to reconnect' : '';
}

function ensureConnected() {
  if (conn?.ready && caps) return Promise.resolve(conn);
  if (!conn || conn.closed) connect();
  return new Promise((resolve, reject) => waiters.push({ resolve, reject }));
}

function connect() {
  conn?.close();
  caps = null;
  const d = devices.find((x) => x.device_id === selectedId && x.pub);
  if (!d) { setStatus('', devices.length ? 'pair this browser with a PC' : 'no PCs paired'); return; }
  const k = keys[d.device_id];
  const c = conn = new Connection({ ...d, signal: k?.signal }, handlers(() => conn === c));
  c.connect();
}

function handlers(isCurrent) {
  const guard = (fn) => (...a) => { if (isCurrent()) fn(...a); };
  return {
    status: guard(setStatus),
    connected: guard((hello) => {
      caps = hello;
      applyMode();
      waiters.forEach((w) => w.resolve(conn)); waiters = [];
    }),
    disconnected: guard((reason) => {
      caps = null;
      setStatus('bad', reason + ' · click to retry');
      waiters.forEach((w) => w.reject(new Error(reason))); waiters = [];
      if (chatReq) { finishChat('(connection lost)'); }
      for (const [id, j] of jobs) if (j.kind !== 'video' && !j.finished) jobFail(id, 'connection lost');
    }),
    chunk: guard((id, delta) => { if (chatReq?.id === id) { chatReq.text += delta; renderBubble(chatReq.el, chatReq.text, true); scrollDown(); } }),
    done: guard((id) => {
      if (chatReq?.id === id) return finishChat();
      if (jobs.has(id)) jobDone(id, null); // a cancelled image ends with done
    }),
    error: guard((id, message) => {
      if (chatReq?.id === id) return finishChat(null, message);
      if (jobs.has(id)) jobFail(id, message);
    }),
    prompt: guard((id, p) => { const j = jobs.get(id); if (j) { j.el.querySelector('.used').textContent = 'Prompt used: ' + p; j.el.querySelector('.used').hidden = false; } }),
    progress: guard((id, step, steps, stage) => jobProgress(id, step, steps, stage)),
    image: guard((id, blob, d) => jobDone(id, blob, d)),
    video: guard((id, blob, d) => {
      if (!jobs.has(id)) addJob(id, 'video', '(finished while you were away)', null);
      jobDone(id, blob, d);
    }),
  };
}

// ----- feed rendering -----

function scrollDown() { const f = $('feed'); f.scrollTop = f.scrollHeight; }

function renderEmpty() {
  const feed = $('feed');
  if (feed.querySelector('.msg, .job')) { feed.querySelector('.empty')?.remove(); return; }
  const paired = devices.some((d) => d.pub);
  feed.innerHTML = `<div class="empty">
    <h2>${paired ? 'Your home AI, from anywhere' : 'Pair this browser with your PC'}</h2>
    <p>${paired ? 'Chat, make pictures, turn photos into short videos, or restyle them. Everything runs on your own PC and travels directly between it and this browser.'
      : 'Get a pairing code on the PC (the <b>HomeLLM pairing code</b> shortcut on its desktop, or <b>Pairing code</b> in HomeLLM Studio), then choose <b>Pair a PC</b> and type it in.'}</p>
    ${paired ? '' : '<button class="primary" data-act="pair">Pair a PC</button>'}</div>`;
  feed.querySelector('[data-act=pair]')?.addEventListener('click', openPair);
}

function md(text) {
  // Minimal, safe formatting for LLM replies: code blocks, inline code, bold, paragraphs.
  const parts = String(text).split(/```/);
  return parts.map((p, i) => {
    if (i % 2) return `<pre><code>${esc(p.replace(/^[a-z0-9+-]*\n/i, ''))}</code></pre>`;
    return esc(p).replace(/`([^`]+)`/g, '<code>$1</code>').replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>')
      .split(/\n{2,}/).map((para) => para.trim() ? `<p>${para.replace(/\n/g, '<br>')}</p>` : '').join('');
  }).join('');
}

function renderBubble(el, text, typing) {
  el.innerHTML = md(text) || '';
  el.classList.toggle('typing', !!typing);
}

function addMsg(role, text, imgUrl) {
  renderEmpty();
  const wrap = document.createElement('div');
  wrap.className = 'msg ' + (role === 'user' ? 'me' : 'ai');
  const b = document.createElement('div');
  b.className = 'bubble';
  if (imgUrl) b.innerHTML = `<img class="thumb" src="${imgUrl}">`;
  const t = document.createElement('div');
  b.appendChild(t);
  renderBubble(t, text);
  wrap.appendChild(b);
  $('feed').appendChild(wrap);
  $('feed').querySelector('.empty')?.remove();
  scrollDown();
  return t;
}

const KIND_NAMES = { image: 'Picture', video: 'Video', restyle: 'Restyle' };

function addJob(id, kind, prompt, srcUrl) {
  $('feed').querySelector('.empty')?.remove();
  const el = document.createElement('div');
  el.className = 'job';
  el.innerHTML = `<div class="head">${srcUrl ? `<img class="src" src="${srcUrl}">` : ''}
      <div class="text"><div class="kind">${KIND_NAMES[kind]}</div><div class="prompt">${esc(prompt)}</div><div class="used" hidden></div></div>
      <button class="ghost small" data-act="cancel">Cancel</button></div>
    <div class="progress"><span class="ptext">Sending to your PC…</span><div class="bar indet"><div></div></div></div>`;
  $('feed').appendChild(el);
  const job = { el, kind, prompt, started: Date.now() };
  jobs.set(id, job);
  el.querySelector('[data-act=cancel]').onclick = () => { conn?.send({ type: 'cancel', id }); jobFail(id, 'Cancelled'); };
  scrollDown();
  return job;
}

function jobProgress(id, step, steps, stage) {
  const j = jobs.get(id); if (!j) return;
  const p = j.el.querySelector('.progress');
  const bar = p.querySelector('.bar');
  let text;
  if (stage === 'sound') text = 'Adding a soundtrack…';
  else if (stage === 'music') text = 'Adding music…';
  else if (!step) text = 'Loading the model on your PC…';
  else text = `Step ${step} of ${steps}`;
  p.querySelector('.ptext').textContent = `${text} · ${Math.round((Date.now() - j.started) / 1000)}s`;
  const indet = !step || stage;
  bar.classList.toggle('indet', !!indet);
  bar.firstElementChild.style.width = indet ? '' : `${Math.round(step / steps * 100)}%`;
}

function jobFail(id, message) {
  const j = jobs.get(id); if (!j || j.finished) return;
  j.finished = true;
  j.el.classList.add('failed');
  j.el.querySelector('[data-act=cancel]')?.remove();
  j.el.querySelector('.progress').innerHTML = esc(message);
}

function jobDone(id, blob, d) {
  const j = jobs.get(id); if (!j) return;
  if (!blob) return jobFail(id, 'Stopped');
  j.finished = true;
  j.el.querySelector('[data-act=cancel]')?.remove();
  const url = URL.createObjectURL(blob);
  const isVideo = blob.type.startsWith('video/') || d?.type === 'video_done';
  const secs = Math.round((Date.now() - j.started) / 1000);
  const name = `homellm-${id}.${isVideo ? 'mp4' : 'jpg'}`;
  j.el.querySelector('.progress').outerHTML = `<div class="media">${isVideo
    ? `<video src="${url}" controls autoplay loop muted playsinline></video>`
    : `<img src="${url}" alt="">`}</div>
    <div class="actions">
      <span class="muted small" style="align-self:center">${d?.width ? `${d.width}×${d.height}` : ''}${secs > 1 ? ` · ${secs}s` : ''}${d?.seed ? ` · seed ${d.seed}` : ''}</span>
      <div class="spacer"></div>
      ${!isVideo && caps?.videos ? '<button class="ghost small" data-act="animate">Make a video</button>' : ''}
      ${!isVideo && caps?.restyle ? '<button class="ghost small" data-act="restyle">Restyle</button>' : ''}
      <a href="${url}" download="${name}"><button class="ghost small">Download</button></a>
    </div>${d?.message ? `<div class="used" style="padding:0 14px 10px">${esc(d.message)}</div>` : ''}`;
  j.el.querySelectorAll('[data-act=animate], [data-act=restyle]').forEach((b) => b.onclick = async () => {
    // Mode first: picture mode takes no attachment and would drop it.
    setMode(b.dataset.act === 'animate' ? 'video' : 'restyle');
    await setAttachment(blob);
    $('input').focus();
  });
  scrollDown();
}

// ----- composer -----

const STYLES = ['cartoon', 'anime', 'watercolour painting', '3D animated film', 'oil painting', 'pencil sketch', 'pixel art', 'comic book'];

function setMode(m) { mode = m; store.set('mode', m); applyMode(); }

function applyMode() {
  document.querySelectorAll('#tabs button').forEach((b) => {
    b.classList.toggle('on', b.dataset.mode === mode);
    const supported = !caps || b.dataset.mode === 'chat' || caps[{ image: 'images', video: 'videos', restyle: 'restyle' }[b.dataset.mode]];
    b.classList.toggle('off', !supported);
    b.title = supported ? '' : 'Your PC has this switched off';
  });
  const o = $('opts');
  const input = $('input');
  if (mode === 'chat') {
    o.innerHTML = '';
    input.placeholder = caps?.vision ? 'Message your home AI (you can add a picture)…' : 'Message your home AI…';
    $('sendBtn').textContent = chatReq ? 'Stop' : 'Send';
  } else if (mode === 'image') {
    o.innerHTML = `<label>Size <select id="optSize"><option value="1024x1024">Square</option><option value="1216x832">Landscape</option><option value="832x1216">Portrait</option></select></label>
      <label><input type="checkbox" id="optEnhance" checked> Improve my prompt</label>`;
    input.placeholder = 'Describe a picture, e.g. a red fox in fresh snow at dawn';
    $('sendBtn').textContent = 'Create';
  } else if (mode === 'video') {
    o.innerHTML = `<label>Length <select id="optFrames"><option value="33">2 s</option><option value="49" selected>3 s</option><option value="81">5 s</option></select></label>
      ${caps?.sound ? '<label><input type="checkbox" id="optSound"> Add sound</label>' : ''}
      ${caps?.music ? '<label><input type="checkbox" id="optMusic"> Add music</label>' : ''}
      <span class="muted">${attachment ? '' : 'Add a start picture with ＋ (or drop / paste one).'}</span>`;
    input.placeholder = 'Describe the motion, e.g. the fox turns its head, camera slowly pushes in';
    $('sendBtn').textContent = 'Animate';
  } else {
    o.innerHTML = `<div class="chips">${STYLES.map((s) => `<button type="button" class="ghost">${s}</button>`).join('')}</div>
      <label><input type="checkbox" id="optEnhance"> Improve my style text</label>
      <span class="muted">${attachment ? '' : 'Add a photo with ＋ (or drop / paste one).'}</span>`;
    o.querySelectorAll('.chips button').forEach((b) => b.onclick = () => { input.value = b.textContent; });
    input.placeholder = 'Style, e.g. watercolour painting (empty = cartoon)';
    $('sendBtn').textContent = 'Restyle';
  }
  const canAttach = mode !== 'image' && (mode !== 'chat' || caps?.vision !== false);
  $('attachBtn').hidden = !canAttach;
  if (!canAttach && attachment) clearAttachment();
}

// Pictures are re-encoded as JPEG (at most 1600 px on the long side) before sending, like the
// phone does: smaller transfers, and the PC's models don't need more.
async function toJpeg(file) {
  const img = await createImageBitmap(file);
  const scale = Math.min(1, 1600 / Math.max(img.width, img.height));
  const c = document.createElement('canvas');
  c.width = Math.round(img.width * scale); c.height = Math.round(img.height * scale);
  c.getContext('2d').drawImage(img, 0, 0, c.width, c.height);
  return new Promise((r) => c.toBlob(r, 'image/jpeg', 0.9));
}

async function setAttachment(file) {
  try {
    const blob = file.type === 'image/jpeg' && file.size < 3e6 ? file : await toJpeg(file);
    if (attachment) URL.revokeObjectURL(attachment.url);
    attachment = { blob, url: URL.createObjectURL(blob) };
    $('attachImg').src = attachment.url;
    $('attachText').textContent = mode === 'video' ? 'Start picture' : mode === 'restyle' ? 'Photo to restyle' : 'Picture';
    $('attachRow').hidden = false;
    applyMode();
  } catch { alert("Couldn't read that picture."); }
}

function clearAttachment() {
  if (attachment) URL.revokeObjectURL(attachment.url);
  attachment = null;
  $('attachRow').hidden = true;
  applyMode();
}

function finishChat(note, error) {
  const r = chatReq; if (!r) return;
  chatReq = null;
  if (error) { r.el.parentElement.classList.add('err'); r.text = r.text ? r.text + '\n\n' + error : error; }
  else if (note) r.text += (r.text ? '\n\n' : '') + note;
  renderBubble(r.el, r.text);
  if (!error && r.text) history.push({ role: 'assistant', content: r.text });
  applyMode();
}

async function send() {
  if (mode === 'chat' && chatReq) { conn?.send({ type: 'cancel', id: chatReq.id }); return finishChat('(stopped)'); }
  const text = $('input').value.trim();
  if (mode === 'chat' && !text && !attachment) return;
  if (mode === 'image' && !text) return $('input').focus();
  if ((mode === 'video' || mode === 'restyle') && !attachment) return $('attachBtn').click();
  if (mode === 'video' && !text) return $('input').focus();

  let c;
  try { c = await ensureConnected(); } catch (e) { return alert('Not connected: ' + e.message); }
  const supported = mode === 'chat' || caps[{ image: 'images', video: 'videos', restyle: 'restyle' }[mode]];
  if (!supported) return alert(`${caps.name || 'Your PC'} doesn't have ${mode} switched on.`);

  const att = attachment;
  $('input').value = '';
  autosize();

  if (mode === 'chat') {
    const imageIds = att ? [c.sendFile(att.blob)] : [];
    addMsg('user', text, att?.url);
    history.push({ role: 'user', content: text, imageIds });
    if (att) { attachment = null; $('attachRow').hidden = true; }
    const id = crypto.randomUUID();
    const messages = history.map((t) => {
      const ids = (t.imageIds || []).filter((x) => c.sentIds.has(x));
      return ids.length ? { role: t.role, content: t.content, image_ids: ids } : { role: t.role, content: t.content };
    });
    chatReq = { id, el: addMsg('assistant', ''), text: '' };
    chatReq.el.classList.add('typing');
    if (!c.send({ type: 'request', id, messages })) finishChat(null, 'not connected');
    applyMode();
    return;
  }
  if (mode === 'image') {
    const [w, h] = ($('optSize')?.value || '1024x1024').split('x').map(Number);
    const id = uid('imagine');
    addJob(id, 'image', text, null);
    c.send({ type: 'imagine', id, prompt: text, enhance: !!$('optEnhance')?.checked, width: w, height: h });
    return;
  }
  const imageId = c.sendFile(att.blob);
  if (mode === 'video') {
    const id = uid('animate');
    addJob(id, 'video', text, att.url);
    c.send({ type: 'animate', id, image_id: imageId, prompt: text, frames: Number($('optFrames')?.value || 49),
      sound: !!$('optSound')?.checked, music: !!$('optMusic')?.checked });
  } else {
    const id = uid('restyle');
    addJob(id, 'restyle', text || 'cartoon', att.url);
    const msg = { type: 'restyle', id, image_id: imageId };
    if (text) { msg.prompt = text; msg.enhance = !!$('optEnhance')?.checked; }
    c.send(msg);
  }
}

function autosize() { const t = $('input'); t.style.height = 'auto'; t.style.height = Math.min(t.scrollHeight, 200) + 'px'; }

function openPair() { $('pairStatus').textContent = ''; $('pairDialog').showModal(); $('pairText').focus(); }

async function showApp() {
  $('signin').hidden = true;
  $('app').hidden = false;
  $('who').textContent = currentUserName();
  applyMode();
  setStatus('busy', 'Finding your PCs…');
  await refreshDevices();
  connect();
}

function wire() {
  $('signinForm').addEventListener('submit', signIn);
  $('signoutBtn').onclick = signOut;
  $('pairBtn').onclick = openPair;
  $('pairCancel').onclick = () => $('pairDialog').close();
  $('pairGo').onclick = pairNow;
  $('pairText').addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); pairNow(); } });
  $('deviceSelect').onchange = () => { selectedId = $('deviceSelect').value; store.set('selected', selectedId); connect(); };
  $('status').onclick = () => { if (!conn || conn.closed) { refreshDevices().then(connect); } };
  $('tabs').onclick = (e) => { const b = e.target.closest('button'); if (b) setMode(b.dataset.mode); };
  $('sendBtn').onclick = send;
  $('input').addEventListener('input', autosize);
  $('input').addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); } });
  $('attachBtn').onclick = () => $('fileInput').click();
  $('fileInput').onchange = () => { const f = $('fileInput').files[0]; if (f) setAttachment(f); $('fileInput').value = ''; };
  $('attachClear').onclick = clearAttachment;
  document.addEventListener('paste', (e) => {
    const it = [...(e.clipboardData?.items || [])].find((x) => x.type.startsWith('image/'));
    if (it && !$('app').hidden && !$('pairDialog').open) { e.preventDefault(); if (mode === 'image') setMode('video'); setAttachment(it.getAsFile()); }
  });
  const feed = $('feed');
  feed.addEventListener('dragover', (e) => { e.preventDefault(); feed.classList.add('drag'); });
  feed.addEventListener('dragleave', () => feed.classList.remove('drag'));
  feed.addEventListener('drop', (e) => {
    e.preventDefault(); feed.classList.remove('drag');
    const f = [...e.dataTransfer.files].find((x) => x.type.startsWith('image/'));
    if (f) { if (mode === 'image') setMode('video'); setAttachment(f); }
  });
  // Coming back to the tab after the PC dropped the link: reconnect quietly.
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && !$('app').hidden && conn?.closed) connect();
  });
}

wire();
if (pool.getCurrentUser()) idToken().then(showApp, () => { $('signin').hidden = false; });
else $('signin').hidden = false;
