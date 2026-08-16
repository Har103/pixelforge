/* Pixelforge front end.
 *
 * No framework, no build step, no dependencies - the same rule the server
 * follows. Rendering is a single <canvas>: the authoritative grid lives in an
 * offscreen buffer at native resolution and is blitted with smoothing disabled,
 * so zooming is a scale factor rather than a redraw of every cell.
 */
'use strict';

(() => {

// Every endpoint is scoped to the room in the URL. The slug is injected by the
// server into the page rather than parsed out of location, so a rewritten path
// or a trailing slash cannot change which canvas the client talks to.
// The slug travels on <body data-slug>, not an inline <script>. Inline scripts
// would need 'unsafe-inline' in the Content Security Policy, and weakening the
// policy for one string is a bad trade.
const SLUG = document.body.dataset.slug || '';
const API = '/api/r/' + SLUG;

// ------------------------------------------------------------------ state --

const S = {
  cfg: null,
  W: 0, H: 0,
  pixels: null,          // Uint8Array, one palette index per cell
  seq: 0,
  palette: [],
  rgb: [],               // palette parsed to [r,g,b] for fast writes
  color: 1,
  cooldownMs: 750,
  readyAt: 0,
  view: { scale: 3, ox: 0, oy: 0 },
  hover: null,
  transport: 'ws',
  socket: null,
  events: null,
  connected: false,
  reconnectDelay: 500,
  reconnectTimer: null,
  lapse: null,           // { entries, index, playing, base }
  placements: 0,
  dirty: true,
  room: null,
  owner: false,
  paused: false,
  locks: [],
  cursors: [],          // other people's pointers, ephemeral
  cursorSent: 0,
  cursorCell: null,     // the last cell we told the server about
  uid: '',              // our own painter id, so we can drop our own cursor
  template: null,       // the ghost image being traced over
  templateOn: true,
  templateSrc: null,    // the decoded image, kept so re-dithering needs no reload
  tplMove: false,       // dragging the template into position
  inspect: null,        // the cell whose provenance is on screen
  minimap: true,
};

const $ = (id) => document.getElementById(id);

const el = {
  stage: $('stage'), board: $('board'), boot: $('boot'), bootMsg: $('bootMsg'),
  coords: $('coords'), zoomLabel: $('zoomLabel'), palette: $('palette'),
  connDot: $('connDot'), connLabel: $('connLabel'), btnTransport: $('btnTransport'),
  statClients: $('statClients'), statPainted: $('statPainted'), statPlacements: $('statPlacements'),
  feedList: $('feedList'), cdFill: $('cdFill'), cooldownText: $('cooldownText'),
  toast: $('toast'), sheet: $('sheet'), sheetBody: $('sheetBody'),
  timelapse: $('timelapse'), scrub: $('scrub'), timelapseLabel: $('timelapseLabel'),
  btnPlay: $('btnPlay'), pausedBadge: $('pausedBadge'),
  roomName: $('roomName'), roomSub: $('roomSub'),
  btnOwner: $('btnOwner'), btnShare: $('btnShare'), btnGif: $('btnGif'),
  minimap: $('minimap'), feedEmpty: $('feedEmpty'),
  tplPanel: $('tplPanel'), tplDrop: $('tplDrop'), tplFile: $('tplFile'),
  tplLoaded: $('tplLoaded'), tplMode: $('tplMode'), tplAvoidBg: $('tplAvoidBg'),
  tplSize: $('tplSize'), tplPercent: $('tplPercent'), tplBar: $('tplBar'),
  tplCount: $('tplCount'), tplErr: $('tplErr'),
  btnTplMove: $('btnTplMove'), btnTplToggle: $('btnTplToggle'),
  inspectPanel: $('inspectPanel'), inspectTitle: $('inspectTitle'), inspectBody: $('inspectBody'),
  btnUndo: $('btnUndo'),
};

const ctx = el.board.getContext('2d', { alpha: false });
const off = document.createElement('canvas');
const offCtx = off.getContext('2d', { alpha: false, willReadFrequently: true });
let image = null;

// ------------------------------------------------------------------ utils --

const clamp = (v, lo, hi) => v < lo ? lo : v > hi ? hi : v;

function hexToRGB(hex) {
  const n = parseInt(hex.slice(1), 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

function fmt(n) {
  if (n === null || n === undefined) return '–';
  if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
}

let toastTimer = null;
function toast(msg, kind) {
  el.toast.textContent = msg;
  el.toast.className = 'toast show' + (kind ? ' ' + kind : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.toast.className = 'toast'; }, 2200);
}

// Painter colours are derived from the id rather than assigned, so the same
// person is the same colour for everyone watching, with no coordination.
const CURSOR_HUES = [12, 32, 52, 92, 152, 172, 192, 212, 258, 288, 318, 338];
function hueFor(uid) {
  let h = 0;
  for (let i = 0; i < uid.length; i++) h = (h * 31 + uid.charCodeAt(i)) >>> 0;
  return CURSOR_HUES[h % CURSOR_HUES.length];
}
const cursorColour = (uid) => `hsl(${hueFor(uid)} 85% 62%)`;

// -------------------------------------------------------------- rendering --

function writePixel(x, y, colorIndex) {
  if (x < 0 || y < 0 || x >= S.W || y >= S.H) return;
  S.pixels[y * S.W + x] = colorIndex;
  const rgb = S.rgb[colorIndex] || S.rgb[0];
  const o = (y * S.W + x) * 4;
  image.data[o] = rgb[0];
  image.data[o + 1] = rgb[1];
  image.data[o + 2] = rgb[2];
  image.data[o + 3] = 255;
  S.dirty = true;
}

function repaintAll() {
  for (let i = 0; i < S.pixels.length; i++) {
    const rgb = S.rgb[S.pixels[i]] || S.rgb[0];
    const o = i * 4;
    image.data[o] = rgb[0];
    image.data[o + 1] = rgb[1];
    image.data[o + 2] = rgb[2];
    image.data[o + 3] = 255;
  }
  S.dirty = true;
}

function resize() {
  const r = el.stage.getBoundingClientRect();
  const dpr = Math.min(window.devicePixelRatio || 1, 2);
  el.board.width = Math.max(1, Math.floor(r.width * dpr));
  el.board.height = Math.max(1, Math.floor(r.height * dpr));
  el.board.style.width = r.width + 'px';
  el.board.style.height = r.height + 'px';
  el.board.style.left = '0px';
  el.board.style.top = '0px';
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  S.dirty = true;
}

function render() {
  requestAnimationFrame(render);
  if (!S.dirty || !image) return;
  S.dirty = false;

  offCtx.putImageData(image, 0, 0);

  const r = el.stage.getBoundingClientRect();
  ctx.imageSmoothingEnabled = false;
  ctx.fillStyle = '#0a0b10';
  ctx.fillRect(0, 0, r.width, r.height);

  const { scale, ox, oy } = S.view;
  const w = S.W * scale, h = S.H * scale;

  // Canvas shadow / border
  ctx.save();
  ctx.shadowColor = 'rgba(0,0,0,.8)';
  ctx.shadowBlur = 40;
  ctx.shadowOffsetY = 14;
  ctx.fillStyle = '#12141c';
  ctx.fillRect(ox, oy, w, h);
  ctx.restore();

  ctx.drawImage(off, 0, 0, S.W, S.H, ox, oy, w, h);

  // A faint grid only once cells are big enough for it to read as structure
  // rather than noise.
  if (scale >= 8) {
    ctx.strokeStyle = 'rgba(255,255,255,.045)';
    ctx.lineWidth = 1;
    ctx.beginPath();
    const x0 = Math.max(0, Math.floor(-ox / scale));
    const x1 = Math.min(S.W, Math.ceil((r.width - ox) / scale));
    const y0 = Math.max(0, Math.floor(-oy / scale));
    const y1 = Math.min(S.H, Math.ceil((r.height - oy) / scale));
    for (let x = x0; x <= x1; x++) {
      const px = Math.round(ox + x * scale) + 0.5;
      ctx.moveTo(px, oy + y0 * scale); ctx.lineTo(px, oy + y1 * scale);
    }
    for (let y = y0; y <= y1; y++) {
      const py = Math.round(oy + y * scale) + 0.5;
      ctx.moveTo(ox + x0 * scale, py); ctx.lineTo(ox + x1 * scale, py);
    }
    ctx.stroke();
  }

  // The template ghost sits under everything else: it is a guide, so it must
  // never be mistaken for a pixel somebody actually painted.
  if (S.template && S.templateOn) {
    const t = S.template;
    if (!t._canvas) {
      t._canvas = document.createElement('canvas');
      t._canvas.width = t.width; t._canvas.height = t.height;
      t._ctx = t._canvas.getContext('2d');
    }
    if (t._dirty !== false) {
      const rgba = t.rgba(S.rgb);
      const img = t._ctx.createImageData(t.width, t.height);
      img.data.set(rgba);
      t._ctx.putImageData(img, 0, 0);
      t._dirty = false;
    }
    ctx.save();
    ctx.globalAlpha = 0.45;
    ctx.imageSmoothingEnabled = false;
    ctx.drawImage(t._canvas, 0, 0, t.width, t.height,
      ox + t.x * scale, oy + t.y * scale, t.width * scale, t.height * scale);
    ctx.restore();
    ctx.save();
    ctx.strokeStyle = 'rgba(58,134,255,.75)';
    ctx.setLineDash([4, 3]);
    ctx.lineWidth = 1;
    ctx.strokeRect(ox + t.x * scale - .5, oy + t.y * scale - .5,
      t.width * scale + 1, t.height * scale + 1);
    ctx.restore();
  }

  // Locked regions, hatched so they read as off-limits rather than decorative
  if (S.locks.length) {
    ctx.save();
    ctx.strokeStyle = 'rgba(255,159,28,.55)';
    ctx.lineWidth = 1.5;
    ctx.setLineDash([5, 4]);
    S.locks.forEach((l) => {
      const x1 = Math.min(l.X1, l.X2), y1 = Math.min(l.Y1, l.Y2);
      const x2 = Math.max(l.X1, l.X2), y2 = Math.max(l.Y1, l.Y2);
      ctx.strokeRect(ox + x1 * scale, oy + y1 * scale,
                     (x2 - x1 + 1) * scale, (y2 - y1 + 1) * scale);
    });
    ctx.restore();
  }

  // The rectangle being dragged out in lock mode
  if (S.lockMode && S.lockStart && S.lockEnd) {
    const x1 = Math.min(S.lockStart.x, S.lockEnd.x), y1 = Math.min(S.lockStart.y, S.lockEnd.y);
    const x2 = Math.max(S.lockStart.x, S.lockEnd.x), y2 = Math.max(S.lockStart.y, S.lockEnd.y);
    ctx.save();
    ctx.fillStyle = 'rgba(255,159,28,.16)';
    ctx.strokeStyle = '#ff9f1c';
    ctx.lineWidth = 2;
    ctx.fillRect(ox + x1 * scale, oy + y1 * scale, (x2 - x1 + 1) * scale, (y2 - y1 + 1) * scale);
    ctx.strokeRect(ox + x1 * scale, oy + y1 * scale, (x2 - x1 + 1) * scale, (y2 - y1 + 1) * scale);
    ctx.restore();
  }

  // Hover cell
  if (S.hover && !S.lapse) {
    const hx = ox + S.hover.x * scale, hy = oy + S.hover.y * scale;
    ctx.strokeStyle = '#ffffff';
    ctx.lineWidth = 2;
    ctx.strokeRect(hx - 1, hy - 1, scale + 2, scale + 2);
    ctx.strokeStyle = 'rgba(0,0,0,.65)';
    ctx.lineWidth = 1;
    ctx.strokeRect(hx - 2.5, hy - 2.5, scale + 5, scale + 5);
  }

  // Canvas outline
  ctx.strokeStyle = 'rgba(255,255,255,.10)';
  ctx.lineWidth = 1;
  ctx.strokeRect(ox - 0.5, oy - 0.5, w + 1, h + 1);

  // Other people's pointers. Drawn last so they are never hidden by the art,
  // and drawn as a filled cell plus a caret so the target is unambiguous even
  // when two people hover the same spot.
  if (S.cursors.length) {
    ctx.save();
    S.cursors.forEach((c) => {
      const cx = ox + c.x * scale, cy = oy + c.y * scale;
      if (cx < -scale || cy < -scale || cx > r.width || cy > r.height) return;
      const col = cursorColour(c.u);
      ctx.fillStyle = S.palette[c.k] || '#fff';
      ctx.globalAlpha = 0.55;
      ctx.fillRect(cx, cy, scale, scale);
      ctx.globalAlpha = 1;
      ctx.strokeStyle = col;
      ctx.lineWidth = 2;
      ctx.strokeRect(cx - 1, cy - 1, scale + 2, scale + 2);
      // a small caret pointing at the cell, so a 1px cell is still findable
      ctx.beginPath();
      ctx.moveTo(cx + scale + 2, cy + scale + 2);
      ctx.lineTo(cx + scale + 11, cy + scale + 7);
      ctx.lineTo(cx + scale + 7, cy + scale + 11);
      ctx.closePath();
      ctx.fillStyle = col;
      ctx.fill();
    });
    ctx.restore();
  }

  // The cell whose provenance is open, marked so the panel and the grid agree.
  if (S.inspect) {
    const ix = ox + S.inspect.x * scale, iy = oy + S.inspect.y * scale;
    ctx.save();
    ctx.strokeStyle = '#ffd60a';
    ctx.lineWidth = 2;
    ctx.setLineDash([4, 3]);
    ctx.strokeRect(ix - 1.5, iy - 1.5, scale + 3, scale + 3);
    ctx.restore();
  }

  drawMinimap(r);
}

// A minimap only earns its space once the canvas is bigger than the viewport.
// Below that it is a second, worse copy of what you are already looking at.
function drawMinimap(r) {
  const el = document.getElementById('minimap');
  if (!el) return;
  const worth = S.minimap && (S.W * S.view.scale > r.width || S.H * S.view.scale > r.height);
  el.hidden = !worth;
  if (!worth) return;

  const m = el.getContext('2d');
  const size = 128;
  if (el.width !== size) { el.width = size; el.height = size; }
  const s = Math.min(size / S.W, size / S.H);
  const w = S.W * s, h = S.H * s;
  const px = (size - w) / 2, py = (size - h) / 2;

  m.fillStyle = '#0a0b10';
  m.fillRect(0, 0, size, size);
  m.imageSmoothingEnabled = false;
  m.drawImage(off, 0, 0, S.W, S.H, px, py, w, h);

  // the part of the canvas currently on screen
  const vx = px + (-S.view.ox / S.view.scale) * s;
  const vy = py + (-S.view.oy / S.view.scale) * s;
  const vw = (r.width / S.view.scale) * s;
  const vh = (r.height / S.view.scale) * s;
  m.strokeStyle = '#ffffff';
  m.lineWidth = 1;
  m.strokeRect(Math.max(px, vx) + .5, Math.max(py, vy) + .5,
    Math.min(vw, w), Math.min(vh, h));
}

function fitToScreen() {
  const r = el.stage.getBoundingClientRect();
  const pad = 48;
  const scale = Math.max(1, Math.min((r.width - pad) / S.W, (r.height - pad) / S.H));
  S.view.scale = scale;
  S.view.ox = (r.width - S.W * scale) / 2;
  S.view.oy = (r.height - S.H * scale) / 2;
  updateZoomLabel();
  S.dirty = true;
}

function zoomAt(factor, cx, cy) {
  const r = el.stage.getBoundingClientRect();
  if (cx === undefined) { cx = r.width / 2; cy = r.height / 2; }
  const old = S.view.scale;
  const next = clamp(old * factor, 0.5, 48);
  if (next === old) return;
  // Keep the point under the cursor fixed while the scale changes.
  S.view.ox = cx - (cx - S.view.ox) * (next / old);
  S.view.oy = cy - (cy - S.view.oy) * (next / old);
  S.view.scale = next;
  updateZoomLabel();
  S.dirty = true;
}

function updateZoomLabel() {
  const s = S.view.scale;
  el.zoomLabel.textContent = (s >= 10 ? Math.round(s) : s.toFixed(1)) + '×';
}

function screenToCell(clientX, clientY) {
  const r = el.stage.getBoundingClientRect();
  const x = Math.floor((clientX - r.left - S.view.ox) / S.view.scale);
  const y = Math.floor((clientY - r.top - S.view.oy) / S.view.scale);
  if (x < 0 || y < 0 || x >= S.W || y >= S.H) return null;
  return { x, y };
}

// ----------------------------------------------------------------- palette --

function buildPalette() {
  el.palette.innerHTML = '';
  S.palette.forEach((hex, i) => {
    const b = document.createElement('button');
    b.className = 'swatch' + (i === S.color ? ' sel' : '');
    b.style.background = hex;
    b.type = 'button';
    b.setAttribute('role', 'radio');
    b.setAttribute('aria-checked', String(i === S.color));
    b.title = i === 0 ? 'background (' + hex + ')' : hex + (i <= 10 ? '  ·  key ' + (i % 10) : '');
    b.addEventListener('click', () => selectColor(i));
    el.palette.appendChild(b);
  });
}

// The dither modes come from template.js rather than being duplicated here, so
// adding one is a single edit. If that script failed to load the feature is
// removed from the page instead of offering a button that throws.
function buildTemplateModes() {
  if (!window.PFTemplate) {
    $('btnTemplate').hidden = true;
    el.tplPanel.hidden = true;
    return;
  }
  const modes = window.PFTemplate.modes || [];
  modes.forEach((m) => {
    const o = document.createElement('option');
    o.value = m.key;
    o.textContent = m.name;
    if (m.note) o.title = m.note;
    el.tplMode.appendChild(o);
  });
  // Floyd–Steinberg by default: it is the one that makes a photograph look like
  // the photograph, and somebody tracing line art will change it once and know
  // why.
  el.tplMode.value = modes.some((m) => m.key === 'floyd-steinberg')
    ? 'floyd-steinberg'
    : (modes[0] && modes[0].key) || 'none';
}

function selectColor(i) {
  if (i < 0 || i >= S.palette.length) return;
  S.color = i;
  [...el.palette.children].forEach((c, idx) => {
    c.classList.toggle('sel', idx === i);
    c.setAttribute('aria-checked', String(idx === i));
  });
}

// --------------------------------------------------------------- cooldown --

function cooldownTick() {
  const left = S.readyAt - Date.now();
  const circumference = 100.5;
  if (left <= 0) {
    el.cdFill.style.strokeDashoffset = '0';
    el.cdFill.classList.remove('cooling');
    // A room with no cooldown is never "ready" in the sense of having waited,
    // and saying so every frame overwrote the label boot had set correctly.
    el.cooldownText.textContent = S.cooldownMs === 0 ? 'no limit' : 'ready';
  } else {
    const frac = clamp(left / S.cooldownMs, 0, 1);
    el.cdFill.style.strokeDashoffset = String(circumference * frac);
    el.cdFill.classList.add('cooling');
    el.cooldownText.textContent = (left / 1000).toFixed(1) + 's';
  }
  requestAnimationFrame(cooldownTick);
}

const canPlace = () => Date.now() >= S.readyAt;

function setPaused(paused) {
  S.paused = paused;
  el.pausedBadge.hidden = !paused;
  document.body.classList.toggle('is-paused', paused);
}

// ------------------------------------------------------------------- feed --

function pushFeed(x, y, colorIndex) {
  const li = document.createElement('li');
  const sw = document.createElement('span');
  sw.className = 'sw';
  sw.style.background = S.palette[colorIndex] || '#000';
  const txt = document.createElement('span');
  txt.innerHTML = '<b>' + x + '</b>, <b>' + y + '</b>';
  li.appendChild(sw);
  li.appendChild(txt);
  el.feedList.prepend(li);
  if (el.feedEmpty) el.feedEmpty.hidden = true;
  while (el.feedList.children.length > 40) el.feedList.lastChild.remove();
}

// -------------------------------------------------------------- placement --

function lockedAt(x, y) {
  return S.locks.some((l) =>
    x >= Math.min(l.X1, l.X2) && x <= Math.max(l.X1, l.X2) &&
    y >= Math.min(l.Y1, l.Y2) && y <= Math.max(l.Y1, l.Y2));
}

function tryPlace(x, y) {
  if (S.lapse) { toast('exit time-lapse to paint'); return; }
  if (S.paused) { toast('the owner has paused this canvas', 'warn'); return; }
  if (lockedAt(x, y)) { toast('that area is locked', 'warn'); return; }
  if (!canPlace()) {
    toast('cooling down — ' + ((S.readyAt - Date.now()) / 1000).toFixed(1) + 's', 'warn');
    return;
  }
  if (S.pixels[y * S.W + x] === S.color) { toast('already that colour'); return; }

  // Optimistic: paint immediately, let the server's broadcast confirm. A denial
  // rolls it back.
  const previous = S.pixels[y * S.W + x];
  writePixel(x, y, S.color);
  S.readyAt = Date.now() + S.cooldownMs;

  const payload = { t: 'place', x, y, c: S.color };
  if (S.transport === 'ws' && S.socket && S.socket.readyState === WebSocket.OPEN) {
    S.socket.send(JSON.stringify(payload));
    return;
  }
  fetch(API + '/place', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ x, y, c: S.color }),
  }).then(async (res) => {
    if (res.ok) return;
    const body = await res.json().catch(() => ({}));
    writePixel(x, y, previous);
    if (res.status === 429) {
      S.readyAt = Date.now() + (body.retryInMs || S.cooldownMs);
      toast('cooling down', 'warn');
    } else {
      S.readyAt = 0;
      toast(body.error || 'placement rejected', 'bad');
    }
  }).catch(() => {
    writePixel(x, y, previous);
    S.readyAt = 0;
    toast('network error', 'bad');
  });
}

