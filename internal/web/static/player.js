// Low-latency H.264 player: fetch('/stream') → 16-byte framed messages →
// WebCodecs VideoDecoder → <canvas>. See docs/DESIGN.md §5.
// MJPEG fallback (DESIGN.md §5.5): an <img> shows /stream.mjpeg, and
// /stream?video=0 still delivers the events. Used from the start when the
// browser has no VideoDecoder (mjpegImg), or switched to at runtime when the
// decoder exists but cannot decode the stream (fallbackImg), as on Firefox
// for Android: VideoDecoder is there, H.264 fails with NotSupportedError.
const HEADER = 16;
const T_VIDEO = 1, T_EVENT = 2;
const F_KEY = 1, F_CONFIG = 2;
const MAX_EARLY_FAILURES = 3; // decoder failures before any frame → fallback

export class Player {
  constructor(canvas, { onEvent = () => {}, onStatus = () => {}, onStats = () => {}, onFallback = () => {}, mjpegImg = null, fallbackImg = null } = {}) {
    this.canvas = canvas;
    this.mjpegImg = mjpegImg; // set: MJPEG fallback mode
    this.fallbackImg = fallbackImg; // switch to MJPEG here if H.264 decoding fails
    this.onFallback = onFallback;
    this.reader = null;
    this.reconnectNow = false;
    this.decodedTotal = 0; // frames decoded since page load
    this.earlyFailures = 0; // decoder failures while decodedTotal == 0
    this.ctx = canvas.getContext('2d');
    this.onEvent = onEvent;
    this.onStatus = onStatus;
    this.onStats = onStats;
    this.decoder = null;
    this.spsKey = '';
    this.needKey = true;
    this.pending = null; // newest decoded frame not yet drawn
    this.drawScheduled = false;
    this.frames = 0;
    this.bytes = 0;
    this.dropped = 0;
    this.width = 0;
    this.height = 0;
    this.decoder_supported = !mjpegImg && 'VideoDecoder' in window;
    setInterval(() => this.reportStats(), 1000);
  }

  async run() {
    if (this.mjpegImg) this.startMJPEG();
    for (let attempt = 0; ; attempt++) {
      if (attempt > 0 && this.mjpegImg) this.loadMJPEG(); // server may have restarted
      try {
        await this.readStream();
        if (!this.reconnectNow) this.onStatus('stream ended');
      } catch (e) {
        if (!this.reconnectNow) this.onStatus('reconnecting: ' + e.message);
      }
      this.resetDecoder();
      if (this.reconnectNow) { // switched to MJPEG: reopen /stream?video=0 now
        this.reconnectNow = false;
        attempt = -1;
        continue;
      }
      await new Promise((r) => setTimeout(r, 2000)); // fixed 2 s retry
    }
  }

  // MJPEG fallback: the browser decodes multipart JPEG in an <img> natively.
  startMJPEG() {
    const img = this.mjpegImg;
    img.hidden = false;
    this.canvas.hidden = true;
    img.addEventListener('error', () => {
      this.onStatus('MJPEG reconnecting');
      setTimeout(() => this.loadMJPEG(), 2000);
    });
    img.addEventListener('load', () => this.onStatus('connected'));
    this.loadMJPEG();
  }

  loadMJPEG() {
    this.mjpegImg.src = '/stream.mjpeg?t=' + Date.now();
  }

  // fallback switches to MJPEG in place: the H.264 decoder does not work in
  // this browser even though VideoDecoder exists.
  fallback(reason) {
    if (this.mjpegImg || !this.fallbackImg) return;
    this.resetDecoder();
    this.decoder_supported = false;
    this.mjpegImg = this.fallbackImg;
    this.onFallback(reason);
    this.startMJPEG();
    // Reconnect /stream without video, so the server stops sending H.264.
    this.reconnectNow = true;
    if (this.reader) this.reader.cancel().catch(() => {});
  }

  // decoderFailed counts decoder failures before the first decoded frame. A
  // NotSupportedError, or repeated failures, means this browser cannot decode
  // the stream at all.
  decoderFailed(e) {
    this.onStatus('decoder: ' + e.message);
    this.resetDecoder();
    if (this.decodedTotal > 0) return;
    this.earlyFailures++;
    if (e.name === 'NotSupportedError' || this.earlyFailures >= MAX_EARLY_FAILURES) {
      this.fallback(e.message || e.name);
    }
  }

