import { Player } from './player.js';

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

let config = null;
let lastMeta = null;
let camLatency = null; // helper: sensor timestamp -> socket write, ms

// ---------- helpers ----------
const getPath = (o, p) => p.split('.').reduce((a, k) => (a == null ? a : a[k]), o);
function setPath(o, p, v) {
  const ks = p.split('.');
  let cur = o;
  ks.slice(0, -1).forEach((k) => (cur = cur[k] ??= {}));
  cur[ks.at(-1)] = v;
  return o;
}
const fmtShutter = (us) => (us >= 500000 ? (us / 1e6).toFixed(2) + ' s' : '1/' + Math.round(1e6 / us) + ' s');

function logPos(el, v) {
  const lo = Math.log(+el.dataset.min), hi = Math.log(+el.dataset.max);
  return Math.round(((Math.log(Math.max(v, 1)) - lo) / (hi - lo)) * 1000);
}
function logVal(el) {
  const lo = Math.log(+el.dataset.min), hi = Math.log(+el.dataset.max);
  return Math.round(Math.exp(lo + ((+el.value) / 1000) * (hi - lo)));
}

function fieldValue(el) {
  if (el.type === 'checkbox') return el.checked;
  if (el.dataset.kind === 'size') return el.value.split('x').map(Number);
  if ('log' in el.dataset) return logVal(el);
  if (el.dataset.scale) return Math.round(+el.value * +el.dataset.scale);
  if (el.type === 'range' || el.type === 'number') return +el.value;
  return el.value;
}

function setField(el, v) {
  if (v === undefined) return;
  if (el.type === 'checkbox') el.checked = v;
  else if (el.dataset.kind === 'size') el.value = v.join('x');
  else if ('log' in el.dataset) el.value = logPos(el, v);
  else if (el.dataset.scale) el.value = v / +el.dataset.scale;
  else el.value = v;
}

function updateOutput(el) {
  const out = document.querySelector(`output[data-for="${el.dataset.path}"]`);
  if (!out) return;
  const v = fieldValue(el);
  if ('log' in el.dataset) out.textContent = fmtShutter(v);
  else if (el.dataset.scale) out.textContent = (v / +el.dataset.scale).toFixed(2);
  else out.textContent = Number.isInteger(v) ? v : (+v).toFixed(2);
}

function updateEnabled() {
  const ae = $('[data-path="controls.auto_exposure"]').checked;
  const awb = $('[data-path="controls.auto_wb"]').checked;
  $$('.manual-exp').forEach((e) => (e.disabled = ae));
  $$('.auto-exp').forEach((e) => (e.disabled = !ae));
  $$('.manual-wb').forEach((e) => (e.disabled = awb));
  $$('.auto-wb').forEach((e) => (e.disabled = !awb));
  $('#lock').disabled = !(ae || awb);
}

function render(cfg) {
  config = cfg;
  $$('[data-path]').forEach((el) => {
    setField(el, getPath(cfg, el.dataset.path));
    updateOutput(el);
  });
  updateEnabled();
}

let toastTimer;
function toast(msg, isError = false) {
  const t = $('#toast');
  t.textContent = msg;
  t.classList.toggle('error', isError);
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), isError ? 5000 : 2500);
}

async function postJSON(url, body) {
  const res = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body ?? {}) });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error((data.field ? data.field + ': ' : '') + (data.error || 'HTTP ' + res.status));
  return data;
}

async function send(partial) {
  try {
    const r = await postJSON('/api/config', partial);
    render(r.config);
    if (r.applied === 'encoder' || r.applied === 'reconfigure') toast('Applied (' + r.applied + ')');
  } catch (e) {
    toast(e.message, true);
    if (config) render(config); // revert the form
  }
}

// ---------- settings form ----------
$$('[data-path]').forEach((el) => {
  el.addEventListener('input', () => updateOutput(el));
  el.addEventListener('change', () => {
    updateEnabled();
    send(setPath({}, el.dataset.path, fieldValue(el)));
  });
});

$('#reset').addEventListener('click', async () => {
  try {
    const r = await postJSON('/api/config/reset');
    render(r.config);
    toast('Defaults restored');
  } catch (e) {
    toast(e.message, true);
  }
});