// ---------------------------------------------------------------- cursors --

// Where somebody's pointer is right now is interesting for about a second, so
// cursor updates travel only over the socket, only when the cell actually
// changes, and at most every 60ms. If the socket is not available the feature
// simply does not exist for that client: a position this perishable is not
// worth a POST, and queueing one that missed its moment is worse than dropping
// it.
const CURSOR_MIN_GAP = 60;

function sendCursor(cell) {
  if (S.transport !== 'ws' || !S.socket || S.socket.readyState !== WebSocket.OPEN) return;
  if (!cell) {
    if (!S.cursorCell) return;
    S.cursorCell = null;
    S.socket.send('{"t":"curoff"}');
    return;
  }
  const now = Date.now();
  const same = S.cursorCell && S.cursorCell.x === cell.x && S.cursorCell.y === cell.y &&
    S.cursorCell.c === S.color;
  if (same || now - S.cursorSent < CURSOR_MIN_GAP) return;
  S.cursorSent = now;
  S.cursorCell = { x: cell.x, y: cell.y, c: S.color };
  S.socket.send(JSON.stringify({ t: 'cur', x: cell.x, y: cell.y, c: S.color }));
}

// ---------------------------------------------------------------- template --

function templateError(message) {
  el.tplErr.hidden = !message;
  el.tplErr.textContent = message || '';
}

