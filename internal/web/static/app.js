import { Player } from './player.js';

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

let config = null;
let lastMeta = null;
let camLatency = null; // helper: sensor timestamp -> socket write, ms
let camera = null; // "camera" event: model, sensor_max_gain

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
  describeProfile();
  describeDenoise();
  describeAwb();
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
  if (md.ExposureTime) $('#md-shutter').textContent = fmtShutter(md.ExposureTime) + ` (${ms(md.ExposureTime)})`;
  if (md.AnalogueGain) $('#md-gain').textContent = md.AnalogueGain.toFixed(2);
  if (md.DigitalGain) $('#md-dgain').textContent = md.DigitalGain.toFixed(2);
  if (md.FrameDuration) $('#md-frame').textContent = `${ms(md.FrameDuration)} (${(1e6 / md.FrameDuration).toFixed(1)} fps)`;
  if (md.AeState != null) $('#md-aestate').textContent = AE_STATES[md.AeState] ?? md.AeState;
  checkAeLimit(md);
  if (md.ColourTemperature) $('#md-ct').textContent = Math.round(md.ColourTemperature) + ' K';
  if (md.Lux != null) $('#md-lux').textContent = Math.round(md.Lux);
  if (md.FocusFoM != null) $('#focus-val').textContent = Math.round(md.FocusFoM);
}

// ---------- explanations (docs/DESIGN.md §3.5-§3.10) ----------
// The UI does not read the camera tuning file. Popups explain the behaviour
// in general and name the tuning parameter (e.g. rpi.agc → exposure_modes)
// that sets the actual values for the camera in use.
const AE_STATES = { 0: 'Idle', 1: 'Searching', 2: 'Converged' }; // libcamera AeState values
const ms = (us) => (us / 1000).toFixed(1).replace(/\.0$/, '') + ' ms';

function fillCameraInfo() {
  if (camera?.sensor_max_gain) $$('[data-fill="sensor-max-gain"]').forEach((e) => (e.textContent = camera.sensor_max_gain.toFixed(2)));
}

// Pi 4 AGC metering zones (Raspberry Pi Camera Algorithm and Tuning Guide,
// §5.9.3, Figure 9) on a 5-row × 7-column grid: [zone, rowStart, rowEnd,
// colStart, colEnd]. Drawn with zone numbers; the weights per zone are
// rpi.agc → metering_modes → <mode> → weights in the tuning file.
const METERING_ZONES = [
  [11, 1, 2, 1, 2], [9, 1, 2, 2, 7], [12, 1, 2, 7, 8],
  [7, 2, 5, 1, 2], [5, 2, 5, 2, 3], [3, 2, 3, 3, 6], [1, 3, 4, 3, 4], [0, 3, 4, 4, 5],
  [2, 3, 4, 5, 6], [4, 4, 5, 3, 6], [6, 2, 5, 6, 7], [8, 2, 5, 7, 8],
  [13, 5, 6, 1, 2], [10, 5, 6, 2, 7], [14, 5, 6, 7, 8],
];
function drawZoneLayout() {
  const cells = METERING_ZONES.map(([z, r1, r2, c1, c2]) => `<div style="grid-area:${r1}/${c1}/${r2}/${c2}">${z}</div>`).join('');
  $$('[data-fill="metering-zones"]').forEach((e) => (e.innerHTML = `<div class="zone-grid zone-layout">${cells}</div>`));
}
drawZoneLayout();

// Denoise settings -> what the Pi 4 ISP does (libcamera vc4.cpp DenoiseModeTable).
const DENOISE_DESC = {
  auto: 'Spatial denoise + fast colour denoise (same as Picamera2\'s video default).',
  off: 'No noise reduction.',
  minimal: 'Spatial denoise only, no colour denoise.',
  fast: 'Spatial denoise + fast colour denoise.',
  high_quality: 'Spatial denoise + high-quality colour denoise (more processing).',
};
function describeDenoise() {
  const el = $('#denoise-desc');
  if (el && config) el.textContent = DENOISE_DESC[config.controls.denoise] ?? '';
}

// Line under the exposure profile select: where the steps are defined, and
// the frame-time cap (which does not depend on the tuning file).
function describeProfile() {
  const el = $('#profile-desc');
  if (!el || !config) return;
  const name = config.controls.exposure_mode;
  el.textContent = `Steps: tuning file rpi.agc → exposure_modes → ${name}. ` +
    `At ${config.controls.fps} fps the shutter cannot exceed ${ms(1e6 / config.controls.fps)} (frame time).`;
}

// Line under the white balance mode select.
function describeAwb() {
  const el = $('#awb-desc');
  if (el && config) el.textContent = `Range: tuning file rpi.awb → modes → ${config.controls.awb_mode} → lo … hi (kelvin).`;
}

// Auto exposure using the longest shutter the frame time allows: from here
// only gain can brighten the image, up to the profile's last gain step.
function checkAeLimit(md) {
  const box = $('#ae-limit');
  const c = config?.controls;
  if (!c?.auto_exposure || !md.ExposureTime || !md.FrameDuration || md.AeState === 0 ||
      md.ExposureTime < 0.95 * md.FrameDuration) {
    box.hidden = true;
    return;
  }
  box.hidden = false;
  box.textContent = `Shutter at the frame-time limit (${ms(md.FrameDuration)} at ${(1e6 / md.FrameDuration).toFixed(0)} fps); ` +
    `only gain (now ${(md.AnalogueGain || 0).toFixed(1)}) can brighten the image further. If the gain has reached the last step of ` +
    `rpi.agc → exposure_modes → ${c.exposure_mode} → gain, the image cannot get brighter in auto mode and EV has no effect: ` +
    `lower the frame rate or use manual exposure.`;
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
    else if (ev.event === 'camera') {
      camera = ev;
      fillCameraInfo();
    }
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
