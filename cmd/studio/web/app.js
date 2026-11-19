'use strict';
// HomeLLM Studio: builds ComfyUI workflows in the browser, queues them through the studio's
// proxy (/comfy → the home PC's ComfyUI over SSH), follows their progress over ComfyUI's
// WebSocket and asks the studio to save each finished run with its settings.

const $ = (id) => document.getElementById(id);

const MODELS = {
  i2v: {
    high: 'wan2.2_i2v_high_noise_14B_fp8_scaled.safetensors',
    low: 'wan2.2_i2v_low_noise_14B_fp8_scaled.safetensors',
    loraHigh: 'wan2.2_i2v_lightx2v_4steps_lora_v1_high_noise.safetensors',
    loraLow: 'wan2.2_i2v_lightx2v_4steps_lora_v1_low_noise.safetensors',
  },
  t2v: {
    high: 'wan2.2_t2v_high_noise_14B_fp8_scaled.safetensors',
    low: 'wan2.2_t2v_low_noise_14B_fp8_scaled.safetensors',
    loraHigh: 'wan2.2_t2v_lightx2v_4steps_lora_v1.1_high_noise.safetensors',
    loraLow: 'wan2.2_t2v_lightx2v_4steps_lora_v1.1_low_noise.safetensors',
  },
  textEncoder: 'umt5_xxl_fp8_e4m3fn_scaled.safetensors',
  vae: 'wan_2.1_vae.safetensors',
  flux: 'flux1-schnell-fp8.safetensors',
};

// Wan's own negative prompt (it reads Chinese best).
const WAN_NEGATIVE = '色调艳丽，过曝，静态，细节模糊不清，字幕，风格，作品，画作，画面，静止，整体发灰，最差质量，低质量，JPEG压缩残留，丑陋的，残缺的，多余的手指，画得不好的手部，画得不好的脸部，畸形的，毁容的，形态畸形的肢体，手指融合，静止不动的画面，杂乱的背景，三条腿，背景人很多，倒着走';

const FAST = { steps: 4, split: 2, cfg: 1, shift: 5 };   // with the lightx2v 4-step LoRAs
const FULL = { steps: 20, split: 10, cfg: 3.5, shift: 8 }; // Wan 2.2's own template

const MODES = {
  i2v: {
    note: 'Wan 2.2 14B image-to-video: your picture becomes frame 1 and the prompt describes what happens next.',
    promptLabel: 'Motion prompt',
    help: 'Describe the motion and camera, not the picture: the image already shows what is there.',
    chips: ['slow camera push-in', 'camera orbits around the subject', 'turns head and smiles', 'hair and clothes move in the wind', 'handheld camera, natural movement', 'static camera'],
  },
  t2v: {
    note: 'Wan 2.2 14B text-to-video: everything comes from the prompt.',
    promptLabel: 'Prompt',
    help: 'Wan likes detailed prompts: subject, action, setting, camera move, lighting and style (50–100 words).',
    chips: ['cinematic, shallow depth of field', 'aerial drone shot', 'slow motion', 'golden hour lighting', 'tracking shot', 'anime style', 'static camera'],
  },
  chain: {
    note: 'FLUX schnell draws the first frame from text, then Wan 2.2 image-to-video animates it, in one job. Often sharper than pure text-to-video, and uses the models already installed.',
    promptLabel: 'Motion prompt',
    help: 'The first-frame prompt sets the look; this one sets the motion.',
    chips: ['slow camera push-in', 'camera orbits around the subject', 'gentle wind', 'subject walks toward the camera', 'static camera'],
  },
  t2i: {
    note: 'FLUX schnell text-to-image in ~4 steps: handy for making start frames for image-to-video.',
    promptLabel: 'Prompt',
    help: 'Describe the picture. Use “Use as start image” on a result to animate it.',
    chips: ['photo, 35mm, natural light', 'cinematic still', 'watercolor illustration', 'studio portrait', 'isometric 3D render'],
  },
};

const VIDEO_SIZES = [
  ['auto', 'Match start image (480p)'],
  ['832x480', '832×480 · 480p landscape'],
  ['480x832', '480×832 · 480p portrait'],
  ['640x640', '640×640 · square'],
  ['1280x720', '1280×720 · 720p landscape (slow)'],
  ['720x1280', '720×1280 · 720p portrait (slow)'],
];
const IMAGE_SIZES = [
  ['1024x1024', '1024×1024 · square'],
  ['1216x832', '1216×832 · landscape'],
  ['832x1216', '832×1216 · portrait'],
  ['1344x768', '1344×768 · wide'],
  ['832x480', '832×480 · video landscape'],
  ['480x832', '480×832 · video portrait'],
];

const LABELS = {
  unet_high: 'load high-noise model', unet_low: 'load low-noise model', lora_high: 'apply LoRA', lora_low: 'apply LoRA',
  ms_high: 'shift', ms_low: 'shift', clip: 'load text encoder', pos: 'encode prompt', neg: 'encode negative prompt',
  vae: 'load VAE', start: 'load start image', i2v: 'encode start image', latent: 'empty video latent',
  fckpt: 'load FLUX', fpos: 'encode FLUX prompt', fneg: 'encode FLUX prompt', fsize: 'empty image latent',
  fsample: 'FLUX drawing first frame', fdecode: 'decode first frame', first: 'save first frame',
  sample_high: 'high-noise expert (motion)', sample_low: 'low-noise expert (detail)', decode: 'VAE decode frames',
  video: 'make video', out: 'save',
};
const PHASE_COLORS = { load: '#9a9aa6', flux: '#e0a100', high: '#5b5bf0', low: '#18a3a3', decode: '#d0508a' };
const PHASE_NAMES = { load: 'loading', flux: 'FLUX', high: 'high', low: 'low', decode: 'decode' };

// ---------- state ----------

const store = {
  get(k, d) { try { const v = localStorage.getItem('studio.' + k); return v == null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem('studio.' + k, JSON.stringify(v)); } catch { } },
};

let mode = store.get('mode', 'i2v');
let available = null;           // { unets:Set, loras:Set, ckpts:Set } once ComfyUI answers
let startImage = null;          // { blob, url, uploaded: 'name in ComfyUI input' }
let runs = [];                  // saved runs, newest first
const jobs = new Map();         // promptId -> job
const selected = new Set();     // run ids picked for comparison
let clientId = store.get('clientId', null);
if (!clientId) { clientId = 'studio-' + Math.random().toString(36).slice(2, 10); store.set('clientId', clientId); }

const NUM_FIELDS = ['frames', 'steps', 'split', 'cfg', 'shift', 'loraStrength', 'fps', 'fluxSteps', 'seed'];