function openTemplate() {
  el.tplPanel.hidden = false;
  templateError('');
}

function closeTemplate() {
  el.tplPanel.hidden = true;
  S.tplMove = false;
  el.btnTplMove.classList.remove('on');
}

function clearTemplate() {
  S.template = null;
  S.templateSrc = null;
  el.tplLoaded.hidden = true;
  el.tplDrop.hidden = false;
  el.tplFile.value = '';
  S.tplMove = false;
  el.btnTplMove.classList.remove('on');
  templateError('');
  S.dirty = true;
}

// The source file is kept rather than the decoded bitmap so that changing the
// dither mode re-quantises from the original pixels. Re-deriving a template
// from the already-quantised one would compound its errors every time somebody
// tried a different setting.
async function buildTemplate(keepOffset) {
  if (!S.templateSrc) return;
  const previous = S.template;
  try {
    const t = await window.PFTemplate.load(S.templateSrc, {
      palette: S.palette,
      maxW: S.W,
      maxH: S.H,
      dither: { mode: el.tplMode.value, avoidBackground: el.tplAvoidBg.checked },
    });
    // A template that lands centred is immediately visible; one that lands at
    // 0,0 on a big canvas can be off screen entirely, which reads as nothing
    // having happened.
    if (keepOffset && previous) t.setOffset(previous.x, previous.y);
    else t.setOffset(Math.floor((S.W - t.width) / 2), Math.floor((S.H - t.height) / 2));
    S.template = t;
    S.templateOn = true;
    el.btnTplToggle.textContent = 'hide';
    el.tplLoaded.hidden = false;
    el.tplDrop.hidden = true;
    el.tplSize.textContent = t.width + '×' + t.height;
    templateError('');
    refreshTemplate();
  } catch (e) {
    templateError(e && e.message ? e.message : 'that image could not be read');
  }
  S.dirty = true;
}

