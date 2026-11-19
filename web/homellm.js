'use strict';
// HomeLLM web client, shared part: sign-in, signalling and the WebRTC connection to the PC.
// Used by the main page (app.js) and the /dubbing/ bridge. The browser counterpart of the Android app's Home AI. It signs in to the
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


// ---------------- signalling ----------------

// openSignal opens the signalling WebSocket as a "phone". Browsers can't set headers on a
// WebSocket, so the ID token goes in the idToken query parameter, which signal-lambda accepts.
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
  // opts.http also opens the web-app channel (protocol/httprelay.go); its messages go to
  // on.http(data).
  constructor(device, on, opts = {}) {
    this.device = device;
    this.on = on;           // handlers: status, connected, disconnected, chunk, done, error, ...
    this.opts = opts;
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
      if (this.opts.http) {
        this.httpCh = pc.createDataChannel('http');
        this.httpCh.binaryType = 'arraybuffer';
        this.httpCh.bufferedAmountLowThreshold = 256 * 1024;
        this.httpCh.onmessage = (ev) => this.on.http?.(ev.data);
      }
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
      case 'chunk': this.on.chunk?.(id, m.delta || ''); break;
      case 'done': this.on.done?.(id); break;
      case 'error': case 'file_error': this.on.error?.(id, m.message || 'failed'); break;
      case 'image_prompt': this.on.prompt?.(id, m.prompt); break;
      case 'image_progress': this.on.progress?.(id, m.step, m.steps, m.stage || ''); break;
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
        this.on.error?.(m.id, m.message || 'file failed');
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
    if (d.type === 'video_done') this.on.video?.(d.id, blob, d);
    else this.on.image?.(d.id, blob, d);
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