function defaults() {
  return { size: 'auto', frames: 49, ...FAST, loraStrength: 1, fps: 16, fluxSteps: 4, lora: true,
    seed: randomSeed(), seedLock: false, negative: WAN_NEGATIVE, imagePrompt: '', sweep: '', sweepValues: '',
    prompts: { i2v: '', t2v: '', chain: '', t2i: '' }, sizes: {} };
}
let form = Object.assign(defaults(), store.get('form', {}));

function randomSeed() { return Math.floor(Math.random() * 2 ** 40); }

// ---------- form ----------

function fillSizes() {
  const list = mode === 't2i' ? IMAGE_SIZES : VIDEO_SIZES.filter(([v]) => v !== 'auto' || mode === 'i2v');
  $('size').innerHTML = list.map(([v, l]) => `<option value="${v}">${l}</option>`).join('');
  const want = form.sizes[mode] || list[0][0];
  $('size').value = list.some(([v]) => v === want) ? want : list[0][0];
}

function applyMode() {
  document.querySelectorAll('#tabs button').forEach((b) => b.classList.toggle('on', b.dataset.mode === mode));
  const m = MODES[mode];
  const video = mode !== 't2i';
  $('modeNote').textContent = m.note;
  $('promptLabel').textContent = m.promptLabel;
  $('promptHelp').textContent = m.help;
  $('prompt').value = form.prompts[mode] || '';
  $('prompt').placeholder = mode === 'i2v' ? 'e.g. the woman turns toward the camera and laughs, camera slowly pushes in'
    : mode === 't2v' ? 'e.g. a red fox trots through fresh snow in a pine forest at dawn, low tracking shot, soft golden light, breath visible in the cold air'
    : mode === 'chain' ? 'e.g. the fox lifts its head and looks around, snow falls gently'
    : 'e.g. a red fox sitting in fresh snow in a pine forest at dawn, 35mm photo';
  $('promptChips').innerHTML = m.chips.map((c) => `<button type="button" class="ghost">${c}</button>`).join('');
  $('startField').hidden = mode !== 'i2v';
  $('imagePromptField').hidden = mode !== 'chain';
  $('framesField').hidden = !video;
  $('videoGroup').hidden = !video;
  $('negField').hidden = !video;
  $('fluxGroup').hidden = !(mode === 'chain' || mode === 't2i');
  $('sizeHelp').hidden = !video;
  for (const o of $('sweep').options) {
    const v = o.value;
    o.hidden = (!video && ['steps', 'split', 'shift', 'cfg', 'loraStrength', 'frames'].includes(v)) ||
      (v === 'fluxSteps' && !(mode === 'chain' || mode === 't2i'));
  }
  if ($('sweep').selectedOptions[0]?.hidden) { $('sweep').value = ''; form.sweep = ''; }
  fillSizes();
  updateSweepLabel();
  updateAvailability();
  store.set('mode', mode);
}

function loadForm() {
  for (const k of NUM_FIELDS) $(k).value = form[k];
  $('lora').checked = form.lora;
  $('seedLock').checked = form.seedLock;
  $('negative').value = form.negative;
  $('imagePrompt').value = form.imagePrompt;
  $('sweep').value = form.sweep;
  $('sweepValues').value = form.sweepValues;
  updateFrames();
}

function readForm() {
  for (const k of NUM_FIELDS) form[k] = Number($(k).value);
  form.lora = $('lora').checked;
  form.seedLock = $('seedLock').checked;
  form.negative = $('negative').value;
  form.imagePrompt = $('imagePrompt').value;
  form.sweep = $('sweep').value;
  form.sweepValues = $('sweepValues').value;
  form.prompts[mode] = $('prompt').value;
  form.sizes[mode] = $('size').value;
  store.set('form', form);
}

function updateFrames() {
  const f = Number($('frames').value), fps = Number($('fps').value) || 16;
  $('framesOut').textContent = `${f} frames · ${(f / fps).toFixed(1)} s`;
}

function updateSweepLabel() {
  const s = $('sweep').value;
  $('sweepValues').disabled = !s;
  $('sweepLabel').textContent = s === 'seed' ? 'How many' : 'Values';
  $('sweepValues').placeholder = { '': '', seed: 'e.g. 4', steps: 'e.g. 4, 6, 8', split: 'e.g. 1, 2, 3', shift: 'e.g. 3, 5, 8',
    cfg: 'e.g. 1, 2, 3.5', loraStrength: 'e.g. 0.6, 0.8, 1', frames: 'e.g. 33, 49, 81', fluxSteps: 'e.g. 1, 2, 4' }[s];
}

// ---------- availability of models on the PC ----------

async function loadModels() {
  try {
    const get = async (cls) => (await (await fetch('comfy/object_info/' + cls)).json())[cls].input.required;
    const [u, l, c] = await Promise.all([get('UNETLoader'), get('LoraLoaderModelOnly'), get('CheckpointLoaderSimple')]);
    available = { unets: new Set(u.unet_name[0]), loras: new Set(l.lora_name[0]), ckpts: new Set(c.ckpt_name[0]) };
  } catch { available = null; }
  updateAvailability();
}

function missingFor(m, lora = $('lora').checked) {
  if (!available) return [];
  const miss = [];
  const need = (set, name) => { if (!set.has(name)) miss.push(name); };
  if (m === 'i2v' || m === 't2v' || m === 'chain') {
    const w = MODELS[m === 't2v' ? 't2v' : 'i2v'];
    need(available.unets, w.high); need(available.unets, w.low);
    if (lora) { need(available.loras, w.loraHigh); need(available.loras, w.loraLow); }
  }
  if (m === 'chain' || m === 't2i') need(available.ckpts, MODELS.flux);
  return miss;
}

function updateAvailability() {
  document.querySelectorAll('#tabs button').forEach((b) => b.classList.toggle('missing', missingFor(b.dataset.mode).length > 0));
  const miss = missingFor(mode);
  $('go').disabled = miss.length > 0;
  $('formErr').textContent = miss.length
    ? (mode === 't2v' ? 'The text-to-video models are not on the PC yet (they may still be downloading). Missing:\n' : 'Missing on the PC:\n') + miss.join('\n')
    : '';
}

// ---------- workflows ----------

const node = (class_type, inputs) => ({ class_type, inputs });

function videoSize(p) {
  if (p.size !== 'auto') return p.size.split('x').map(Number);
  const r = p.imageAspect || 16 / 9;
  return r > 1.25 ? [832, 480] : r < 0.8 ? [480, 832] : [640, 640];
}