function loadTemplateFile(file) {
  if (!file) return;
  if (!/^image\//.test(file.type || '')) {
    templateError('that is not an image');
    return;
  }
  S.templateSrc = file;
  buildTemplate(false);
}

let tplTimer = null;
// Progress is recomputed on a timer rather than per placement: it walks the
// whole template, and a busy canvas would otherwise pay for that walk on every
// pixel anybody paints.
function refreshTemplate() {
  if (!S.template || tplTimer) return;
  tplTimer = setTimeout(() => {
    tplTimer = null;
    if (!S.template || !S.pixels) return;
    const p = S.template.progress(S.pixels, S.W);
    S.template._next = p.nextMismatch;
    const pct = p.total === 0 ? 100 : p.percent;
    // Floored, not rounded: "100%" with two cells left to paint is a lie the
    // person tracing it will find out about the hard way.
    el.tplPercent.textContent = Math.floor(pct) + '%';
    el.tplBar.style.width = pct.toFixed(1) + '%';
    // Exact counts, not the abbreviated form used for the headline stats: this
    // is a progress readout somebody is working against, and "645 of 6k" hides
    // the last few hundred cells they still have to paint.
    el.tplCount.textContent = p.done.toLocaleString() + ' of ' +
      p.total.toLocaleString() + ' cells match';
  }, 180);
}

// Sends the painter to the unpainted cell nearest the middle of the template
// and picks the colour it needs, so tracing is click, click, click rather than
// a hunt for the next difference.
function jumpToNextCell() {
  if (!S.template) return;
  const p = S.template.progress(S.pixels, S.W);
  if (!p.nextMismatch) { toast('the template is complete', 'good'); return; }
  const { x, y, want } = p.nextMismatch;
  selectColor(want);
  const r = el.stage.getBoundingClientRect();
  S.view.ox = r.width / 2 - (x + 0.5) * S.view.scale;
  S.view.oy = r.height / 2 - (y + 0.5) * S.view.scale;
  S.hover = { x, y };
  el.coords.textContent = x + ', ' + y;
  S.dirty = true;
}

// --------------------------------------------------------------- inspector --

function closeInspect() {
  el.inspectPanel.hidden = true;
  S.inspect = null;
  S.dirty = true;
}

function ago(ms) {
  const s = Math.max(0, (Date.now() - ms) / 1000);
  if (s < 60) return Math.floor(s) + 's ago';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

async function inspectCell(x, y) {
  S.inspect = { x, y };
  S.dirty = true;
  el.inspectPanel.hidden = false;
  el.inspectTitle.textContent = 'cell ' + x + ', ' + y;
  el.inspectBody.textContent = 'looking…';

  let data;
  try {
    const res = await fetch(API + '/pixel?x=' + x + '&y=' + y);
    data = await res.json();
    if (!res.ok) throw new Error(data.error || 'could not read that cell');
  } catch (e) {
    el.inspectBody.textContent = e.message;
    return;
  }
  if (!S.inspect || S.inspect.x !== x || S.inspect.y !== y) return;  // moved on

  el.inspectBody.textContent = '';
  const history = data.history || [];
  if (!history.length) {
    const p = document.createElement('p');
    p.className = 'muted';
    p.textContent = 'Nobody has painted here. This is the canvas as it started.';
    el.inspectBody.appendChild(p);
    return;
  }

  // Built as nodes rather than markup. Painter ids come from the server and are
  // hex, but the moment history includes anything a person chose, an innerHTML
  // here becomes the bug that lets them choose markup.
  const ul = document.createElement('ul');
  ul.className = 'history';
  history.forEach((h, i) => {
    const li = document.createElement('li');
    if (h.undone) li.className = 'undone';

    const sw = document.createElement('span');
    sw.className = 'sw';
    sw.style.background = S.palette[h.c] || '#000';
    li.appendChild(sw);

    const who = document.createElement('b');
    const mine = data.you && h.uid === data.you;
    who.textContent = mine ? 'you' : h.uid.slice(0, 6);
    if (mine) who.className = 'mine';
    else who.style.color = cursorColour(h.uid);
    li.appendChild(who);

    const when = document.createElement('span');
    when.className = 'when';
    when.textContent = (i === 0 && !h.undone ? '' : h.undone ? 'undone · ' : 'was · ') + ago(h.t);
    li.appendChild(when);

    ul.appendChild(li);
  });
  el.inspectBody.appendChild(ul);

  if (history.length >= 12) {
    const p = document.createElement('p');
    p.className = 'muted';
    p.textContent = 'Showing the twelve most recent.';
    el.inspectBody.appendChild(p);
  }
}

// -------------------------------------------------------------------- undo --

// Undo asks the server rather than reversing the pixel locally, because only
// the placement log knows what was underneath. The server refuses when somebody
// else has painted over it since, and that refusal is the point: taking back
// your pixel should never quietly erase theirs.
async function undoMine() {
  if (S.lapse) { toast('exit time-lapse first'); return; }
  try {
    const res = await fetch(API + '/undo', { method: 'POST' });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      toast(body.error || 'could not undo', res.status === 409 ? 'warn' : 'bad');
      return;
    }
    writePixel(body.x, body.y, body.c);
    S.readyAt = 0;   // the server clears the cooldown too; do not make them wait for the round trip
    toast('took back ' + body.x + ', ' + body.y);
    refreshTemplate();
  } catch (e) {
    toast('network error', 'bad');
  }
}

// -------------------------------------------------------------- transport --

function setConn(state, label) {
  el.connDot.className = 'conn-dot ' + state;
  el.connLabel.textContent = label;
  S.connected = state === 'live';
}

function connect() {
  disconnect();
  if (S.transport === 'ws') connectWS(); else connectSSE();
}

function disconnect() {
  clearTimeout(S.reconnectTimer);
  if (S.socket) { try { S.socket.close(); } catch (e) {} S.socket = null; }
  if (S.events) { try { S.events.close(); } catch (e) {} S.events = null; }
}

function scheduleReconnect() {
  clearTimeout(S.reconnectTimer);
  setConn('wait', 'reconnecting');
  S.reconnectTimer = setTimeout(() => {
    connect();
    // Back off up to 10s so a server restart does not turn into a stampede.
    S.reconnectDelay = Math.min(S.reconnectDelay * 1.7, 10000);
  }, S.reconnectDelay);
}

function connectWS() {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  let sock;
  try {
    sock = new WebSocket(proto + '//' + location.host + API + '/ws');
  } catch (e) {
    fallbackToSSE('websocket unavailable');
    return;
  }
  sock.binaryType = 'arraybuffer';
  S.socket = sock;
  setConn('wait', 'connecting');

  const openTimer = setTimeout(() => {
    if (sock.readyState !== WebSocket.OPEN) {
      try { sock.close(); } catch (e) {}
      fallbackToSSE('websocket timed out');
    }
  }, 8000);

  sock.onopen = () => {
    clearTimeout(openTimer);
    S.reconnectDelay = 500;
    setConn('live', 'websocket');
    // A reconnect may have missed placements; refetch rather than guess.
    if (S.seq > 0) loadSnapshot(true);
  };
  sock.onmessage = (ev) => {
    if (typeof ev.data === 'string') handleJSON(ev.data);
    else handleBinary(new Uint8Array(ev.data));
  };
  sock.onerror = () => {};
  sock.onclose = () => {
    clearTimeout(openTimer);
    if (S.socket !== sock) return;
    S.socket = null;
    if (S.transport === 'ws') scheduleReconnect();
  };
}

function fallbackToSSE(why) {
  if (S.transport !== 'ws') return;
  S.transport = 'sse';
  el.btnTransport.textContent = 'sse';
  toast(why + ' — falling back to SSE', 'warn');
  connect();
}

function connectSSE() {
  const src = new EventSource(API + '/sse');
  S.events = src;
  setConn('wait', 'connecting');
  src.onopen = () => {
    S.reconnectDelay = 500;
    setConn('live', 'sse');
    if (S.seq > 0) loadSnapshot(true);
  };
  src.onmessage = (ev) => handleJSON(ev.data);
  src.onerror = () => {
    // EventSource retries on its own; just reflect the state.
    setConn('wait', 'reconnecting');
  };
}