$('#lock').addEventListener('click', () => {
  if (!lastMeta) return toast('No metadata yet', true);
  const c = {};
  if (config.controls.auto_exposure && lastMeta.ExposureTime) {
    Object.assign(c, {
      auto_exposure: false,
      shutter_us: Math.max(31, Math.round(lastMeta.ExposureTime)),
      gain: Math.max(1, Math.round((lastMeta.AnalogueGain || 1) * 10) / 10),
    });
  }
  if (config.controls.auto_wb && lastMeta.ColourGains) {
    Object.assign(c, {
      auto_wb: false,
      wb_gain_r: Math.round(lastMeta.ColourGains[0] * 100) / 100,
      wb_gain_b: Math.round(lastMeta.ColourGains[1] * 100) / 100,
    });
  }
  send({ controls: c });
});

// ---------- capture ----------
async function capture() {
  const btn = $('#capture');
  btn.disabled = true;
  try {
    const res = await fetch('/api/capture', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ quality: config?.still?.quality ?? 0 }),
    });
    if (!res.ok) {
      const d = await res.json().catch(() => ({}));
      throw new Error(d.error || 'HTTP ' + res.status);
    }
    const blob = await res.blob();
    const m = /filename="?([^";]+)"?/.exec(res.headers.get('Content-Disposition') || '');
    const name = m ? m[1] : 'capture.jpg';
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 10000);
    toast(`Captured ${res.headers.get('X-Capture-Width')}×${res.headers.get('X-Capture-Height')}, ${(blob.size / 1e6).toFixed(1)} MB`);
  } catch (e) {
    toast('Capture failed: ' + e.message, true);
  } finally {
    btn.disabled = false;
  }
}
$('#capture').addEventListener('click', capture);
document.addEventListener('keydown', (e) => {
  if (e.key === 'c' && !e.ctrlKey && !e.metaKey && !e.altKey && !/INPUT|SELECT|TEXTAREA/.test(document.activeElement?.tagName)) capture();
});
$('#fullscreen').addEventListener('click', () => $('.video-wrap').requestFullscreen?.());

// ---------- metadata ----------
function showMeta(md) {
  lastMeta = md;
  if (md.ExposureTime) $('#md-shutter').textContent = fmtShutter(md.ExposureTime);
  if (md.AnalogueGain) $('#md-gain').textContent = md.AnalogueGain.toFixed(2);
  if (md.ColourTemperature) $('#md-ct').textContent = Math.round(md.ColourTemperature) + ' K';
  if (md.Lux != null) $('#md-lux').textContent = Math.round(md.Lux);
  if (md.FocusFoM != null) $('#focus-val').textContent = Math.round(md.FocusFoM);
}

// ---------- player ----------
const overlay = $('#overlay');
function setOverlay(text) {
  overlay.textContent = text || '';
  overlay.hidden = !text;
}

const player = new Player($('#video'), {
  onEvent(ev) {
    if (ev.event === 'state') setOverlay(ev.state === 'streaming' ? '' : ev.state + (ev.detail ? ': ' + ev.detail : ''));
    else if (ev.event === 'metadata') showMeta(ev.data);
    else if (ev.event === 'stats') camLatency = ev.data?.latency_ms ?? null;
    else if (ev.event === 'error') toast('Camera error: ' + JSON.stringify(ev.data), true);
  },
  onStatus(s) {
    if (s !== 'connected') setOverlay(s);
  },
  onStats(s) {
    $('#stats').textContent = s.width
      ? `${s.width}×${s.height} · ${s.fps} fps · ${Math.round(s.kbps)} kbit/s · dropped ${s.dropped}` +
        (camLatency != null ? ` · camera ${Math.round(camLatency)} ms` : '')
      : `${Math.round(s.kbps)} kbit/s · waiting for keyframe`;
  },
});

if (!('VideoDecoder' in window)) {
  $('#nocodec').hidden = false;
  setOverlay('WebCodecs not available (needs HTTPS or localhost)');
}

fetch('/api/config').then((r) => r.json()).then(render).catch((e) => toast(e.message, true));
player.run();
