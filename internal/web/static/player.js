// Live player with two modes:
//   'h264'  fetch('/stream') → 16-byte framed messages → WebCodecs
//           VideoDecoder → <canvas> (docs/ARCHITECTURE.md §5.2)
//   'mjpeg' an <img> shows /stream.mjpeg; fetch('/stream?video=0') still
//           delivers the events (ARCHITECTURE.md §5.3)
// The page starts in 'mjpeg' when the browser has no VideoDecoder. In 'h264'
// the player switches to 'mjpeg' in place if the decoder cannot decode the
// stream before showing any frame (Firefox for Android: VideoDecoder exists,
// H.264 fails with NotSupportedError).
const HEADER = 16;
const T_VIDEO = 1, T_EVENT = 2;
const F_KEY = 1, F_CONFIG = 2;
const MAX_EARLY_FAILURES = 3; // decoder failures before any frame → MJPEG
const RETRY_MS = 2000; // fixed reconnect interval

export class Player {
  constructor({ canvas, img, mjpeg = false, onEvent = () => {}, onStatus = () => {}, onStats = () => {}, onFallback = () => {} }) {
    this.canvas = canvas;
    this.img = img;
    this.mode = mjpeg ? 'mjpeg' : 'h264';
    this.onEvent = onEvent;
    this.onStatus = onStatus;
    this.onStats = onStats;
    this.onFallback = onFallback;
    this.reader = null; // current /stream reader, cancelled on a mode switch
    // H.264 decoding
    this.ctx = canvas.getContext('2d');
    this.decoder = null;
    this.spsKey = '';
    this.needKey = true;
    this.pending = null; // newest decoded frame not yet drawn
    this.drawScheduled = false;
    this.decodedTotal = 0; // frames decoded since page load
    this.earlyFailures = 0; // decoder failures while decodedTotal == 0
    // stats, reported every second
    this.frames = 0;
    this.bytes = 0;
    this.dropped = 0;
    this.width = 0;
    this.height = 0;
    setInterval(() => this.reportStats(), 1000);
  }

  async run() {
    if (this.mode === 'mjpeg') this.showMJPEG();
    for (;;) {
      const mode = this.mode;
      try {
        await this.readStream(mode);
        if (this.mode === mode) this.onStatus('stream ended');
      } catch (e) {
        if (this.mode === mode) this.onStatus('reconnecting: ' + e.message);
      }
      this.resetDecoder();
      if (this.mode !== mode) continue; // switched to MJPEG: reopen /stream?video=0 at once
      await new Promise((r) => setTimeout(r, RETRY_MS));
      if (this.mode === 'mjpeg') this.loadMJPEG(); // the server may have restarted
    }
  }

  // switchToMJPEG changes to the MJPEG fallback in place: the H.264 decoder
  // does not work in this browser even though VideoDecoder exists.
  switchToMJPEG(reason) {
    if (this.mode === 'mjpeg') return;
    this.mode = 'mjpeg';
    this.resetDecoder();
    this.onFallback(reason);
    this.showMJPEG();
    // End the current /stream; run() reconnects without video.
    this.reader?.cancel().catch(() => {});
  }

  // The browser decodes multipart JPEG in an <img> natively.
  showMJPEG() {
    this.img.hidden = false;
    this.canvas.hidden = true;
    this.img.addEventListener('error', () => {
      this.onStatus('MJPEG reconnecting');
      setTimeout(() => this.loadMJPEG(), RETRY_MS);
    });
    this.img.addEventListener('load', () => this.onStatus('connected'));
    this.loadMJPEG();
  }

  loadMJPEG() {
    this.img.src = '/stream.mjpeg?t=' + Date.now();
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
      this.switchToMJPEG(e.message || e.name);
    }
  }

  async readStream(mode) {
    // In MJPEG mode only the events are needed from /stream.
    const res = await fetch(mode === 'mjpeg' ? '/stream?video=0' : '/stream', { cache: 'no-store' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    this.onStatus('connected');
    const reader = res.body.getReader();
    this.reader = reader;
    if (this.mode !== mode) { // switched while connecting
      reader.cancel().catch(() => {});
      return;
    }
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
    if (type !== T_VIDEO || this.mode !== 'h264') return;
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
        if (!r.supported && this.decodedTotal === 0) this.switchToMJPEG('H.264 ' + codec + ' not supported');
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
    if (this.mode === 'mjpeg') {
      this.onStats({ mjpeg: true, width: this.img.naturalWidth, height: this.img.naturalHeight });
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