function handleBinary(buf) {
  if (buf.length < 3 || buf[0] !== 0x01) return;
  const count = (buf[1] << 8) | buf[2];
  let o = 3;
  for (let i = 0; i < count && o + 4 < buf.length + 1; i++) {
    const x = (buf[o] << 8) | buf[o + 1];
    const y = (buf[o + 2] << 8) | buf[o + 3];
    const c = buf[o + 4];
    o += 5;
    if (!S.lapse) writePixel(x, y, c);
    if (i === 0) pushFeed(x, y, c);
    S.placements++;
  }
  el.statPlacements.textContent = fmt(S.placements);
  refreshTemplate();
}

function handleJSON(raw) {
  let msg;
  try { msg = JSON.parse(raw); } catch (e) { return; }
  switch (msg.t) {
    case 'hello':
      S.seq = msg.seq || 0;
      if (msg.uid) S.uid = msg.uid;
      if (typeof msg.clients === 'number') el.statClients.textContent = fmt(msg.clients);
      break;
    case 'presence':
      el.statClients.textContent = fmt(msg.n);
      break;
    case 'cursors':
      // The room broadcasts every cursor, ours included. Drawing our own would
      // put a second, laggier pointer a frame behind the real one.
      S.cursors = (msg.c || []).filter((c) => c.u !== S.uid);
      S.dirty = true;
      break;
    case 'px':
      (msg.p || []).forEach((p, i) => {
        if (!S.lapse) writePixel(p.x, p.y, p.c);
        if (i === 0) pushFeed(p.x, p.y, p.c);
        S.placements++;
        S.seq = Math.max(S.seq, p.s || 0);
      });
      el.statPlacements.textContent = fmt(S.placements);
      break;
    case 'denied':
      S.readyAt = Date.now() + (msg.retryInMs || 0);
      toast(msg.reason || 'placement rejected', 'warn');
      loadSnapshot(true);
      break;
    case 'room':
      if (typeof msg.paused === 'boolean') setPaused(msg.paused);
      if (msg.name) {
        S.room.name = msg.name;
        el.roomName.textContent = msg.name;
        document.title = msg.name + ' — Pixelforge';
      }
      break;
    case 'locks':
      S.locks = msg.locks || [];
      S.dirty = true;
      break;
    case 'cleared':
      toast('the owner cleared the canvas', 'warn');
      loadSnapshot(true);
      break;
    case 'rebuilt':
      toast(`${msg.undone} placement${msg.undone === 1 ? '' : 's'} were undone`, 'warn');
      loadSnapshot(true);
      break;
  }
  updatePaintedStat();
  refreshTemplate();
}

let paintedTimer = null;
function updatePaintedStat() {
  if (paintedTimer) return;
  paintedTimer = setTimeout(() => {
    paintedTimer = null;
    let n = 0;
    for (let i = 0; i < S.pixels.length; i++) if (S.pixels[i] !== 0) n++;
    const pct = ((n / S.pixels.length) * 100);
    el.statPainted.textContent = pct >= 10 ? Math.round(pct) + '%' : pct.toFixed(1) + '%';
  }, 600);
}

// ------------------------------------------------------------------- load --

async function loadSnapshot(silent) {
  const res = await fetch(API + '/snapshot', { cache: 'no-store' });
  if (!res.ok) throw new Error('snapshot request failed: ' + res.status);
  const buf = new Uint8Array(await res.arrayBuffer());
  if (buf.length < 16 || buf[0] !== 80 || buf[1] !== 88 || buf[2] !== 70 || buf[3] !== 49) {
    throw new Error('snapshot has an unexpected format');
  }
  const w = (buf[4] << 8) | buf[5];
  const h = (buf[6] << 8) | buf[7];
  let seq = 0;
  for (let i = 8; i < 16; i++) seq = seq * 256 + buf[i];
  const body = buf.subarray(16);
  if (body.length !== w * h) throw new Error('snapshot payload is the wrong size');

  if (!S.pixels || w !== S.W || h !== S.H) {
    S.W = w; S.H = h;
    off.width = w; off.height = h;
    image = offCtx.createImageData(w, h);
    S.pixels = new Uint8Array(w * h);
  }
  S.pixels.set(body);
  S.seq = seq;
  repaintAll();
  updatePaintedStat();
  if (!silent) fitToScreen();
}

async function loadStats() {
  try {
    const res = await fetch(API + '/stats');
    if (!res.ok) return;
    const j = await res.json();
    S.placements = j.placements || 0;
    el.statPlacements.textContent = fmt(S.placements);
    el.statClients.textContent = fmt(j.clients);
    return j;
  } catch (e) { /* stats are cosmetic */ }
}

// ------------------------------------------------------------- time-lapse --

async function startTimelapse() {
  toast('loading history…');
  let entries = [];
  try {
    const res = await fetch(API + '/history?after=0&limit=30000');
    if (!res.ok) throw new Error();
    entries = (await res.json()).entries || [];
  } catch (e) {
    toast('could not load history', 'bad');
    return;
  }
  if (!entries.length) {
    toast('no history recorded yet' + (el.ephemeralBadge.hidden ? '' : ' (no database attached)'), 'warn');
    return;
  }
  S.lapse = { entries, index: entries.length, playing: false, live: S.pixels.slice() };
  el.timelapse.hidden = false;
  el.scrub.max = String(entries.length);
  el.scrub.value = String(entries.length);
  renderLapse(entries.length);
  toast(entries.length.toLocaleString() + ' placements loaded — drag to scrub');
}

function renderLapse(upto) {
  if (!S.lapse) return;
  S.pixels.fill(0);
  for (let i = 0; i < upto; i++) {
    const e = S.lapse.entries[i];
    S.pixels[e.y * S.W + e.x] = e.c;
  }
  repaintAll();
  S.lapse.index = upto;
  el.timelapseLabel.textContent = upto.toLocaleString() + ' / ' + S.lapse.entries.length.toLocaleString();
  el.scrub.value = String(upto);
}

function exitTimelapse() {
  if (!S.lapse) return;
  S.pixels.set(S.lapse.live);
  S.lapse = null;
  el.timelapse.hidden = true;
  el.btnPlay.textContent = '▶';
  repaintAll();
  loadSnapshot(true).catch(() => {});
}

function lapseStep() {
  if (!S.lapse || !S.lapse.playing) return;
  const total = S.lapse.entries.length;
  // Aim for a replay of roughly 12 seconds regardless of history length.
  const step = Math.max(1, Math.round(total / (12 * 60)));
  let next = S.lapse.index + step;
  if (next >= total) {
    next = total;
    S.lapse.playing = false;
    el.btnPlay.textContent = '▶';
  }
  renderLapse(next);
  if (S.lapse.playing) requestAnimationFrame(lapseStep);
}

// ---------------------------------------------------------------- sheets --

function openSheet(html) {
  el.sheetBody.innerHTML = html;
  el.sheet.hidden = false;
  el.sheetBody.querySelectorAll('[data-copy]').forEach((b) => {
    b.addEventListener('click', () => {
      const input = document.getElementById(b.dataset.copy);
      input.select();
      navigator.clipboard.writeText(input.value)
        .then(() => toast('copied'))
        .catch(() => toast('press Ctrl/⌘+C to copy'));
    });
  });
}
function closeSheet() { el.sheet.hidden = true; }