function fluxNodes(g, p, w, h, prompt) {
  g.fckpt = node('CheckpointLoaderSimple', { ckpt_name: MODELS.flux });
  g.fpos = node('CLIPTextEncode', { text: prompt, clip: ['fckpt', 1] });
  g.fneg = node('CLIPTextEncode', { text: '', clip: ['fckpt', 1] });
  g.fsize = node('EmptySD3LatentImage', { width: w, height: h, batch_size: 1 });
  g.fsample = node('KSampler', { seed: p.seed, steps: p.fluxSteps, cfg: 1, sampler_name: 'euler', scheduler: 'simple', denoise: 1,
    model: ['fckpt', 0], positive: ['fpos', 0], negative: ['fneg', 0], latent_image: ['fsize', 0] });
  g.fdecode = node('VAEDecode', { samples: ['fsample', 0], vae: ['fckpt', 2] });
}

function buildWorkflow(p) {
  const g = {};
  if (p.mode === 't2i') {
    const [w, h] = p.size.split('x').map(Number);
    fluxNodes(g, p, w, h, p.prompt);
    g.out = node('SaveImage', { images: ['fdecode', 0], filename_prefix: 'studio/image' });
    return g;
  }
  const [w, h] = videoSize(p);
  const m = MODELS[p.mode === 't2v' ? 't2v' : 'i2v'];
  g.unet_high = node('UNETLoader', { unet_name: m.high, weight_dtype: 'default' });
  g.unet_low = node('UNETLoader', { unet_name: m.low, weight_dtype: 'default' });
  let hi = 'unet_high', lo = 'unet_low';
  if (p.lora) {
    g.lora_high = node('LoraLoaderModelOnly', { lora_name: m.loraHigh, strength_model: p.loraStrength, model: ['unet_high', 0] });
    g.lora_low = node('LoraLoaderModelOnly', { lora_name: m.loraLow, strength_model: p.loraStrength, model: ['unet_low', 0] });
    hi = 'lora_high'; lo = 'lora_low';
  }
  g.ms_high = node('ModelSamplingSD3', { shift: p.shift, model: [hi, 0] });
  g.ms_low = node('ModelSamplingSD3', { shift: p.shift, model: [lo, 0] });
  g.clip = node('CLIPLoader', { clip_name: MODELS.textEncoder, type: 'wan', device: 'default' });
  g.pos = node('CLIPTextEncode', { text: p.prompt, clip: ['clip', 0] });
  g.neg = node('CLIPTextEncode', { text: p.negative, clip: ['clip', 0] });
  g.vae = node('VAELoader', { vae_name: MODELS.vae });

  let pos, neg, latent;
  if (p.mode === 't2v') {
    g.latent = node('EmptyHunyuanLatentVideo', { width: w, height: h, length: p.frames, batch_size: 1 });
    pos = ['pos', 0]; neg = ['neg', 0]; latent = ['latent', 0];
  } else {
    let startRef;
    if (p.mode === 'chain') {
      fluxNodes(g, p, w, h, p.imagePrompt || p.prompt);
      g.first = node('SaveImage', { images: ['fdecode', 0], filename_prefix: 'studio/first' });
      startRef = ['fdecode', 0];
    } else {
      g.start = node('LoadImage', { image: p.startImage || '(uploaded when you press Generate)' });
      startRef = ['start', 0];
    }
    g.i2v = node('WanImageToVideo', { positive: ['pos', 0], negative: ['neg', 0], vae: ['vae', 0], start_image: startRef,
      width: w, height: h, length: p.frames, batch_size: 1 });
    pos = ['i2v', 0]; neg = ['i2v', 1]; latent = ['i2v', 2];
  }
  const split = Math.min(Math.max(p.split, 0), p.steps);
  const sampler = (model, noise, start, end, lat, leftover) => node('KSamplerAdvanced', {
    add_noise: noise, noise_seed: p.seed, steps: p.steps, cfg: p.cfg, sampler_name: 'euler', scheduler: 'simple',
    start_at_step: start, end_at_step: end, return_with_leftover_noise: leftover,
    model: [model, 0], positive: pos, negative: neg, latent_image: lat });
  g.sample_high = sampler('ms_high', 'enable', 0, split, latent, 'enable');
  g.sample_low = sampler('ms_low', 'disable', split, 10000, ['sample_high', 0], 'disable');
  g.decode = node('VAEDecode', { samples: ['sample_low', 0], vae: ['vae', 0] });
  g.video = node('CreateVideo', { images: ['decode', 0], fps: p.fps });
  g.out = node('SaveVideo', { video: ['video', 0], filename_prefix: 'studio/video', format: 'mp4', codec: 'h264' });
  return g;
}

// The settings of one run, as stored with it.
function paramsFromForm() {
  readForm();
  const p = { mode, prompt: form.prompts[mode].trim(), size: form.sizes[mode] };
  if (mode !== 't2i') Object.assign(p, { frames: normFrames(form.frames), fps: form.fps, lora: form.lora, loraStrength: form.loraStrength,
    steps: form.steps, split: form.split, cfg: form.cfg, shift: form.shift, negative: form.negative });
  if (mode === 'chain' || mode === 't2i') p.fluxSteps = form.fluxSteps;
  if (mode === 'chain') p.imagePrompt = form.imagePrompt.trim();
  if (mode === 'i2v' && startImage) p.imageAspect = startImage.aspect;
  p.seed = form.seed;
  return p;
}

const normFrames = (n) => Math.floor((Math.min(Math.max(Math.round(n), 5), 161) - 1) / 4) * 4 + 1;

function expandSweep(base) {
  const s = form.sweep;
  if (!s) return [base];
  const raw = form.sweepValues.split(/[\s,;]+/).filter(Boolean).map(Number).filter((n) => Number.isFinite(n));
  if (!raw.length) throw new Error('Enter the values to compare.');
  if (s === 'seed') {
    const n = Math.min(Math.max(Math.round(raw[0]), 1), 12);
    return Array.from({ length: n }, (_, i) => ({ ...base, seed: form.seedLock ? base.seed + i : randomSeed(), sweep: 'seed' }));
  }
  if (raw.length > 12) throw new Error('At most 12 values at a time.');
  return raw.map((v) => ({ ...base, [s]: s === 'frames' ? normFrames(v) : v, sweep: s }));
}

// ---------- start image ----------

async function setStartImage(blob) {
  const url = URL.createObjectURL(blob);
  const img = new Image();
  img.src = url;
  await img.decode().catch(() => { });
  if (startImage) URL.revokeObjectURL(startImage.url);
  startImage = { blob, url, aspect: img.naturalWidth / (img.naturalHeight || 1), uploaded: null };
  $('startPreview').src = url;
  $('drop').classList.add('has');
}

