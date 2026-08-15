/* Read-only embedded canvas. Deliberately tiny: it subscribes, it draws, and it
 * does nothing else — no cookies it depends on, no input, no moderation. An
 * embed on somebody else's page should not be able to do anything on their
 * visitors' behalf. */
'use strict';

(() => {

// The slug travels on <body data-slug>, not an inline <script>. Inline scripts
// would need 'unsafe-inline' in the Content Security Policy, and weakening the
// policy for one string is a bad trade.
const SLUG = document.body.dataset.slug || '';
const API = '/api/r/' + SLUG;

const stage = document.getElementById('stage');
const board = document.getElementById('board');
const ctx = board.getContext('2d', { alpha: false });
const off = document.createElement('canvas');
const offCtx = off.getContext('2d', { alpha: false, willReadFrequently: true });

let W = 0, H = 0, pixels = null, image = null, rgb = [], dirty = true;
let socket = null, retry = 800;

const hexToRGB = (hex) => {
  const n = parseInt(hex.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
};

function write(x, y, c) {
  if (x < 0 || y < 0 || x >= W || y >= H) return;
  pixels[y * W + x] = c;
  const col = rgb[c] || rgb[0];
  const o = (y * W + x) * 4;
  image.data[o] = col[0]; image.data[o + 1] = col[1]; image.data[o + 2] = col[2]; image.data[o + 3] = 255;
  dirty = true;
}

function repaint() {
  for (let i = 0; i < pixels.length; i++) {
    const col = rgb[pixels[i]] || rgb[0];
    const o = i * 4;
    image.data[o] = col[0]; image.data[o + 1] = col[1]; image.data[o + 2] = col[2]; image.data[o + 3] = 255;
  }
  dirty = true;
}

function resize() {
  const r = stage.getBoundingClientRect();
  const dpr = Math.min(window.devicePixelRatio || 1, 2);
  board.width = Math.max(1, Math.floor(r.width * dpr));
  board.height = Math.max(1, Math.floor(r.height * dpr));
  board.style.width = r.width + 'px';
  board.style.height = r.height + 'px';
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  dirty = true;
}

function draw() {
  requestAnimationFrame(draw);
  if (!dirty || !image) return;
  dirty = false;

  offCtx.putImageData(image, 0, 0);
  const r = stage.getBoundingClientRect();
  ctx.imageSmoothingEnabled = false;
  ctx.fillStyle = '#0a0b10';
  ctx.fillRect(0, 0, r.width, r.height);

  // Always fit: an embed has no controls, so it has to show the whole canvas.
  const scale = Math.min(r.width / W, r.height / H);
  const w = W * scale, h = H * scale;
  ctx.drawImage(off, 0, 0, W, H, (r.width - w) / 2, (r.height - h) / 2, w, h);
}

async function loadSnapshot() {
  const res = await fetch(API + '/snapshot', { cache: 'no-store' });
  if (!res.ok) throw new Error('snapshot ' + res.status);
  const buf = new Uint8Array(await res.arrayBuffer());
  if (buf.length < 16 || buf[0] !== 80 || buf[1] !== 88 || buf[2] !== 70 || buf[3] !== 49) {
    throw new Error('unexpected snapshot format');
  }
  const w = (buf[4] << 8) | buf[5];
  const h = (buf[6] << 8) | buf[7];
  const body = buf.subarray(16);
  if (body.length !== w * h) throw new Error('snapshot payload is the wrong size');

  if (!pixels || w !== W || h !== H) {
    W = w; H = h;
    off.width = w; off.height = h;
    image = offCtx.createImageData(w, h);
    pixels = new Uint8Array(w * h);
  }
  pixels.set(body);
  repaint();
}

function connect() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  try {
    socket = new WebSocket(proto + '//' + location.host + API + '/ws');
  } catch (e) {
    return; // a static preview is still better than a broken frame
  }
  socket.binaryType = 'arraybuffer';
  socket.onopen = () => { retry = 800; loadSnapshot().catch(() => {}); };
  socket.onmessage = (ev) => {
    if (typeof ev.data === 'string') return; // control messages do not matter here
    const b = new Uint8Array(ev.data);
    if (b.length < 3 || b[0] !== 0x01) return;
    const count = (b[1] << 8) | b[2];
    let o = 3;
    for (let i = 0; i < count && o + 4 < b.length + 1; i++) {
      write((b[o] << 8) | b[o + 1], (b[o + 2] << 8) | b[o + 3], b[o + 4]);
      o += 5;
    }
  };
  socket.onclose = () => {
    socket = null;
    setTimeout(connect, retry);
    retry = Math.min(retry * 1.8, 20000);
  };
}

async function boot() {
  try {
    const cfg = await (await fetch(API + '/config')).json();
    rgb = (cfg.room.palette || []).map(hexToRGB);
    await loadSnapshot();
  } catch (e) {
    return;
  }
  resize();
  window.addEventListener('resize', resize);
  draw();
  connect();
  // A framed page can be hidden for a long time; refetch on return so it is
  // never quietly stale.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) loadSnapshot().catch(() => {});
  });
}

boot();

})();