function helpSheet() {
  openSheet(`
    <h2>Pixelforge</h2>
    <p>A single canvas shared by everyone who has it open. Place a pixel and it
    appears on every other screen within about a tenth of a second.</p>
    <h3>Controls</h3>
    <table>
      <tr><td>Place a pixel</td><td>click</td></tr>
      <tr><td>Pan</td><td>drag, or arrow keys</td></tr>
      <tr><td>Zoom</td><td>scroll, or <kbd>+</kbd> <kbd>−</kbd></td></tr>
      <tr><td>Fit to screen</td><td><kbd>F</kbd></td></tr>
      <tr><td>Pick a colour</td><td><kbd>1</kbd>…<kbd>9</kbd> <kbd>0</kbd></td></tr>
      <tr><td>Eyedropper</td><td>hold <kbd>Alt</kbd> and click</td></tr>
      <tr><td>Who painted this?</td><td>hold <kbd>Shift</kbd> and click</td></tr>
      <tr><td>Take back your last pixel</td><td><kbd>Ctrl</kbd>/<kbd>⌘</kbd> <kbd>Z</kbd></td></tr>
      <tr><td>Template overlay</td><td><kbd>T</kbd>, or drop an image</td></tr>
      <tr><td>Next cell to paint</td><td><kbd>N</kbd></td></tr>
      <tr><td>Hide the template</td><td><kbd>H</kbd></td></tr>
      <tr><td>Close a panel</td><td><kbd>Esc</kbd></td></tr>
    </table>
    <h3>Tracing an image</h3>
    <p>Drop a picture onto the canvas and it is resized to fit, quantised to
    this room's palette and drawn underneath as a ghost. <kbd>N</kbd> sends you
    to the cell nearest the middle that does not match yet and picks the colour
    it needs, so tracing is click, click, click.</p>
    <p>The image is decoded in your browser and never uploaded. Nobody else can
    see what you are copying, and the server never learns you used one.</p>
    <h3>Undo</h3>
    <p>Undo takes back your most recent pixel and restores whatever was
    underneath, read from the placement log rather than guessed. It refuses if
    somebody has painted over it since &mdash; taking back your pixel should
    never quietly erase theirs.</p>
    <h3>How it works</h3>
    <p>The Go server keeps the authoritative grid in memory, appends every
    placement to PostgreSQL through a hand-written wire-protocol driver, and
    fans updates out over a hand-written WebSocket server. Updates are batched
    on a 50&nbsp;ms tick, so a paint storm costs a handful of frames per second
    rather than hundreds.</p>
    <p>If a WebSocket cannot get through, the client falls back to
    Server-Sent Events automatically. Toggle the transport yourself with the
    button in the top right to see both paths work.</p>
  `);
}

async function statsSheet() {
  openSheet('<h2>Statistics</h2><p>Loading…</p>');
  const j = await loadStats();
  if (!j) { openSheet('<h2>Statistics</h2><p>Statistics are unavailable.</p>'); return; }
  const c = j.canvas || {};
  const counts = Object.entries(c.colorCounts || {})
    .filter(([hex]) => hex !== S.palette[0])
    .sort((a, b) => b[1] - a[1]).slice(0, 8);
  const maxCount = counts.length ? counts[0][1] : 1;

  const leaders = (j.leaderboard || []).map((r, i) =>
    `<tr><td>${i + 1}. ${r.uid === S.cfg.uid ? '<b style="color:var(--accent)">you</b>' : r.uid}</td><td>${r.count.toLocaleString()}</td></tr>`
  ).join('') || '<tr><td>no placements recorded</td><td>–</td></tr>';

  openSheet(`
    <h2>Statistics</h2>
    <h3>Canvas</h3>
    <table>
      <tr><td>Grid</td><td>${c.width} × ${c.height}</td></tr>
      <tr><td>Painted</td><td>${(c.painted || 0).toLocaleString()} / ${(c.total || 0).toLocaleString()}</td></tr>
      <tr><td>Placements all time</td><td>${(j.placements || 0).toLocaleString()}</td></tr>
      <tr><td>Connected now</td><td>${j.clients}</td></tr>
      <tr><td>Server uptime</td><td>${j.uptime}</td></tr>
    </table>
    <h3>Most used colours</h3>
    ${counts.map(([hex, n]) => `
      <div style="display:flex;align-items:center;gap:9px;margin:9px 0">
        <span style="width:13px;height:13px;border-radius:3px;background:${hex};box-shadow:inset 0 0 0 1px rgba(255,255,255,.15);flex:none"></span>
        <div style="flex:1">
          <div class="bar"><i style="width:${(n / maxCount) * 100}%;background:${hex}"></i></div>
        </div>
        <span style="font-family:var(--mono);font-size:11px;color:var(--text-dim);min-width:52px;text-align:right">${n.toLocaleString()}</span>
      </div>`).join('') || '<p>Nothing painted yet.</p>'}
    <h3>Top painters</h3>
    <table>${leaders}</table>
  `);
}


// ----------------------------------------------------------------- share ---

function shareSheet() {
  const url = S.cfg.shareUrl || location.origin + '/r/' + SLUG;
  const embed = `<iframe src="${S.cfg.embedUrl || location.origin + '/embed/' + SLUG}" width="480" height="520" style="border:0;border-radius:12px" title="${escapeAttr(S.room.name)}"></iframe>`;
  openSheet(`
    <h2>Share this canvas</h2>
    <p>Anyone with the link can paint. Nothing to install and no account needed.</p>
    <div class="copyrow"><input readonly id="shUrl" value="${escapeAttr(url)}"><button class="ghost" data-copy="shUrl">copy</button></div>

    <h3>Preview</h3>
    <p>This is what the link turns into when it is pasted into Slack, Discord or
    a timeline — rendered from the canvas as it looks right now.</p>
    <img class="card-preview" src="/r/${encodeURIComponent(SLUG)}/card.png?t=${Date.now()}" alt="Link preview for this canvas">

    <h3>Embed it</h3>
    <p>A read-only view that keeps syncing, for a wiki page or a stream overlay.</p>
    <div class="copyrow"><input readonly id="shEmbed" value="${escapeAttr(embed)}"><button class="ghost" data-copy="shEmbed">copy</button></div>

    <h3>Take it with you</h3>
    <div class="sheet-actions">
      <a class="ghost linkish" href="/r/${encodeURIComponent(SLUG)}/canvas.png?scale=8" download>PNG</a>
      <a class="ghost linkish" href="/r/${encodeURIComponent(SLUG)}/timelapse.gif" download>time-lapse GIF</a>
    </div>
  `);
}