async function uploadStart() {
  if (startImage.uploaded) return startImage.uploaded;
  const fd = new FormData();
  const ext = startImage.blob.type === 'image/png' ? 'png' : 'jpg';
  fd.append('image', startImage.blob, `studio-${Date.now()}.${ext}`);
  fd.append('overwrite', 'true');
  const r = await fetch('comfy/upload/image', { method: 'POST', body: fd });
  if (!r.ok) throw new Error('Uploading the start image failed: ' + r.status);
  const j = await r.json();
  startImage.uploaded = j.subfolder ? `${j.subfolder}/${j.name}` : j.name;
  return startImage.uploaded;
}

async function lastFrame(src) {
  const v = document.createElement('video');
  v.muted = true; v.preload = 'auto'; v.src = src;
  await new Promise((res, rej) => { v.onloadeddata = res; v.onerror = () => rej(new Error('cannot read video')); });
  v.currentTime = Math.max(0, v.duration - 0.03);
  await new Promise((res) => { v.onseeked = res; });
  const c = document.createElement('canvas');
  c.width = v.videoWidth; c.height = v.videoHeight;
  c.getContext('2d').drawImage(v, 0, 0);
  return new Promise((res) => c.toBlob(res, 'image/png'));
}

// ---------- queueing ----------

function runId() {
  const d = new Date(), z = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}${z(d.getMonth() + 1)}${z(d.getDate())}-${z(d.getHours())}${z(d.getMinutes())}${z(d.getSeconds())}-${Math.random().toString(36).slice(2, 6)}`;
}

async function generate() {
  $('formErr').textContent = '';
  try {
    const base = paramsFromForm();
    if (mode === 'i2v' && !startImage) throw new Error('Choose a start image first.');
    if (mode !== 'i2v' && !base.prompt) throw new Error('Write a prompt first.');
    const list = expandSweep(base);
    $('go').disabled = true;
    if (mode === 'i2v') {
      const name = await uploadStart();
      list.forEach((p) => { p.startImage = name; });
    }
    for (const p of list) await queue(p);
    if (!form.seedLock) { form.seed = randomSeed(); $('seed').value = form.seed; readForm(); }
  } catch (e) {
    $('formErr').textContent = e.message;
  } finally {
    updateAvailability();
  }
}

// Which model files a job keeps in the PC's memory. ComfyUI caches models in RAM between jobs,
// and Wan i2v + Wan t2v + FLUX together are more than the PC's RAM: ComfyUI then crashes. So
// before a job needs models the previous one didn't load, ask ComfyUI to drop what it holds
// (it does that between jobs, so a running one isn't disturbed).
const FAMILIES = { i2v: ['wan-i2v'], t2v: ['wan-t2v'], chain: ['flux', 'wan-i2v'], t2i: ['flux'] };
let lastFamilies = null;
async function freeIfSwitching(m) {
  const need = FAMILIES[m];
  if (lastFamilies && need.every((f) => lastFamilies.includes(f))) return;
  lastFamilies = need;
  await fetch('comfy/free', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ unload_models: true, free_memory: true }) }).catch(() => { });
}

async function queue(p) {
  const workflow = buildWorkflow(p);
  await freeIfSwitching(p.mode);
  const r = await fetch('comfy/prompt', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ prompt: workflow, client_id: clientId }) });
  const j = await r.json().catch(() => ({}));
  if (!r.ok || !j.prompt_id) {
    const detail = j.node_errors ? Object.entries(j.node_errors).map(([n, e]) => `${n}: ${e.errors.map((x) => x.details || x.message).join('; ')}`).join('\n') : '';
    throw new Error(`ComfyUI rejected the job: ${j.error?.message || r.status}\n${detail}`);
  }
  const job = { id: runId(), promptId: j.prompt_id, params: p, workflow, status: 'queued', queuedAt: Date.now(),
    nodes: {}, current: null, progress: null, startUrl: p.mode === 'i2v' ? startImage?.url : null };
  jobs.set(job.promptId, job);
  savePending();
  renderJobs();
}

function savePending() {
  store.set('pending', [...jobs.values()].filter((j) => j.status !== 'failed').map((j) => ({ id: j.id, promptId: j.promptId, params: j.params, workflow: j.workflow, queuedAt: j.queuedAt })));
}

// After a reload: pick up jobs that were queued before it.
async function adoptPending() {
  const pending = store.get('pending', []);
  if (!pending.length) return;
  let q = { queue_running: [], queue_pending: [] };
  try { q = await (await fetch('comfy/queue')).json(); } catch { return; }
  const live = new Set([...q.queue_running, ...q.queue_pending].map((e) => e[1]));
  for (const p of pending) {
    const job = { ...p, status: live.has(p.promptId) ? 'queued' : 'saving', nodes: {}, current: null, progress: null };
    jobs.set(p.promptId, job);
    if (!live.has(p.promptId)) finish(job);
  }
  renderJobs();
}

async function cancel(job) {
  const running = job.status === 'running';
  await fetch(running ? 'comfy/interrupt' : 'comfy/queue', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(running ? { prompt_id: job.promptId } : { delete: [job.promptId] }) }).catch(() => { });
  jobs.delete(job.promptId);
  savePending();
  renderJobs();
}

// ---------- progress ----------

// ComfyUI's progress messages, through the studio server as server-sent events (plain HTTP,
// so this also works through HomeLLM's relay). EventSource reconnects by itself.
function connectWS() {
  const es = new EventSource(`api/events?clientId=${encodeURIComponent(clientId)}`);
  es.onopen = () => { if (wsDropped) { wsDropped = false; reconcile(); } };
  es.onmessage = (ev) => {
    let m; try { m = JSON.parse(ev.data); } catch { return; }
    onEvent(m.type, m.data || {});
  };
  es.addEventListener('preview', (ev) => onPreview(Uint8Array.from(atob(ev.data), (c) => c.charCodeAt(0)).buffer));
  es.onerror = () => { wsDropped = true; };
}
let wsDropped = false;

// After losing ComfyUI's event stream (tunnel drop, or ComfyUI restarting after a crash), find
// out what became of each job: still queued, finished meanwhile, or lost.
async function reconcile() {
  let q;
  try { q = await (await fetch('comfy/queue')).json(); } catch { return; }
  const live = new Set([...q.queue_running, ...q.queue_pending].map((e) => e[1]));
  for (const job of [...jobs.values()]) {
    if (live.has(job.promptId) || job.status === 'failed' || job.status === 'saving') continue;
    let h = {};
    try { h = await (await fetch('comfy/history/' + job.promptId)).json(); } catch { }
    const st = h[job.promptId]?.status;
    if (st?.completed) { job.endedAt = Date.now(); finish(job); continue; }
    job.status = 'failed';
    job.error = st ? 'ComfyUI reported an error for this job.'
      : 'ComfyUI stopped while running this job (most likely it ran out of memory) and has restarted. Run it again; if it keeps happening, use Unload LLM / Free VRAM first or try fewer frames.';
    lastFamilies = null;
  }
  savePending();
  renderJobs();
}

function onPreview(buf) {
  if (new DataView(buf).getUint32(0) !== 1) return;
  const job = [...jobs.values()].find((j) => j.status === 'running');
  if (!job) return;
  if (job.preview) URL.revokeObjectURL(job.preview);
  job.preview = URL.createObjectURL(new Blob([buf.slice(8)], { type: 'image/jpeg' }));
  renderJobs();
}

function closeNode(job, now) {
  if (job.current && job.nodes[job.current] && !job.nodes[job.current].end) job.nodes[job.current].end = now;
}

function onEvent(type, d) {
  if (type === 'status') {
    const n = d.status?.exec_info?.queue_remaining;
    $('queueText').textContent = n != null ? `${n} in ComfyUI's queue` : '';
    return;
  }
  const job = jobs.get(d.prompt_id);
  if (!job) return;
  const now = Date.now();
  switch (type) {
    case 'execution_start':
      job.status = 'running'; job.startedAt = now; break;
    case 'execution_cached':
      for (const n of d.nodes || []) job.nodes[n] = { cached: true };
      break;
    case 'executing':
      if (job.status !== 'running') { job.status = 'running'; job.startedAt = job.startedAt || now; }
      closeNode(job, now);
      job.current = d.node;
      if (d.node) job.nodes[d.node] = { start: now };
      job.progress = null;
      break;
    case 'progress': {
      const n = job.nodes[d.node] || (job.nodes[d.node] = { start: now });
      if (!n.firstStep) n.firstStep = now;
      n.steps = d.max;
      job.progress = { node: d.node, value: d.value, max: d.max };
      break;
    }
    case 'execution_success':
      closeNode(job, now); job.current = null; job.endedAt = now; finish(job); break;
    case 'execution_error':
      closeNode(job, now); job.status = 'failed';
      job.error = `${LABELS[d.node_id] || d.node_id || ''}: ${d.exception_message || 'failed'}`.trim();
      savePending(); break;
    case 'execution_interrupted':
      jobs.delete(job.promptId); savePending(); break;
  }
  renderJobs();
}