  async readStream() {
    // In MJPEG mode only the events are needed from /stream.
    const res = await fetch(this.mjpegImg ? '/stream?video=0' : '/stream', { cache: 'no-store' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    this.onStatus('connected');
    const reader = res.body.getReader();
    this.reader = reader;
    let buf = new Uint8Array(0);
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      buf = buf.length ? concat(buf, value) : value;
      let off = 0;
      while (buf.length - off >= HEADER) {
        const dv = new DataView(buf.buffer, buf.byteOffset + off, HEADER);
        const len = dv.getUint32(4);
        if (buf.length - off < HEADER + len) break;
        const type = dv.getUint8(0), flags = dv.getUint8(1);
        const ts = Number(dv.getBigUint64(8));
        const payload = buf.subarray(off + HEADER, off + HEADER + len);
        this.handle(type, flags, ts, payload);
        off += HEADER + len;
      }
      buf = off === buf.length ? new Uint8Array(0) : buf.slice(off);
    }
  }

  handle(type, flags, ts, payload) {
    if (type === T_EVENT) {
      let ev;
      try { ev = JSON.parse(new TextDecoder().decode(payload)); } catch { return; }
      if (ev.event === 'drop') this.dropped = ev.total;
      if (ev.event === 'stream') this.needKey = true;
      this.onEvent(ev);
      return;
    }
    if (type !== T_VIDEO || !this.decoder_supported) return;
    this.bytes += payload.length;
    const key = (flags & F_KEY) !== 0;
    if (flags & F_CONFIG) this.maybeConfigure(payload);
    if (!this.decoder || this.decoder.state !== 'configured') return;
    if (this.needKey && !key) return;
    this.needKey = false;
    try {
      this.decoder.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: ts, data: payload }));
    } catch (e) {
      this.decoderFailed(e);
    }
  }

  maybeConfigure(au) {
    const sps = findSPS(au);
    if (!sps) return;
    const k = Array.from(sps.subarray(0, 32)).join(',');
    if (k === this.spsKey && this.decoder && this.decoder.state === 'configured') return;
    this.resetDecoder();
    this.spsKey = k;
    const codec = 'avc1.' + [sps[1], sps[2], sps[3]].map((b) => b.toString(16).padStart(2, '0')).join('');
    const decoder = new VideoDecoder({
      output: (frame) => this.onFrame(frame),
      error: (e) => { if (this.decoder === decoder) this.decoderFailed(e); },
    });
    this.decoder = decoder;
    // No `description`: chunks are Annex-B. hardwareAcceleration is left at
    // its default: Chrome treats 'prefer-hardware' as hardware-only and fails
    // on machines without a hardware H.264 decoder; the default still uses
    // hardware when available.
    const cfg = { codec, optimizeForLatency: true };
    try {
      decoder.configure(cfg);
    } catch (e) {
      this.decoderFailed(e);
      return;
    }
    this.codec = codec;
    // Ask early whether this config can be decoded at all; an unsupported
    // config otherwise only shows up as a NotSupportedError on the decoder.
    if (this.decodedTotal === 0 && VideoDecoder.isConfigSupported) {
      VideoDecoder.isConfigSupported(cfg).then((r) => {
        if (!r.supported && this.decodedTotal === 0) this.fallback('H.264 ' + codec + ' not supported');
      }).catch(() => {});
    }
    this.needKey = true;
  }

  resetDecoder() {
    if (this.decoder && this.decoder.state !== 'closed') {
      try { this.decoder.close(); } catch {}
    }
    this.decoder = null;
    this.spsKey = '';
    this.needKey = true;
  }

  onFrame(frame) {
    this.decodedTotal++;
    if (this.pending) this.pending.close(); // only the newest frame is drawn
    this.pending = frame;
    if (!this.drawScheduled) {
      this.drawScheduled = true;
      requestAnimationFrame(() => this.draw());
    }
  }

  draw() {
    this.drawScheduled = false;
    const f = this.pending;
    if (!f) return;
    this.pending = null;
    if (this.canvas.width !== f.displayWidth || this.canvas.height !== f.displayHeight) {
      this.canvas.width = f.displayWidth;
      this.canvas.height = f.displayHeight;
    }
    this.width = f.displayWidth;
    this.height = f.displayHeight;
    this.ctx.drawImage(f, 0, 0);
    f.close();
    this.frames++;
  }

  reportStats() {
    if (this.mjpegImg) {
      this.onStats({ mjpeg: true, width: this.mjpegImg.naturalWidth, height: this.mjpegImg.naturalHeight });
      return;
    }
    this.onStats({ fps: this.frames, kbps: (this.bytes * 8) / 1000, width: this.width, height: this.height, dropped: this.dropped, codec: this.codec });
    this.frames = 0;
    this.bytes = 0;
  }
}

function concat(a, b) {
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

// findSPS returns the first SPS NAL unit (without start code) in an Annex-B AU.
export function findSPS(b) {
  let start = -1;
  for (let i = 0; i + 2 < b.length; i++) {
    if (b[i] === 0 && b[i + 1] === 0 && b[i + 2] === 1) {
      if (start >= 0 && (b[start] & 0x1f) === 7) {
        let end = i;
        if (b[end - 1] === 0) end--;
        return b.subarray(start, end);
      }
      start = i + 3;
      i += 2;
    }
  }
  if (start >= 0 && start < b.length && (b[start] & 0x1f) === 7) return b.subarray(start);
  return null;
}