function escapeAttr(v) {
  return String(v == null ? '' : v).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// ----------------------------------------------------------- moderation ---

async function mod(path, body) {
  const res = await fetch(API + '/mod/' + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  const out = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(out.error || 'that did not work');
  return out;
}

async function manageSheet() {
  openSheet('<h2>Manage</h2><p>Loading…</p>');
  let stats = {};
  try { stats = await (await fetch(API + '/stats')).json(); } catch (e) { /* best effort */ }
  const painters = (stats.leaderboard || []).slice(0, 12);

  openSheet(`
    <h2>Manage this canvas</h2>

    <h3>Right now</h3>
    <div class="sheet-actions">
      <button class="ghost" id="mPause">${S.paused ? 'Resume painting' : 'Pause painting'}</button>
      <button class="ghost" id="mLock">Lock a region</button>
      <button class="ghost" id="mUnlock" ${S.locks.length ? '' : 'disabled'}>Clear ${S.locks.length} lock${S.locks.length === 1 ? '' : 's'}</button>
    </div>
    <p class="hint">Pausing freezes the canvas for everyone but leaves it visible.
    A locked region stays exactly as it is; the rest of the canvas keeps going.</p>

    <h3>Settings</h3>
    <label class="field"><span class="field-label">Name</span>
      <input type="text" id="mName" maxlength="60" value="${escapeAttr(S.room.name)}"></label>
    <label class="check"><input type="checkbox" id="mUnlisted" ${S.room.visibility === 'unlisted' ? 'checked' : ''}>
      <span>Unlisted — reachable by link, kept off the browse page</span></label>
    <div class="sheet-actions"><button class="ghost" id="mSave">Save settings</button></div>

    <h3>Painters</h3>
    ${painters.length ? `<table class="painters">${painters.map((pn) => `
      <tr>
        <td>${escapeAttr(pn.uid)}${pn.uid === S.cfg.uid ? ' <em>(you)</em>' : ''}</td>
        <td>${pn.count.toLocaleString()}</td>
        <td class="painter-actions">
          <button class="ghost sm" data-undo="${escapeAttr(pn.uid)}">undo all</button>
          <button class="ghost sm" data-ban="${escapeAttr(pn.uid)}">block</button>
        </td>
      </tr>`).join('')}</table>
      <p class="hint">Undoing a painter removes every pixel they placed and rebuilds
      the canvas from the log, so whatever they painted over comes back.</p>`
      : '<p>Nobody has painted here yet.</p>'}

    <h3 class="danger-head">Clear everything</h3>
    <p>Wipes the grid to background. The history is kept, so a time-lapse still
    shows what was there.</p>
    <div class="sheet-actions"><button class="danger" id="mClear">Clear the canvas</button></div>
  `);

  $('mPause').addEventListener('click', async () => {
    try {
      const out = await mod('pause', { paused: !S.paused });
      setPaused(out.paused);
      toast(out.paused ? 'canvas paused' : 'painting resumed');
      closeSheet();
    } catch (e) { toast(e.message, 'bad'); }
  });

  $('mLock').addEventListener('click', () => {
    S.lockMode = true;
    closeSheet();
    toast('drag on the canvas to lock a rectangle');
  });

  $('mUnlock').addEventListener('click', async () => {
    try {
      await mod('locks', { locks: [] });
      S.locks = [];
      S.dirty = true;
      toast('locks cleared');
      closeSheet();
    } catch (e) { toast(e.message, 'bad'); }
  });

  $('mSave').addEventListener('click', async () => {
    try {
      const out = await mod('settings', {
        name: $('mName').value,
        visibility: $('mUnlisted').checked ? 'unlisted' : 'public',
      });
      S.room = out.room || S.room;
      el.roomName.textContent = S.room.name;
      toast('saved');
      closeSheet();
    } catch (e) { toast(e.message, 'bad'); }
  });

  $('mClear').addEventListener('click', async () => {
    if (!confirm('Clear every pixel on this canvas?')) return;
    try {
      await mod('clear');
      await loadSnapshot(true);
      toast('canvas cleared');
      closeSheet();
    } catch (e) { toast(e.message, 'bad'); }
  });

  el.sheetBody.querySelectorAll('[data-ban]').forEach((b) => b.addEventListener('click', async () => {
    try { await mod('ban', { uid: b.dataset.ban }); toast('blocked ' + b.dataset.ban); }
    catch (e) { toast(e.message, 'bad'); }
  }));
  el.sheetBody.querySelectorAll('[data-undo]').forEach((b) => b.addEventListener('click', async () => {
    if (!confirm('Undo everything ' + b.dataset.undo + ' painted?')) return;
    try {
      const out = await mod('undo', { uid: b.dataset.undo });
      await loadSnapshot(true);
      toast(`undid ${out.undone} placement${out.undone === 1 ? '' : 's'}`);
      closeSheet();
    } catch (e) { toast(e.message, 'bad'); }
  }));
}

async function commitLock(a, b) {
  const lock = {
    X1: Math.min(a.x, b.x), Y1: Math.min(a.y, b.y),
    X2: Math.max(a.x, b.x), Y2: Math.max(a.y, b.y),
  };
  try {
    const out = await mod('locks', { locks: S.locks.concat([lock]) });
    S.locks = out.locks || [];
    S.dirty = true;
    toast('region locked');
  } catch (e) { toast(e.message, 'bad'); }
}

// ----------------------------------------------------------------- input --

function bindInput() {
  let dragging = false, moved = false, lastX = 0, lastY = 0, downX = 0, downY = 0;
  const pointers = new Map();
  let pinchDist = 0;
  let tplGrab = null;

  el.stage.addEventListener('pointerdown', (e) => {
    // Only the canvas is a painting surface. The zoom box, the panels, the
    // minimap and the time-lapse bar are all children of the stage, and
    // capturing the pointer for one of their buttons retargets the pointerup to
    // the stage: the click event is then dispatched at the common ancestor, so
    // the button never hears about it, and the gesture is treated as a
    // placement on the cell underneath instead.
    if (e.target !== el.board && e.target !== el.stage) return;
    el.stage.setPointerCapture(e.pointerId);
    pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      pinchDist = Math.hypot(a.x - b.x, a.y - b.y);
      return;
    }
    dragging = true; moved = false;
    lastX = downX = e.clientX; lastY = downY = e.clientY;
    if (S.lockMode) { S.lockStart = screenToCell(e.clientX, e.clientY); }
    if (S.tplMove && S.template) {
      // Grab the template by the point under the pointer so it moves with the
      // hand rather than jumping its corner to the cursor.
      const cell = screenToCell(e.clientX, e.clientY);
      tplGrab = cell ? { dx: S.template.x - cell.x, dy: S.template.y - cell.y } : { dx: 0, dy: 0 };
    }
  });

  el.stage.addEventListener('pointermove', (e) => {
    if (pointers.has(e.pointerId)) pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });

    if (pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      const d = Math.hypot(a.x - b.x, a.y - b.y);
      if (pinchDist > 0) {
        const r = el.stage.getBoundingClientRect();
        zoomAt(d / pinchDist, (a.x + b.x) / 2 - r.left, (a.y + b.y) / 2 - r.top);
      }
      pinchDist = d;
      moved = true;
      return;
    }

    const cell = screenToCell(e.clientX, e.clientY);
    const changed = (!!cell !== !!S.hover) ||
      (cell && S.hover && (cell.x !== S.hover.x || cell.y !== S.hover.y));
    S.hover = cell;
    if (changed) {
      el.coords.textContent = cell ? cell.x + ', ' + cell.y : '–, –';
      S.dirty = true;
    }
    sendCursor(cell);

    if (S.tplMove && S.template && tplGrab && dragging) {
      if (cell) {
        S.template.setOffset(cell.x + tplGrab.dx, cell.y + tplGrab.dy);
        refreshTemplate();
        S.dirty = true;
      }
      moved = true;
      lastX = e.clientX; lastY = e.clientY;
      return;
    }

    if (S.lockMode && S.lockStart && dragging) {
      S.lockEnd = cell;
      S.dirty = true;
      lastX = e.clientX; lastY = e.clientY;
      return;
    }
    if (!dragging) return;
    const dx = e.clientX - lastX, dy = e.clientY - lastY;
    if (Math.abs(e.clientX - downX) > 3 || Math.abs(e.clientY - downY) > 3) {
      if (!moved) el.stage.classList.add('panning');
      moved = true;
    }
    if (moved) {
      S.view.ox += dx; S.view.oy += dy;
      S.dirty = true;
    }
    lastX = e.clientX; lastY = e.clientY;
  });

  const endPointer = (e) => {
    pointers.delete(e.pointerId);
    if (pointers.size < 2) pinchDist = 0;
    if (!dragging) return;
    dragging = false;
    el.stage.classList.remove('panning');

    if (S.tplMove && tplGrab) {
      tplGrab = null;
      return;
    }

    if (S.lockMode) {
      const end = screenToCell(e.clientX, e.clientY) || S.lockEnd;
      if (S.lockStart && end) commitLock(S.lockStart, end);
      S.lockMode = false; S.lockStart = null; S.lockEnd = null;
      S.dirty = true;
      return;
    }
    if (moved) return;
    const cell = screenToCell(e.clientX, e.clientY);
    if (!cell) return;
    if (e.altKey) { selectColor(S.pixels[cell.y * S.W + cell.x]); toast('picked ' + S.palette[S.color]); return; }
    // Shift turns a click into a question instead of a placement. Asking who
    // painted something must never risk painting over it.
    if (e.shiftKey) { inspectCell(cell.x, cell.y); return; }
    tryPlace(cell.x, cell.y);
  };
  el.stage.addEventListener('pointerup', endPointer);
  el.stage.addEventListener('pointercancel', (e) => { pointers.delete(e.pointerId); dragging = false; tplGrab = null; el.stage.classList.remove('panning'); });
  el.stage.addEventListener('pointerleave', () => {
    S.hover = null;
    el.coords.textContent = '–, –';
    sendCursor(null);
    S.dirty = true;
  });

  el.stage.addEventListener('wheel', (e) => {
    e.preventDefault();
    const r = el.stage.getBoundingClientRect();
    zoomAt(e.deltaY < 0 ? 1.15 : 1 / 1.15, e.clientX - r.left, e.clientY - r.top);
  }, { passive: false });

  el.stage.addEventListener('contextmenu', (e) => {
    const cell = screenToCell(e.clientX, e.clientY);
    if (!cell) return;
    e.preventDefault();
    selectColor(S.pixels[cell.y * S.W + cell.x]);
  });

  window.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && (e.key === 'z' || e.key === 'Z')) {
      e.preventDefault();
      undoMine();
      return;
    }
    if (e.metaKey || e.ctrlKey) return;
    // Typing into the template panel's controls should type, not paint.
    const tag = (e.target && e.target.tagName) || '';
    if (tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA') return;
    if (e.key === 'Escape') {
      closeSheet();
      if (S.inspect) closeInspect();
      if (S.lapse) exitTimelapse();
      return;
    }
    if (e.key >= '0' && e.key <= '9') { selectColor(e.key === '0' ? 10 : Number(e.key)); return; }
    switch (e.key) {
      case 'f': case 'F': fitToScreen(); break;
      case '+': case '=': zoomAt(1.25); break;
      case '-': case '_': zoomAt(1 / 1.25); break;
      case 'ArrowLeft':  S.view.ox += 40; S.dirty = true; e.preventDefault(); break;
      case 'ArrowRight': S.view.ox -= 40; S.dirty = true; e.preventDefault(); break;
      case 'ArrowUp':    S.view.oy += 40; S.dirty = true; e.preventDefault(); break;
      case 'ArrowDown':  S.view.oy -= 40; S.dirty = true; e.preventDefault(); break;
      case 't': case 'T': el.tplPanel.hidden ? openTemplate() : closeTemplate(); break;
      case 'n': case 'N': jumpToNextCell(); break;
      case 'h': case 'H':
        if (S.template) { el.btnTplToggle.click(); }
        break;
      case '?': helpSheet(); break;
    }
  });

  window.addEventListener('resize', () => { resize(); });

  $('btnZoomIn').addEventListener('click', () => zoomAt(1.3));
  $('btnZoomOut').addEventListener('click', () => zoomAt(1 / 1.3));
  $('btnFit').addEventListener('click', fitToScreen);
  $('btnHelp').addEventListener('click', helpSheet);
  el.btnShare.addEventListener('click', shareSheet);
  el.btnOwner.addEventListener('click', manageSheet);
  $('btnStats').addEventListener('click', statsSheet);
  $('btnSheetClose').addEventListener('click', closeSheet);
  el.sheet.addEventListener('click', (e) => { if (e.target === el.sheet) closeSheet(); });

  el.btnTransport.addEventListener('click', () => {
    S.transport = S.transport === 'ws' ? 'sse' : 'ws';
    el.btnTransport.textContent = S.transport;
    S.reconnectDelay = 500;
    toast('switched to ' + (S.transport === 'ws' ? 'WebSocket' : 'Server-Sent Events'));
    connect();
  });

  // ---- template ----
  $('btnTemplate').addEventListener('click', () => {
    el.tplPanel.hidden ? openTemplate() : closeTemplate();
  });
  $('btnTplClose').addEventListener('click', closeTemplate);
  el.tplDrop.addEventListener('click', () => el.tplFile.click());
  el.tplDrop.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); el.tplFile.click(); }
  });
  el.tplFile.addEventListener('change', () => loadTemplateFile(el.tplFile.files[0]));
  ['dragenter', 'dragover'].forEach((ev) => el.tplDrop.addEventListener(ev, (e) => {
    e.preventDefault();
    el.tplDrop.classList.add('over');
  }));
  ['dragleave', 'drop'].forEach((ev) => el.tplDrop.addEventListener(ev, (e) => {
    e.preventDefault();
    el.tplDrop.classList.remove('over');
  }));
  el.tplDrop.addEventListener('drop', (e) => {
    const f = e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files[0];
    loadTemplateFile(f);
  });
  // Dropping onto the canvas is what people try first, so accept it there too
  // and open the panel for them rather than letting the browser navigate away
  // to the image they just dropped.
  ['dragover', 'drop'].forEach((ev) => el.stage.addEventListener(ev, (e) => e.preventDefault()));
  el.stage.addEventListener('drop', (e) => {
    const f = e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files[0];
    if (!f) return;
    openTemplate();
    loadTemplateFile(f);
  });
  el.tplMode.addEventListener('change', () => buildTemplate(true));
  el.tplAvoidBg.addEventListener('change', () => buildTemplate(true));
  el.btnTplMove.addEventListener('click', () => {
    S.tplMove = !S.tplMove;
    el.btnTplMove.classList.toggle('on', S.tplMove);
    toast(S.tplMove ? 'drag on the canvas to position the template' : 'positioning off');
  });
  $('btnTplNext').addEventListener('click', jumpToNextCell);
  el.btnTplToggle.addEventListener('click', () => {
    S.templateOn = !S.templateOn;
    el.btnTplToggle.textContent = S.templateOn ? 'hide' : 'show';
    S.dirty = true;
  });
  $('btnTplClear').addEventListener('click', clearTemplate);

  // ---- inspector and undo ----
  $('btnInspectClose').addEventListener('click', closeInspect);
  el.btnUndo.addEventListener('click', undoMine);

  // ---- minimap ----
  // The overview is only worth showing when it tells you something, so it also
  // has to be worth clicking: tapping it recentres the view there.
  el.minimap.addEventListener('click', (e) => {
    const box = el.minimap.getBoundingClientRect();
    const size = 128;
    const s = Math.min(size / S.W, size / S.H);
    const px = (size - S.W * s) / 2, py = (size - S.H * s) / 2;
    const mx = ((e.clientX - box.left) / box.width) * size;
    const my = ((e.clientY - box.top) / box.height) * size;
    const cx = clamp((mx - px) / s, 0, S.W);
    const cy = clamp((my - py) / s, 0, S.H);
    const r = el.stage.getBoundingClientRect();
    S.view.ox = r.width / 2 - cx * S.view.scale;
    S.view.oy = r.height / 2 - cy * S.view.scale;
    S.dirty = true;
  });

  $('btnLapse').addEventListener('click', () => { if (S.lapse) exitTimelapse(); else startTimelapse(); });
  $('btnExitLapse').addEventListener('click', exitTimelapse);
  el.scrub.addEventListener('input', () => {
    if (!S.lapse) return;
    S.lapse.playing = false;
    el.btnPlay.textContent = '▶';
    renderLapse(Number(el.scrub.value));
  });
  el.btnPlay.addEventListener('click', () => {
    if (!S.lapse) return;
    S.lapse.playing = !S.lapse.playing;
    el.btnPlay.textContent = S.lapse.playing ? '❚❚' : '▶';
    if (S.lapse.playing) {
      if (S.lapse.index >= S.lapse.entries.length) renderLapse(0);
      requestAnimationFrame(lapseStep);
    }
  });

  // A backgrounded tab stops receiving frames; refetch on return so the canvas
  // is never quietly stale.
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && !S.lapse) loadSnapshot(true).catch(() => {});
  });
}