// timing turns node times into phases: loading, FLUX, high expert, low expert, decode. Time a
// sampler spends before its first step is moving weights onto the GPU, so it counts as loading.
function timing(job) {
  const ph = { load: 0, flux: 0, high: 0, low: 0, decode: 0 };
  const per = {};
  for (const [id, n] of Object.entries(job.nodes)) {
    if (!n.start || !n.end) continue;
    const dur = n.end - n.start;
    const samplePhase = { sample_high: 'high', sample_low: 'low', fsample: 'flux' }[id];
    if (samplePhase) {
      const pre = n.firstStep ? n.firstStep - n.start : 0;
      ph.load += pre; ph[samplePhase] += dur - pre;
      if (n.steps) per[samplePhase] = (dur - pre) / n.steps;
    } else if (['decode', 'video', 'out', 'fdecode', 'first'].includes(id)) ph.decode += dur;
    else ph.load += dur;
  }
  const total = (job.endedAt || Date.now()) - (job.startedAt || job.queuedAt);
  return { total, phases: ph, perStep: per };
}

async function finish(job) {
  job.status = 'saving';
  renderJobs();
  const p = job.params;
  const meta = { mode: p.mode, params: p, workflow: job.workflow, queuedAt: job.queuedAt };
  if (job.startedAt) meta.timing = timing(job);
  try {
    const r = await fetch('api/runs', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: job.id, promptId: job.promptId, startImage: p.startImage || '', meta }) });
    if (!r.ok) throw new Error(await r.text());
    const saved = await r.json();
    jobs.delete(job.promptId);
    runs.unshift(saved);
    renderGallery();
  } catch (e) {
    job.status = 'failed'; job.error = 'Saving failed: ' + e.message;
  }
  savePending();
  renderJobs();
}

const fmtS = (ms) => ms >= 60000 ? `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s` : `${(ms / 1000).toFixed(ms < 10000 ? 1 : 0)}s`;
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

function jobPhase(job) {
  if (job.status === 'queued') return 'waiting in queue…';
  if (job.status === 'saving') return 'saving…';
  if (job.status === 'failed') return job.error;
  const n = job.current, pr = job.progress;
  if (pr && pr.node === n) return `${LABELS[n] || n}: step ${pr.value}/${pr.max}`;
  if (n && (n === 'sample_high' || n === 'sample_low' || n === 'fsample')) return `${LABELS[n]}: moving weights to the GPU…`;
  return n ? (LABELS[n] || n) + '…' : 'starting…';
}

function jobFraction(job) {
  if (job.status !== 'running') return job.status === 'saving' ? 1 : 0;
  const p = job.params, pr = job.progress;
  if (p.mode === 't2i') return pr ? pr.value / pr.max : 0.05;
  const order = ['fsample', 'sample_high', 'sample_low'].filter((n) => job.workflow[n]);
  const totalSteps = order.reduce((a, n) => a + (n === 'fsample' ? p.fluxSteps : n === 'sample_high' ? Math.min(p.split, p.steps) : p.steps - Math.min(p.split, p.steps)), 0) || 1;
  let done = 0;
  for (const n of order) {
    const steps = n === 'fsample' ? p.fluxSteps : n === 'sample_high' ? Math.min(p.split, p.steps) : p.steps - Math.min(p.split, p.steps);
    if (job.nodes[n]?.end) done += steps;
    else if (pr && pr.node === n) done += pr.value;
  }
  return Math.min(0.97, 0.03 + 0.92 * done / totalSteps + (job.nodes.decode?.start ? 0.03 : 0));
}