// ------------------------------------------------------------------- boot --

async function boot() {
  if (!SLUG) {
    el.bootMsg.className = 'error';
    el.bootMsg.textContent = 'This page did not tell the client which canvas to load.';
    return;
  }
  try {
    const res = await fetch(API + '/config');
    if (!res.ok) throw new Error('config request failed: ' + res.status);
    S.cfg = await res.json();
  } catch (e) {
    el.bootMsg.className = 'error';
    el.bootMsg.textContent = 'Could not reach the server. ' + e.message;
    return;
  }

  S.room = S.cfg.room || {};
  S.palette = S.room.palette || [];
  S.rgb = S.palette.map(hexToRGB);
  S.cooldownMs = S.room.cooldownMs ?? 750;
  S.color = S.palette.length > 6 ? 6 : 1;
  S.owner = !!S.cfg.owner;
  S.locks = S.room.locks || [];
  setPaused(!!S.room.paused);

  el.roomName.textContent = S.room.name || 'Canvas';
  document.title = (S.room.name || 'Canvas') + ' — Pixelforge';
  el.roomSub.textContent = `${S.room.width}×${S.room.height} · ${
    S.cooldownMs ? (S.cooldownMs >= 1000 ? (S.cooldownMs / 1000) + 's' : S.cooldownMs + 'ms') + ' cooldown' : 'no cooldown'
  }${S.room.visibility === 'unlisted' ? ' · unlisted' : ''}`;
  el.btnOwner.hidden = !S.owner;
  el.btnGif.href = '/r/' + SLUG + '/timelapse.gif';

  buildPalette();
  buildTemplateModes();
  resize();

  try {
    await loadSnapshot(false);
  } catch (e) {
    el.bootMsg.className = 'error';
    el.bootMsg.textContent = 'Could not load the canvas. ' + e.message;
    return;
  }

  await loadStats();
  if (S.cooldownMs === 0) el.cooldownText.textContent = 'no limit';

  bindInput();
  connect();
  render();
  cooldownTick();

  el.boot.classList.add('gone');
  setTimeout(() => { el.boot.hidden = true; }, 500);
}

boot();

})();