function renderJobs() {
  const list = [...jobs.values()];
  $('live').hidden = list.length === 0;
  $('jobs').innerHTML = list.map((job) => {
    const p = job.params;
    const elapsed = job.startedAt ? ` · ${fmtS(Date.now() - job.startedAt)}` : '';
    const thumb = job.preview || job.startUrl;
    return `<div class="job ${job.status}" data-id="${job.promptId}">
      ${thumb ? `<img class="thumb" src="${thumb}">` : '<div class="thumb"></div>'}
      <div class="info"><div class="title">${esc(modeName(p.mode))}${p.sweep ? ` · ${esc(p.sweep)} = ${esc(p[p.sweep])}` : ''} — ${esc(p.prompt || '(no prompt)')}</div>
      <div class="phase">${esc(jobPhase(job))}${elapsed}</div>
      <div class="bar"><div style="width:${Math.round(jobFraction(job) * 100)}%"></div></div></div>
      <button class="ghost" data-act="${job.status === 'failed' ? 'dismiss' : 'cancel'}">${job.status === 'failed' ? 'Dismiss' : 'Cancel'}</button>
    </div>`;
  }).join('');
}
setInterval(() => { if ([...jobs.values()].some((j) => j.status === 'running')) renderJobs(); }, 1000);

const modeName = (m) => ({ i2v: 'Image → Video', t2v: 'Text → Video', chain: 'Text → Image → Video', t2i: 'Text → Image' }[m] || m);

// ---------- gallery ----------

async function loadRuns() {
  try { runs = await (await fetch('api/runs')).json(); } catch { runs = []; }
  renderGallery();
}

function runTags(r) {
  const p = r.params || {};
  const t = [];
  const [w, h] = p.mode === 't2i' ? (p.size || '').split('x') : videoSize(p);
  t.push(['size', `${w}×${h}`]);
  if (p.mode !== 't2i') {
    t.push(['frames', `${p.frames}f · ${(p.frames / p.fps).toFixed(1)}s`]);
    t.push(['steps', `${p.steps} steps`]);
    t.push(['split', `switch @${p.split}`]);
    t.push(['cfg', `cfg ${p.cfg}`]);
    t.push(['shift', `shift ${p.shift}`]);
    t.push(['loraStrength', p.lora ? `LoRA ${p.loraStrength}` : 'no LoRA']);
  }
  if (p.fluxSteps) t.push(['fluxSteps', `FLUX ${p.fluxSteps} steps`]);
  t.push(['seed', `seed ${p.seed}`]);
  return t.map(([k, v]) => `<span class="tag ${p.sweep === k ? 'diff' : ''}">${esc(v)}</span>`).join('');
}

function timingHTML(tm) {
  if (!tm || !tm.total) return '';
  const ph = tm.phases, sum = Object.values(ph).reduce((a, b) => a + b, 0) || 1;
  const order = Object.keys(PHASE_NAMES).filter((k) => ph[k] != null).map((k) => [k, ph[k]]);
  const bar = order.filter(([, v]) => v > 0).map(([k, v]) =>
    `<div title="${PHASE_NAMES[k]} ${fmtS(v)}" style="width:${(v / sum * 100).toFixed(1)}%;background:${PHASE_COLORS[k]}"></div>`).join('');
  const parts = order.filter(([, v]) => v > 300).map(([k, v]) =>
    `<span style="color:${PHASE_COLORS[k]}">●</span> ${PHASE_NAMES[k]} ${fmtS(v)}${tm.perStep[k] ? ` (${fmtS(tm.perStep[k])}/step)` : ''}`);
  return `<div class="timing">${bar}</div><div class="timingText"><b>${fmtS(tm.total)}</b> · ${parts.join(' · ')}</div>`;
}

function mainFile(r) {
  const f = r.files || [];
  return f.find((x) => x.kind === 'video') || f.find((x) => x.node === 'out') || f[0];
}
const fileUrl = (r, name) => `runs/${encodeURIComponent(r.id)}/${encodeURIComponent(name)}`;

function renderGallery() {
  const q = $('filter').value.trim().toLowerCase(), mf = $('modeFilter').value;
  const list = runs.filter((r) => (!mf || r.mode === mf) &&
    (!q || `${r.params?.prompt} ${r.params?.imagePrompt}`.toLowerCase().includes(q)));
  $('empty').hidden = runs.length > 0;
  $('gallery').innerHTML = list.map((r) => {
    const f = mainFile(r), p = r.params || {};
    const first = (r.files || []).find((x) => x.node === 'first');
    const startThumb = first ? fileUrl(r, first.name) : r.startFile ? fileUrl(r, r.startFile) : '';
    const media = !f ? '<span class="small">no output</span>' : f.kind === 'video'
      ? `<video src="${fileUrl(r, f.name)}" loop muted autoplay playsinline controls preload="metadata"></video>`
      : `<img src="${fileUrl(r, f.name)}" loading="lazy">`;
    return `<article class="card ${selected.has(r.id) ? 'sel' : ''}" data-id="${esc(r.id)}">
      <div class="media">${media}${startThumb ? `<img class="startThumb" src="${startThumb}" title="start image">` : ''}
        <input type="checkbox" class="pick" title="Select to compare" ${selected.has(r.id) ? 'checked' : ''}></div>
      <div class="body">
        <div class="small">${esc(modeName(r.mode))} · ${new Date(r.savedAt).toLocaleString()}</div>
        <div class="prompt" title="${esc(p.prompt)}">${p.imagePrompt ? `<b>Frame:</b> ${esc(p.imagePrompt)}<br>` : ''}${esc(p.prompt || '(no prompt)')}</div>
        <div class="tags">${runTags(r)}</div>
        ${timingHTML(r.timing)}
        <div class="cardActions">
          <button class="ghost" data-act="reuse">Reuse settings</button>
          ${f?.kind === 'video' ? '<button class="ghost" data-act="continue" title="Use the last frame as the start image, to make the clip longer">Continue from last frame</button>' : ''}
          ${f?.kind === 'image' || first ? '<button class="ghost" data-act="useStart">Use as start image</button>' : ''}
          <button class="ghost" data-act="workflow">Workflow</button>
          ${f ? `<a href="${fileUrl(r, f.name)}" download="${esc(r.id + '-' + f.name)}"><button class="ghost">Download</button></a>` : ''}
          ${EMBEDDED ? '' : '<button class="ghost" data-act="reveal">Show in Finder</button>'}
          <button class="ghost" data-act="delete">Delete</button>
        </div>
      </div></article>`;
  }).join('');
  observeVideos();
  $('compareBtn').disabled = selected.size < 2;
  $('compareBtn').textContent = selected.size ? `Compare selected (${selected.size})` : 'Compare selected';
}

// Play only the clips on screen.
const io = new IntersectionObserver((entries) => {
  for (const e of entries) {
    if (e.isIntersecting) e.target.play().catch(() => { }); else e.target.pause();
  }
}, { threshold: 0.4 });
function observeVideos() { document.querySelectorAll('#gallery video').forEach((v) => io.observe(v)); }

async function reuse(r, switchMode = true) {
  const p = r.params || {};
  if (switchMode) mode = r.mode;
  for (const k of ['frames', 'steps', 'split', 'cfg', 'shift', 'loraStrength', 'fps', 'fluxSteps', 'seed']) if (p[k] != null) form[k] = p[k];
  if (p.lora != null) form.lora = p.lora;
  if (p.negative != null) form.negative = p.negative;
  if (p.imagePrompt != null) form.imagePrompt = p.imagePrompt;
  form.prompts[mode] = p.prompt || '';
  if (p.size) form.sizes[mode] = p.size;
  form.seedLock = true;
  form.sweep = '';
  loadForm();
  applyMode();
  readForm();
  if (r.mode === 'i2v' && r.startFile) setStartImage(await (await fetch(fileUrl(r, r.startFile))).blob());
  document.getElementById('panel').scrollTop = 0;
}

async function onCardAction(r, act, btn) {
  if (act === 'reuse') return reuse(r);
  if (act === 'continue') {
    btn.disabled = true;
    try {
      const blob = await lastFrame(fileUrl(r, mainFile(r).name));
      await reuse(r, false);
      mode = 'i2v'; form.sizes.i2v = 'auto';
      applyMode();
      await setStartImage(blob);
      if (r.mode !== 'i2v') { form.prompts.i2v = r.params.prompt || ''; $('prompt').value = form.prompts.i2v; }
    } finally { btn.disabled = false; }
    return;
  }
  if (act === 'useStart') {
    const first = (r.files || []).find((x) => x.node === 'first') || mainFile(r);
    mode = 'i2v'; applyMode();
    await setStartImage(await (await fetch(fileUrl(r, first.name))).blob());
    return;
  }
  if (act === 'workflow') return showWorkflow(r.workflow, `Workflow · ${r.id}`);
  if (act === 'reveal') return fetch('api/reveal?id=' + encodeURIComponent(r.id), { method: 'POST' });
  if (act === 'delete') {
    if (!confirm('Delete this run and its files?')) return;
    await fetch('api/runs/' + encodeURIComponent(r.id), { method: 'DELETE' });
    runs = runs.filter((x) => x.id !== r.id);
    selected.delete(r.id);
    renderGallery();
  }
}

function showWorkflow(wf, title) {
  $('wfTitle').textContent = title;
  $('wfText').textContent = JSON.stringify(wf, null, 2);
  $('wfDialog').showModal();
}

function openCompare() {
  const list = runs.filter((r) => selected.has(r.id));
  // Show only the settings that differ between the picked runs.
  const keys = ['mode', 'prompt', 'imagePrompt', 'size', 'frames', 'fps', 'steps', 'split', 'cfg', 'shift', 'lora', 'loraStrength', 'fluxSteps', 'seed'];
  const differ = keys.filter((k) => new Set(list.map((r) => JSON.stringify(r.params?.[k]))).size > 1);
  $('cmpGrid').innerHTML = list.map((r) => {
    const f = mainFile(r);
    const media = f?.kind === 'video' ? `<video src="${fileUrl(r, f.name)}" loop muted playsinline controls></video>` : f ? `<img src="${fileUrl(r, f.name)}">` : '';
    const diff = differ.map((k) => `${k} = ${esc(String(r.params?.[k]).slice(0, 80))}`).join(' · ');
    return `<figure>${media}<figcaption>${diff || 'same settings'}${r.timing ? ` · ${fmtS(r.timing.total)}` : ''}</figcaption></figure>`;
  }).join('');
  $('cmpDialog').showModal();
  syncPlay();
}
function syncPlay() {
  const vids = [...document.querySelectorAll('#cmpGrid video')];
  vids.forEach((v) => { v.pause(); v.currentTime = 0; });
  Promise.all(vids.map((v) => new Promise((res) => (v.readyState >= 3 ? res() : v.addEventListener('canplay', res, { once: true })))))
    .then(() => vids.forEach((v) => v.play().catch(() => { })));
}

// ---------- header status ----------

async function pollStatus() {
  let ok = false;
  try {
    const s = await (await fetch('api/status')).json();
    const t = s.tunnel || '';
    ok = t === 'up' || t === 'direct';
    $('connText').textContent = ok ? `connected to ${s.host}` : t.startsWith('down') ? `no connection: ${t.slice(6) || 'ssh exited'}` : 'connecting to ' + s.host + '…';
  } catch { $('connText').textContent = 'studio not running'; }
  let comfy = false;
  if (ok) {
    try {
      const st = await (await fetch('comfy/system_stats')).json();
      const d = st.devices?.[0];
      if (d) {
        const used = d.vram_total - d.vram_free;
        $('vramBar').style.width = `${(used / d.vram_total * 100).toFixed(0)}%`;
        $('vramText').textContent = `${(used / 2 ** 30).toFixed(1)} / ${(d.vram_total / 2 ** 30).toFixed(0)} GB`;
      }
      comfy = true;
    } catch { $('connText').textContent += ' · ComfyUI not answering'; }
    try {
      const ps = await (await fetch('ollama/api/ps')).json();
      const names = (ps.models || []).map((m) => m.name);
      $('llmText').textContent = names.length ? names.join(', ') : 'none loaded';
      $('unloadLlm').disabled = !names.length;
    } catch { $('llmText').textContent = '–'; }
    if (comfy && !available) loadModels();
  }
  $('conn').querySelector('.dot').className = 'dot ' + (comfy ? 'ok' : ok ? '' : 'bad');
}

async function unloadLlm() {
  const ps = await (await fetch('ollama/api/ps')).json();
  await Promise.all((ps.models || []).map((m) => fetch('ollama/api/generate', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: m.name, keep_alive: 0 }) })));
  pollStatus();
}

// ---------- wiring ----------

function wire() {
  $('tabs').addEventListener('click', (e) => {
    const b = e.target.closest('button'); if (!b) return;
    readForm(); mode = b.dataset.mode; applyMode();
  });
  $('promptChips').addEventListener('click', (e) => {
    const b = e.target.closest('button'); if (!b) return;
    const t = $('prompt'); t.value = t.value.trim() ? `${t.value.trim().replace(/[,.]$/, '')}, ${b.textContent}` : b.textContent;
    readForm();
  });
  document.querySelectorAll('#panel input, #panel select, #panel textarea').forEach((el) => el.addEventListener('change', readForm));
  $('frames').addEventListener('input', updateFrames);
  $('fps').addEventListener('input', updateFrames);
  $('sweep').addEventListener('change', updateSweepLabel);
  $('lora').addEventListener('change', () => {
    const d = $('lora').checked ? FAST : FULL;
    for (const [k, v] of Object.entries(d)) $(k).value = v;
    readForm(); updateAvailability();
  });
  $('dice').addEventListener('click', () => { $('seed').value = randomSeed(); readForm(); });
  $('go').addEventListener('click', generate);
  $('showWf').addEventListener('click', () => {
    try {
      const p = expandSweep(paramsFromForm())[0];
      if (mode === 'i2v') p.startImage = startImage?.uploaded;
      showWorkflow(buildWorkflow(p), `Workflow · ${modeName(mode)}`);
    } catch (e) { $('formErr').textContent = e.message; }
  });

  const drop = $('drop');
  drop.addEventListener('click', () => $('fileInput').click());
  drop.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') $('fileInput').click(); });
  $('fileInput').addEventListener('change', () => { const f = $('fileInput').files[0]; if (f) setStartImage(f); });
  drop.addEventListener('dragover', (e) => { e.preventDefault(); drop.classList.add('over'); });
  drop.addEventListener('dragleave', () => drop.classList.remove('over'));
  drop.addEventListener('drop', (e) => {
    e.preventDefault(); drop.classList.remove('over');
    const f = [...e.dataTransfer.files].find((x) => x.type.startsWith('image/')); if (f) setStartImage(f);
  });
  document.addEventListener('paste', (e) => {
    if (mode !== 'i2v') return;
    const it = [...(e.clipboardData?.items || [])].find((x) => x.type.startsWith('image/'));
    if (it) { setStartImage(it.getAsFile()); e.preventDefault(); }
  });

  $('jobs').addEventListener('click', (e) => {
    const b = e.target.closest('button[data-act]'); if (!b) return;
    const job = jobs.get(b.closest('.job').dataset.id); if (!job) return;
    if (b.dataset.act === 'cancel') cancel(job); else { jobs.delete(job.promptId); savePending(); renderJobs(); }
  });
  $('cancelAll').addEventListener('click', () => [...jobs.values()].reverse().forEach(cancel));

  $('gallery').addEventListener('click', (e) => {
    const card = e.target.closest('.card'); if (!card) return;
    const r = runs.find((x) => x.id === card.dataset.id); if (!r) return;
    if (e.target.classList.contains('pick')) {
      if (e.target.checked) selected.add(r.id); else selected.delete(r.id);
      card.classList.toggle('sel', e.target.checked);
      $('compareBtn').disabled = selected.size < 2;
      $('compareBtn').textContent = selected.size ? `Compare selected (${selected.size})` : 'Compare selected';
      return;
    }
    const b = e.target.closest('button[data-act]');
    if (b) onCardAction(r, b.dataset.act, b);
  });
  $('filter').addEventListener('input', renderGallery);
  $('modeFilter').addEventListener('change', renderGallery);
  $('compareBtn').addEventListener('click', openCompare);
  $('cmpSync').addEventListener('click', syncPlay);
  $('cmpClose').addEventListener('click', () => { $('cmpDialog').close(); $('cmpGrid').innerHTML = ''; });
  $('wfClose').addEventListener('click', () => $('wfDialog').close());
  $('wfCopy').addEventListener('click', () => navigator.clipboard.writeText($('wfText').textContent));
  $('wfSave').addEventListener('click', () => {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([$('wfText').textContent], { type: 'application/json' }));
    a.download = 'studio-workflow.json'; a.click();
  });

  $('unloadLlm').addEventListener('click', unloadLlm);
  $('pairCodeBtn').addEventListener('click', async () => {
    $('codeText').textContent = '…'; $('codeNote').textContent = '';
    $('codeDialog').showModal();
    try {
      const r = await fetch('api/pair-code', { method: 'POST' });
      if (!r.ok) throw new Error(await r.text());
      const c = await r.json();
      $('codeText').textContent = c.code;
      $('codeNote').textContent = `Type it in the HomeLLM web app (Pair a PC) to pair with ${c.name}. It works once, until ${new Date(c.expires_at * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}.`;
    } catch (e) { $('codeText').textContent = '–'; $('codeNote').textContent = e.message; }
  });
  $('codeClose').addEventListener('click', () => $('codeDialog').close());
  $('freeVram').addEventListener('click', () => fetch('comfy/free', { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ unload_models: true, free_memory: true }) }).then(pollStatus));
  $('openFolder').addEventListener('click', () => fetch('api/reveal', { method: 'POST' }));
  $('quit').addEventListener('click', async () => {
    if (jobs.size && !confirm('Runs are still queued. Their results will stay on the PC but not be saved here. Quit anyway?')) return;
    await fetch('api/quit', { method: 'POST' }).catch(() => { });
    document.body.innerHTML = '<p class="empty">HomeLLM Studio stopped. You can close this window.</p>';
    window.close();
  });
}

// Inside HomeLLM's web page (homellm.eiuoo.com/studio/) the studio runs on the PC itself:
// there's no Finder to show and no app to quit from here.
const EMBEDDED = window.top !== window;
if (EMBEDDED) {
  document.body.classList.add('embedded');
  ['openFolder', 'quit'].forEach((id) => { $(id).hidden = true; });
  // The only header on homellm.eiuoo.com/studio/: its title leads back to HomeLLM.
  const brand = document.querySelector('header .brand');
  const home = document.createElement('a');
  home.className = 'brand';
  home.href = '/';
  home.target = '_top';
  home.title = 'Back to HomeLLM (chat & pictures)';
  home.innerHTML = '← HomeLLM <b>Studio</b>';
  brand.replaceWith(home);
  // HomeLLM's site-wide text size (homellm.js keeps it under the same localStorage key); the
  // bridge page around this frame applies it when it changes.
  const sizes = [[0.9, 'Small'], [1, 'Normal'], [1.15, 'Large'], [1.3, 'Larger'], [1.5, 'Largest']];
  const pick = document.createElement('label');
  pick.className = 'stat';
  pick.title = 'Text size (this browser)';
  pick.innerHTML = '<b>Aa</b> <select id="textSize">' + sizes.map(([v, n]) => `<option value="${v}">${n}</option>`).join('') + '</select>';
  document.querySelector('header .spacer').before(pick);
  const sel = pick.querySelector('select');
  try { sel.value = String(JSON.parse(localStorage.getItem('homellm.textSize') || '1')); } catch { sel.value = '1'; }
  sel.onchange = () => {
    try { localStorage.setItem('homellm.textSize', JSON.stringify(Number(sel.value))); } catch { }
    document.documentElement.style.zoom = sel.value; // at once; the bridge does the same
  };
}

loadForm();
applyMode();
wire();
loadRuns();
connectWS();
pollStatus().then(adoptPending);
setInterval(pollStatus, 3000);
setInterval(() => { if (available && missingFor('t2v').length) { available = null; } }, 60000); // re-check models (downloads finishing)
