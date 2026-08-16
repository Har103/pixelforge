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
  booted: false,
  // The keyboard's own place on the grid. It is not the pointer: the pointer is
  // wherever the mouse happens to be and forgets itself the moment it leaves,
  // while this is a position somebody is standing on and will still be standing
  // on after they have gone to the palette and come back.
  kb: { cell: null, shown: false, oriented: false },
  // Anything this tab placed, kept just long enough to recognise it coming back
  // on the broadcast. See notePlacement.
  mine: [],
  peers: { seen: 0, near: 0, lastNear: 0, clients: 0, saidClients: -1 },
  awaitReady: false,    // somebody is waiting out a cooldown and asked to be told
  sheetReturn: null,
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
  btnUndo: $('btnUndo'), btnTemplate: $('btnTemplate'),
  srSelf: $('srSelf'), srPeers: $('srPeers'), sheetInner: $('sheetInner'),
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

// ------------------------------------------------------------ announcing --

// What a screen reader is told, and — the harder half — what it is not.
//
// This canvas produces hundreds of events a minute and almost none of them are
// worth a sentence. Two rules keep the volume survivable. Anything the person
// is in the middle of doing is announced on a trailing debounce, so holding an
// arrow key across forty cells says where they arrived rather than naming all
// forty. Anything anybody else did is summarised on a slow tick instead, and
// the tick stays silent when there is nothing to summarise.

const SAY_SETTLE_MS = 130;

let selfTimer = null;
let sayFlip = false;

// Assistive technology ignores a live region set to the text it already holds,
// and "cooling down" twice running is a thing somebody genuinely needs to hear
// twice. A zero-width space, alternated, makes every write textually new
// without adding a syllable to what is spoken.
function writeRegion(region, text) {
  sayFlip = !sayFlip;
  region.textContent = text + (sayFlip ? '\u200B' : '');
}

// saySelf answers something the person just did. A later message always
// replaces a pending earlier one, because the later one is by definition about
// where they now are.
function saySelf(text, settle) {
  clearTimeout(selfTimer);
  selfTimer = null;
  if (!settle) { writeRegion(el.srSelf, text); return; }
  selfTimer = setTimeout(() => { selfTimer = null; writeRegion(el.srSelf, text); }, settle);
}

function sayPeers(text) { writeRegion(el.srPeers, text); }

// Colour names are derived from the hex rather than looked up in a table,
// because the palette belongs to the room and a room can be created with any of
// them — a table here would go stale the first time somebody adds a palette to
// the server, and a hex code read out as eight characters is not a colour.
function hsl(hex) {
  const rgb = hexToRGB(hex).map((v) => v / 255);
  const [r, g, b] = rgb;
  const max = Math.max(r, g, b), min = Math.min(r, g, b);
  const l = (max + min) / 2;
  const d = max - min;
  if (d === 0) return { h: 0, s: 0, l };
  const s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
  let h;
  if (max === r) h = (g - b) / d + (g < b ? 6 : 0);
  else if (max === g) h = (b - r) / d + 2;
  else h = (r - g) / d + 4;
  return { h: h * 60, s, l };
}

const HUE_NAMES = [
  [16, 'red'], [45, 'orange'], [66, 'yellow'], [95, 'lime'], [150, 'green'],
  [178, 'teal'], [200, 'cyan'], [242, 'blue'], [265, 'indigo'], [290, 'violet'],
  [320, 'purple'], [340, 'pink'], [361, 'red'],
];

function colourName(index) {
  if (index === 0) return 'background';
  const hex = S.palette[index];
  if (!hex) return 'colour ' + index;
  const c = hsl(hex);
  if (c.s < 0.15) {
    if (c.l < 0.12) return 'black';
    if (c.l < 0.35) return 'dark grey';
    if (c.l < 0.66) return 'grey';
    if (c.l < 0.9) return 'light grey';
    return 'white';
  }
  let name = 'colour ' + index;
  for (let i = 0; i < HUE_NAMES.length; i++) {
    if (c.h < HUE_NAMES[i][0]) { name = HUE_NAMES[i][1]; break; }
  }
  // Dark orange is brown. Every palette here has one and nobody calls it
  // orange, so the arithmetic has to know that too.
  if ((name === 'orange' || name === 'yellow') && c.l < 0.45) return 'brown';
  if (c.l < 0.34) return 'dark ' + name;
  if (c.l > 0.75) return 'pale ' + name;
  // Barely saturated is a grey with an opinion, and calling #6b7899 "blue"
  // sends somebody looking for something bluer than anything on the canvas.
  if (c.s < 0.3) return name + ' grey';
  return name;
}

// ------------------------------------------------------------- describing --

// Joining a list the way a person would say it, because "north, east" and
// "north and east" are the difference between a readout and a sentence.
function spoken(words) {
  if (words.length <= 1) return words[0] || '';
  return words.slice(0, -1).join(', ') + ' and ' + words[words.length - 1];
}

const colourAt = (x, y) => S.pixels[y * S.W + x];

function cellText(x, y) {
  return x + ', ' + y + ', ' + colourName(colourAt(x, y)) + (lockedAt(x, y) ? ', locked' : '');
}

const AROUND = [
  [0, -1, 'north'], [1, -1, 'north-east'], [1, 0, 'east'], [1, 1, 'south-east'],
  [0, 1, 'south'], [-1, 1, 'south-west'], [-1, 0, 'west'], [-1, -1, 'north-west'],
];

// One cell at a time is how you read a canvas through a keyhole. This is the
// smallest readout that is actually a picture: eight neighbours, grouped by
// colour so that "red to the east and south-east" arrives as a shape rather
// than as eight separate facts.
function describeAround(x, y) {
  const groups = new Map();
  const edges = [];
  AROUND.forEach((d) => {
    const nx = x + d[0], ny = y + d[1];
    if (nx < 0 || ny < 0 || nx >= S.W || ny >= S.H) { edges.push(d[2]); return; }
    const name = colourName(colourAt(nx, ny));
    if (!groups.has(name)) groups.set(name, []);
    groups.get(name).push(d[2]);
  });

  const head = 'Around ' + x + ', ' + y + ', which is ' + colourName(colourAt(x, y)) + ': ';
  if (groups.size === 1 && !edges.length) {
    return head + 'all eight neighbours are ' + [...groups.keys()][0] + '.';
  }

  // Biggest group last and folded into "the rest", because on a canvas that is
  // mostly empty the small groups are the entire content of the answer.
  const ordered = [...groups.entries()].sort((a, b) => b[1].length - a[1].length);
  const rest = ordered.length > 1 && ordered[0][1].length >= 4 ? ordered.shift() : null;
  const parts = ordered.map((g) => g[0] + ' to the ' + spoken(g[1]));
  if (edges.length) parts.push('the canvas edge to the ' + spoken(edges));
  if (rest) parts.push('the rest ' + rest[0]);
  return head + parts.join('; ') + '.';
}

// A whole row, run-length encoded, and only the painted runs: on a canvas that
// is four percent full, listing the gaps is listing the canvas.
function describeRow(y) {
  const runs = [];
  let start = 0;
  for (let x = 1; x <= S.W; x++) {
    const prev = colourAt(x - 1, y);
    if (x === S.W || colourAt(x, y) !== prev) {
      if (prev !== 0) runs.push({ from: start, to: x - 1, c: prev });
      start = x;
    }
  }
  if (!runs.length) return 'Row ' + y + ' of ' + S.H + ' is empty.';

  let painted = 0;
  runs.forEach((r) => { painted += r.to - r.from + 1; });
  const shown = runs.slice(0, 10).map((r) => (r.from === r.to
    ? 'column ' + r.from
    : 'columns ' + r.from + ' to ' + r.to) + ' ' + colourName(r.c));
  const more = runs.length - shown.length;
  return 'Row ' + y + ': ' + painted + ' of ' + S.W + ' cells painted. ' +
    spoken(shown) + (more ? ', and ' + more + ' more run' + (more === 1 ? '' : 's') : '') + '.';
}

const SECTORS = ['top left', 'top', 'top right', 'left', 'middle', 'right',
  'bottom left', 'bottom', 'bottom right'];

// Where the art is. Without this a keyboard user has no way to find out that
// everything happening on a 96×64 grid is happening in one corner of it, short
// of walking the whole canvas a row at a time.
function describeRegions() {
  const counts = new Array(9).fill(0);
  let painted = 0;
  for (let y = 0; y < S.H; y++) {
    const band = Math.min(2, Math.floor((y * 3) / S.H));
    for (let x = 0; x < S.W; x++) {
      if (colourAt(x, y) === 0) continue;
      painted++;
      counts[band * 3 + Math.min(2, Math.floor((x * 3) / S.W))]++;
    }
  }
  const size = S.W + ' by ' + S.H;
  if (!painted) return 'Canvas ' + size + ', nothing painted yet.';

  const pct = (painted / (S.W * S.H)) * 100;
  const busiest = counts
    .map((n, i) => ({ n, name: SECTORS[i] }))
    .filter((s) => s.n > 0)
    .sort((a, b) => b.n - a.n)
    .slice(0, 3)
    .map((s) => s.name + ', ' + s.n);
  let where = '';
  if (S.kb.cell) {
    const band = Math.min(2, Math.floor((S.kb.cell.y * 3) / S.H));
    const col = Math.min(2, Math.floor((S.kb.cell.x * 3) / S.W));
    where = ' Your cursor is in the ' + SECTORS[band * 3 + col] + '.';
  }
  return 'Canvas ' + size + ', ' + (pct < 1 ? 'under 1' : Math.round(pct)) +
    ' percent painted. Busiest: ' + spoken(busiest) + '.' + where;
}

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

  // The keyboard cursor: a white box inside a black one, because a caret drawn
  // in one colour vanishes the moment somebody paints the cell it is standing
  // on, and on a twenty colour palette that is a matter of when rather than if.
  if (S.kb.shown && S.kb.cell && !S.lapse) {
    const kx = ox + S.kb.cell.x * scale, ky = oy + S.kb.cell.y * scale;
    ctx.save();
    ctx.strokeStyle = '#000000';
    ctx.lineWidth = 4;
    ctx.strokeRect(kx - 2, ky - 2, scale + 4, scale + 4);
    ctx.strokeStyle = '#ffffff';
    ctx.lineWidth = 2;
    ctx.strokeRect(kx - 2, ky - 2, scale + 4, scale + 4);
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

// -------------------------------------------------------------- the caret --

// Arrow keys used to pan the view by forty pixels. They move this cursor now,
// and the view follows it. The trade is one-sided: panning already had the
// drag, the minimap, fit-to-screen and the zoom box, while painting had no
// keyboard route at all — the product's entire claim was unavailable to
// anybody who does not use a mouse. Moving the cursor pans as a side effect, so
// nothing was actually lost; panning without moving the cursor was never worth
// a keystroke of its own.

function cellInView() {
  const r = el.stage.getBoundingClientRect();
  return {
    x: clamp(Math.floor((r.width / 2 - S.view.ox) / S.view.scale), 0, S.W - 1),
    y: clamp(Math.floor((r.height / 2 - S.view.oy) / S.view.scale), 0, S.H - 1),
  };
}

// A sighted keyboard user has to be able to see where the cursor went, so the
// view chases it — minimally when it is stepping, and by recentring when it has
// jumped somewhere it could not have walked to.
function keepCursorInView(centre) {
  const c = S.kb.cell;
  if (!c) return;
  const r = el.stage.getBoundingClientRect();
  const s = S.view.scale;
  if (centre) {
    S.view.ox = r.width / 2 - (c.x + 0.5) * s;
    S.view.oy = r.height / 2 - (c.y + 0.5) * s;
    S.dirty = true;
    return;
  }
  // Two cells of clearance, or forty pixels. At one pixel per cell "just on
  // screen" and "off screen" look identical. The margin is capped at half the
  // stage so that a viewport smaller than the margin cannot ask for a window
  // with nothing in it and leave the view oscillating.
  const padX = Math.min(Math.max(40, s * 2), Math.max(0, (r.width - s) / 2));
  const padY = Math.min(Math.max(40, s * 2), Math.max(0, (r.height - s) / 2));
  const left = S.view.ox + c.x * s, right = left + s;
  const top = S.view.oy + c.y * s, bottom = top + s;
  if (left < padX) S.view.ox += padX - left;
  else if (right > r.width - padX) S.view.ox -= right - (r.width - padX);
  if (top < padY) S.view.oy += padY - top;
  else if (bottom > r.height - padY) S.view.oy -= bottom - (r.height - padY);
  S.dirty = true;
}

function announceCursor(settle) {
  if (!S.kb.cell) return;
  saySelf(cellText(S.kb.cell.x, S.kb.cell.y), settle);
}

function setCursor(x, y, opts) {
  const o = opts || {};
  S.kb.cell = { x: clamp(x, 0, S.W - 1), y: clamp(y, 0, S.H - 1) };
  S.kb.shown = true;
  el.coords.textContent = S.kb.cell.x + ', ' + S.kb.cell.y;
  keepCursorInView(o.centre);
  // Somebody moving by keyboard is present in the room exactly as much as
  // somebody moving a mouse, and everyone else should see them arrive.
  sendCursor(S.kb.cell);
  S.dirty = true;
  if (o.quiet) return;
  announceCursor(o.settle === undefined ? SAY_SETTLE_MS : o.settle);
}

function moveCursor(dx, dy) {
  if (!S.pixels) return;
  if (!S.kb.cell) {
    // The first press puts the cursor where the person is already looking
    // rather than at a corner they would then have to walk back from.
    const start = cellInView();
    setCursor(start.x, start.y);
    return;
  }
  const nx = clamp(S.kb.cell.x + dx, 0, S.W - 1);
  const ny = clamp(S.kb.cell.y + dy, 0, S.H - 1);
  if (nx === S.kb.cell.x && ny === S.kb.cell.y) {
    // Silence here would be indistinguishable from a key that did not register.
    const edge = dx < 0 ? 'left' : dx > 0 ? 'right' : dy < 0 ? 'top' : 'bottom';
    saySelf('The ' + edge + ' edge. ' + cellText(nx, ny));
    return;
  }
  setCursor(nx, ny);
}

function jumpCursor(x, y) {
  if (!S.pixels) return;
  setCursor(x, y, { centre: true, settle: 0 });
}

function screenToCell(clientX, clientY) {
  const r = el.stage.getBoundingClientRect();
  const x = Math.floor((clientX - r.left - S.view.ox) / S.view.scale);
  const y = Math.floor((clientY - r.top - S.view.oy) / S.view.scale);
  if (x < 0 || y < 0 || x >= S.W || y >= S.H) return null;
  return { x, y };
}

// ----------------------------------------------------------------- palette --

// The swatch's spoken name. A hex code read out as eight characters is not a
// colour, and the digit that selects it is worth more than either.
function swatchLabel(i, hex) {
  const key = i <= 10 ? ', key ' + (i % 10) : '';
  return colourName(i) + ', ' + hex + key;
}

function buildPalette() {
  el.palette.innerHTML = '';
  S.palette.forEach((hex, i) => {
    const b = document.createElement('button');
    b.className = 'swatch' + (i === S.color ? ' sel' : '');
    b.style.background = hex;
    b.type = 'button';
    b.setAttribute('role', 'radio');
    b.setAttribute('aria-checked', String(i === S.color));
    b.setAttribute('aria-label', swatchLabel(i, hex));
    // A roving tabindex, because twenty swatches were twenty tab stops between
    // the canvas and the undo button. A radio group is one stop and the arrows
    // move within it; anything else makes a keyboard user walk the rainbow.
    b.tabIndex = i === S.color ? 0 : -1;
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

function selectColor(i, quiet) {
  if (i < 0 || i >= S.palette.length) return;
  S.color = i;
  [...el.palette.children].forEach((c, idx) => {
    const on = idx === i;
    c.classList.toggle('sel', on);
    c.setAttribute('aria-checked', String(on));
    c.tabIndex = on ? 0 : -1;
  });
  // Which colour is in your hand is the one piece of state you cannot see by
  // looking at the cell under the cursor, so it is said out loud when it
  // changes rather than only when something is painted with it.
  if (!quiet) saySelf(colourName(i) + ' selected.');
  // Everybody else's view of what this painter is holding travels with the
  // cursor, so a colour picked from the keyboard has to refresh it.
  const at = S.kb.cell || S.hover;
  if (at) sendCursor(at);
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
    // Said once, and only to somebody who was actually waiting. Announcing
    // every cooldown would be an interruption every 750ms on the default room.
    if (S.awaitReady) { S.awaitReady = false; saySelf('Ready to paint.'); }
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
  const changed = S.booted && S.paused !== paused;
  S.paused = paused;
  el.pausedBadge.hidden = !paused;
  document.body.classList.toggle('is-paused', paused);
  // A canvas that stops accepting pixels while you are painting on it is the
  // one moderator action a painter has to be told about; the badge that says so
  // is a badge.
  if (changed) {
    sayPeers(paused ? 'The owner paused this canvas. Painting is off.' : 'Painting has resumed.');
  }
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

// ------------------------------------------------------------- other people --

// Presence, summarised, because there is no version of announcing every remote
// placement that is not hostile — a busy canvas produces several a second and
// none of them is about you.
//
// One thing gets through immediately: a pixel landing next to yours. That is
// the event you cannot see coming and the one that makes a shared canvas feel
// shared, so it interrupts once and then stays quiet for ten seconds however
// much lands after it. Everything else is a count on a slow tick, and the tick
// says nothing at all when there is nothing to count.
const NEAR_CELLS = 4;
const NEAR_QUIET_MS = 10000;
const DIGEST_MS = 12000;

// A placement this tab made, remembered only long enough to recognise it coming
// back. The broadcast carries no painter id, so the alternative is a digest
// that counts your own work as somebody else's.
function rememberMine(x, y, c) {
  const now = Date.now();
  S.mine = S.mine.filter((m) => now - m.t < 5000);
  S.mine.push({ x, y, c, t: now });
}

function notePlacement(x, y, c) {
  for (let i = 0; i < S.mine.length; i++) {
    const m = S.mine[i];
    if (m.x === x && m.y === y && m.c === c) { S.mine.splice(i, 1); return; }
  }
  S.peers.seen++;
  if (!S.kb.cell) return;
  const away = Math.max(Math.abs(x - S.kb.cell.x), Math.abs(y - S.kb.cell.y));
  if (away > NEAR_CELLS) return;
  S.peers.near++;
  const now = Date.now();
  if (now - S.peers.lastNear < NEAR_QUIET_MS) return;
  S.peers.lastNear = now;
  sayPeers('Somebody painted ' + colourName(c) + ' at ' + x + ', ' + y + ', ' +
    (away === 1 ? 'right next to your cursor' : away + ' cells from your cursor') + '.');
}

function digestPeers() {
  // A backgrounded tab is not a tab anybody is listening to, and a queue of
  // digests read out on return is worse than having missed them.
  if (document.hidden) { S.peers.seen = 0; S.peers.near = 0; return; }
  const parts = [];
  if (S.peers.seen > 0) {
    parts.push(S.peers.seen + (S.peers.seen === 1 ? ' pixel' : ' pixels') +
      ' painted in the last ' + Math.round(DIGEST_MS / 1000) + ' seconds');
    if (S.peers.near > 0) parts.push(S.peers.near + ' of them beside you');
  }
  if (S.peers.clients !== S.peers.saidClients) {
    S.peers.saidClients = S.peers.clients;
    parts.push(S.peers.clients + (S.peers.clients === 1 ? ' person here' : ' people here'));
  }
  S.peers.seen = 0;
  S.peers.near = 0;
  if (parts.length) sayPeers(parts.join(', ') + '.');
}

function notePresence(n) {
  if (typeof n !== 'number') return;
  el.statClients.textContent = fmt(n);
  // The first count is this tab arriving, which nobody needs telling about.
  if (S.peers.saidClients < 0) S.peers.saidClients = n;
  S.peers.clients = n;
}

// -------------------------------------------------------------- placement --

function lockedAt(x, y) {
  return S.locks.some((l) =>
    x >= Math.min(l.X1, l.X2) && x <= Math.max(l.X1, l.X2) &&
    y >= Math.min(l.Y1, l.Y2) && y <= Math.max(l.Y1, l.Y2));
}

function tryPlace(x, y) {
  // Every refusal below reaches a screen reader through the toast, which is
  // already a status region; saying it a second time here would double every
  // one of them.
  if (S.lapse) { toast('exit time-lapse to paint'); return; }
  if (S.paused) { toast('the owner has paused this canvas', 'warn'); return; }
  if (lockedAt(x, y)) { toast('that area is locked', 'warn'); return; }
  if (!canPlace()) {
    toast('cooling down — ' + ((S.readyAt - Date.now()) / 1000).toFixed(1) + 's', 'warn');
    // Somebody who has just been refused is by definition waiting, so this is
    // the one case where the end of the wait is worth announcing.
    S.awaitReady = true;
    return;
  }
  if (S.pixels[y * S.W + x] === S.color) { toast('already that colour'); return; }

  // Optimistic: paint immediately, let the server's broadcast confirm. A denial
  // rolls it back.
  const previous = S.pixels[y * S.W + x];
  writePixel(x, y, S.color);
  S.readyAt = Date.now() + S.cooldownMs;
  rememberMine(x, y, S.color);
  // Success is silent on screen — the pixel simply appears — so it is the one
  // outcome that has nothing to announce it unless this does.
  saySelf('Painted ' + colourName(S.color) + ' at ' + x + ', ' + y + '.');
  // A cooldown short enough to be over before the sentence finishes is not
  // worth a second sentence about.
  if (S.cooldownMs >= 2000) S.awaitReady = true;

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

// ---------------------------------------------------------------- focus ---

// Focus is a place. Anything that takes it has to be able to put it back, and
// anything that removes the element holding it has to notice that it did — a
// panel that closes while its own close button is focused drops the keyboard
// user at the top of the document with no idea why.

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), ' +
  'select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

function focusables(root) {
  return [...root.querySelectorAll(FOCUSABLE)]
    .filter((n) => !n.hidden && n.getClientRects().length > 0);
}

// getClientRects rather than offsetParent: everything on this page is inside a
// position:fixed bar, and offsetParent is null for all of them.
function focusable(node) {
  return !!node && document.contains(node) && !node.hidden && node.getClientRects().length > 0;
}

// The canvas is the fallback because it is the page: dropping somebody back
// there is always more useful than dropping them on <body>.
function focusBack(node) {
  (focusable(node) ? node : el.board).focus();
}

function templateError(message) {
  el.tplErr.hidden = !message;
  el.tplErr.textContent = message || '';
}

function openTemplate() {
  if (!el.tplPanel.hidden) return;
  S.tplReturn = document.activeElement;
  el.tplPanel.hidden = false;
  templateError('');
  // The panel is a group of controls somebody deliberately opened in order to
  // use, so focus goes into it rather than leaving them to hunt for it.
  el.tplPanel.focus();
}

function closeTemplate() {
  if (el.tplPanel.hidden) return;
  const inside = el.tplPanel.contains(document.activeElement);
  el.tplPanel.hidden = true;
  S.tplMove = false;
  el.btnTplMove.classList.remove('on');
  if (inside) focusBack(S.tplReturn);
  S.tplReturn = null;
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
  selectColor(want, true);
  S.hover = { x, y };
  // The cursor goes too, so that "next cell" and Enter are the whole tracing
  // loop for somebody who never touches the mouse. Whether it is *drawn* is
  // left as it was: a pointer user pressing this button has not asked for a
  // caret and would only get a second highlight on the cell they are looking at.
  const wasShown = S.kb.shown;
  setCursor(x, y, { centre: true, quiet: true });
  S.kb.shown = wasShown;
  saySelf('Next cell to paint: ' + x + ', ' + y + '. ' + colourName(want) + ' selected.');
}

// --------------------------------------------------------------- inspector --

function closeInspect() {
  if (el.inspectPanel.hidden) return;
  const inside = el.inspectPanel.contains(document.activeElement);
  el.inspectPanel.hidden = true;
  S.inspect = null;
  S.dirty = true;
  if (inside) focusBack(S.inspectReturn);
  S.inspectReturn = null;
}

function ago(ms) {
  const s = Math.max(0, (Date.now() - ms) / 1000);
  if (s < 60) return Math.floor(s) + 's ago';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

// takeFocus is set only when the question was asked from the keyboard. A
// shift-click has to leave the pointer user's focus where it was — yanking it
// into a panel would mean their next arrow key no longer moved the cursor.
async function inspectCell(x, y, takeFocus) {
  const wasOpen = !el.inspectPanel.hidden;
  S.inspect = { x, y };
  S.dirty = true;
  el.inspectPanel.hidden = false;
  el.inspectTitle.textContent = 'cell ' + x + ', ' + y;
  el.inspectBody.textContent = 'looking…';
  if (!wasOpen) S.inspectReturn = document.activeElement;
  if (takeFocus) el.inspectPanel.focus();

  let data;
  try {
    const res = await fetch(API + '/pixel?x=' + x + '&y=' + y);
    data = await res.json();
    if (!res.ok) throw new Error(data.error || 'could not read that cell');
  } catch (e) {
    el.inspectBody.textContent = e.message;
    saySelf(e.message);
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
    saySelf('Cell ' + x + ', ' + y + '. Nobody has painted here.');
    return;
  }

  // The panel is a list somebody has to go and read; the answer to the question
  // they asked is the first line of it, so that is what gets said.
  const top = history[0];
  const who = data.you && top.uid === data.you ? 'you' : 'painter ' + top.uid.slice(0, 6);
  saySelf('Cell ' + x + ', ' + y + '. ' + colourName(top.c) + ', painted by ' + who + ' ' +
    ago(top.t) + '. ' + history.length + ' entr' + (history.length === 1 ? 'y' : 'ies') +
    ' in this cell’s history.');

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
  // 'gone' means the canvas was deleted underneath us. Reconnecting would spend
  // the next ten seconds retrying something that will never answer again.
  if (S.transport === 'gone') return;
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
    notePlacement(x, y, c);
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
      notePresence(msg.clients);
      break;
    case 'presence':
      notePresence(msg.n);
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
        notePlacement(p.x, p.y, p.c);
        S.placements++;
        S.seq = Math.max(S.seq, p.s || 0);
      });
      el.statPlacements.textContent = fmt(S.placements);
      break;
    case 'undone':
      // Deliberately not counted as a placement: the server does not record it
      // as one either, and a client that counted it would drift one ahead of
      // every stat page and every reload.
      if (!S.lapse) writePixel(msg.x, msg.y, msg.c);
      pushFeed(msg.x, msg.y, msg.c);
      // If the panel happens to be open on that very cell, the history it is
      // showing is now wrong. Ask again rather than leave a retracted pixel on
      // screen looking current.
      if (S.inspect && S.inspect.x === msg.x && S.inspect.y === msg.y) {
        inspectCell(msg.x, msg.y);
      }
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
    case 'deleted':
      // The canvas this page is showing no longer exists. Stop reconnecting -
      // otherwise the client spends the next ten seconds retrying a 404 - say
      // what happened, and let them leave.
      S.transport = 'gone';
      disconnect();
      setConn('down', 'gone');
      saySelf(msg.reason || 'This canvas was deleted.');
      openSheet(`<h2>This canvas is gone</h2>
        <p>${escapeAttr(msg.reason || 'The owner deleted it.')} Nothing you paint
        here now will be saved.</p>
        <div class="sheet-actions"><a class="primary linkish" href="/">See the other canvases</a></div>`);
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
    notePresence(j.clients);
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
    // There is no ephemeral badge on this page - that lives on the home page -
    // so reading one threw a TypeError here and the toast never appeared:
    // pressing "time-lapse" on a canvas nobody had painted on did nothing at
    // all, silently, which is the worst of the available outcomes.
    toast('no history recorded yet', 'warn');
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

// While a modal sheet is open the rest of the page stops existing: not
// focusable, not clickable, not in the accessibility tree. Without this the
// dialog is a visual effect — a screen reader reads straight past it into the
// page behind and the user answers a question they cannot see. The live regions
// and the toast are kept alive because the sheet's own buttons talk through
// them.
const KEEP_LIVE = ['sheet', 'toast', 'srSelf', 'srPeers'];
function setBackgroundInert(on) {
  [...document.body.children].forEach((node) => {
    if (KEEP_LIVE.indexOf(node.id) >= 0) return;
    node.inert = on;
  });
}

function openSheet(html) {
  const opening = el.sheet.hidden;
  if (opening) S.sheetReturn = document.activeElement;
  el.sheetBody.innerHTML = html;
  // The dialog is named by whatever heading the sheet just built, so the first
  // thing announced is which dialog this is.
  const heading = el.sheetBody.querySelector('h2');
  if (heading) heading.id = 'sheetTitle';
  el.sheet.hidden = false;
  if (opening) setBackgroundInert(true);
  // Focused on every call, not only the first: statsSheet and manageSheet
  // replace their own contents once the fetch lands, and a dialog whose entire
  // body has changed has to say so.
  el.sheetInner.focus();
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

function closeSheet() {
  if (el.sheet.hidden) return;
  el.sheet.hidden = true;
  setBackgroundInert(false);
  const back = S.sheetReturn;
  S.sheetReturn = null;
  focusBack(back);
}

function helpSheet() {
  openSheet(`
    <h2>Pixelforge</h2>
    <p>A single canvas shared by everyone who has it open. Place a pixel and it
    appears on every other screen within about a tenth of a second.</p>
    <h3>Painting with a mouse</h3>
    <table>
      <tr><td>Place a pixel</td><td>click</td></tr>
      <tr><td>Pan</td><td>drag</td></tr>
      <tr><td>Zoom</td><td>scroll, or <kbd>+</kbd> <kbd>−</kbd></td></tr>
      <tr><td>Eyedropper</td><td>hold <kbd>Alt</kbd> and click</td></tr>
      <tr><td>Who painted this?</td><td>hold <kbd>Shift</kbd> and click</td></tr>
    </table>

    <h3>Painting without one</h3>
    <p>Tab to the canvas, or press the skip link at the very top. It has its own
    cursor: the arrows move it, the view follows it, and the whole canvas is
    reachable without touching a pointing device.</p>
    <table>
      <tr><td>Move the cursor</td><td><kbd>↑</kbd> <kbd>↓</kbd> <kbd>←</kbd> <kbd>→</kbd></td></tr>
      <tr><td>Move ten cells</td><td>hold <kbd>Shift</kbd></td></tr>
      <tr><td>Move ten rows</td><td><kbd>PgUp</kbd> <kbd>PgDn</kbd></td></tr>
      <tr><td>Ends of the row</td><td><kbd>Home</kbd> <kbd>End</kbd></td></tr>
      <tr><td>Corners of the canvas</td><td><kbd>Ctrl</kbd> <kbd>Home</kbd> / <kbd>End</kbd></td></tr>
      <tr><td>Paint the cursor cell</td><td><kbd>Enter</kbd> or <kbd>Space</kbd></td></tr>
      <tr><td>Pick up the colour under it</td><td><kbd>E</kbd></td></tr>
      <tr><td>Who painted this?</td><td><kbd>I</kbd></td></tr>
    </table>

    <h3>Reading the canvas</h3>
    <p>Three descriptions, spoken into a live region, for anybody who is not
    looking at the grid.</p>
    <table>
      <tr><td>The eight cells around the cursor</td><td><kbd>R</kbd></td></tr>
      <tr><td>The whole row, painted runs only</td><td><kbd>Shift</kbd> <kbd>R</kbd></td></tr>
      <tr><td>Where the painted areas are</td><td><kbd>M</kbd></td></tr>
    </table>

    <h3>Everything else</h3>
    <table>
      <tr><td>Pick a colour</td><td><kbd>1</kbd>…<kbd>9</kbd> <kbd>0</kbd></td></tr>
      <tr><td>Step through the palette</td><td><kbd>[</kbd> <kbd>]</kbd></td></tr>
      <tr><td>Fit to screen</td><td><kbd>F</kbd></td></tr>
      <tr><td>Take back your last pixel</td><td><kbd>Ctrl</kbd>/<kbd>⌘</kbd> <kbd>Z</kbd></td></tr>
      <tr><td>Template overlay</td><td><kbd>T</kbd>, or drop an image</td></tr>
      <tr><td>Next cell to paint</td><td><kbd>N</kbd></td></tr>
      <tr><td>Hide the template</td><td><kbd>H</kbd></td></tr>
      <tr><td>Move the template</td><td><kbd>P</kbd>, then the arrows</td></tr>
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
        <span aria-hidden="true" style="width:13px;height:13px;border-radius:3px;background:${hex};box-shadow:inset 0 0 0 1px rgba(255,255,255,.15);flex:none"></span>
        <div style="flex:1">
          <!-- The swatch and the bar are the colour and the number, drawn. The
               name is the only part of this row that survives being read aloud,
               and without it every row here is an unlabelled count. -->
          <span class="sr-only">${escapeAttr(colourName(S.palette.indexOf(hex)))}</span>
          <div class="bar" aria-hidden="true"><i style="width:${(n / maxCount) * 100}%;background:${hex}"></i></div>
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
    <div class="copyrow"><input readonly id="shUrl" aria-label="Link to this canvas" value="${escapeAttr(url)}"><button class="ghost" data-copy="shUrl">copy<span class="sr-only"> the link</span></button></div>

    <h3>Preview</h3>
    <p>This is what the link turns into when it is pasted into Slack, Discord or
    a timeline — rendered from the canvas as it looks right now.</p>
    <img class="card-preview" src="/r/${encodeURIComponent(SLUG)}/card.png?t=${Date.now()}" alt="Link preview for this canvas">

    <h3>Embed it</h3>
    <p>A read-only view that keeps syncing, for a wiki page or a stream overlay.</p>
    <div class="copyrow"><input readonly id="shEmbed" aria-label="Embed code for this canvas" value="${escapeAttr(embed)}"><button class="ghost" data-copy="shEmbed">copy<span class="sr-only"> the embed code</span></button></div>

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

    <h3 class="danger-head">Delete this canvas</h3>
    <p>Removes the canvas, every pixel, its whole history and this link, for good.
    Anybody looking at it right now will be told it is gone. There is no undo and
    no time-lapse afterwards.</p>
    <label class="field">
      <span class="field-label" id="mDelLabel">Type <code>${escapeAttr(SLUG)}</code> to confirm</span>
      <input type="text" id="mDelConfirm" autocomplete="off" spellcheck="false"
             aria-labelledby="mDelLabel" aria-describedby="mDelHint" placeholder="${escapeAttr(SLUG)}">
    </label>
    <p class="hint" id="mDelHint">Typing the name is deliberate. A button on its own
    is one mis-click away from something that cannot be taken back.</p>
    <div class="sheet-actions"><button class="danger" id="mDelete" disabled>Delete for ever</button></div>
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

  // The button only becomes usable once the name matches, so the confirmation is
  // a state you have to reach rather than a dialog you dismiss.
  const delInput = $('mDelConfirm');
  const delButton = $('mDelete');
  delInput.addEventListener('input', () => {
    delButton.disabled = delInput.value.trim() !== SLUG;
  });
  delButton.addEventListener('click', async () => {
    if (delInput.value.trim() !== SLUG) return;
    delButton.disabled = true;
    delButton.textContent = 'Deleting…';
    try {
      await mod('delete', { confirm: delInput.value.trim() });
      // Leave rather than sit on a page whose canvas no longer exists.
      location.href = '/?deleted=' + encodeURIComponent(SLUG);
    } catch (e) {
      delButton.disabled = false;
      delButton.textContent = 'Delete for ever';
      toast(e.message, 'bad');
    }
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

function typingInto(node) {
  if (!node) return false;
  const tag = node.tagName || '';
  return tag === 'INPUT' || tag === 'SELECT' || tag === 'TEXTAREA' || !!node.isContentEditable;
}

// The cell cursor answers to the arrows only when the canvas has focus, or when
// nothing on the page does. Anywhere else the arrows belong to whatever is
// focused — stepping through the palette, dragging the time-lapse slider — and
// a canvas that grabbed them there would make every other control unusable.
function canvasHasKeys() {
  const a = document.activeElement;
  return !a || a === el.board || a === document.body;
}

function onKey(e) {
  // Escape first, and ahead of the typing guard: a dialog you cannot leave from
  // inside its own text field is a dialog you are stuck in. One layer per
  // press, outermost first, so a single Escape never closes something the
  // person could not see was open.
  if (e.key === 'Escape') {
    if (!el.sheet.hidden) closeSheet();
    else if (S.inspect) closeInspect();
    else if (!el.tplPanel.hidden) closeTemplate();
    else if (S.lapse) exitTimelapse();
    return;
  }
  if ((e.metaKey || e.ctrlKey) && (e.key === 'z' || e.key === 'Z')) {
    e.preventDefault();
    undoMine();
    return;
  }
  // Typing into a control should type, not paint. Alt combinations belong to
  // the browser and the operating system.
  if (typingInto(e.target) || e.altKey) return;

  if (canvasHasKeys() && S.pixels) {
    const far = e.shiftKey ? 10 : 1;
    // Positioning the template was drag-only, which made a feature of the
    // product unreachable without a pointing device. While positioning is on
    // the arrows move the template instead of the cursor, which is the same
    // gesture without the hand.
    if (S.tplMove && S.template && !e.ctrlKey && !e.metaKey) {
      const step = { ArrowLeft: [-far, 0], ArrowRight: [far, 0], ArrowUp: [0, -far], ArrowDown: [0, far] }[e.key];
      if (step) {
        e.preventDefault();
        S.template.setOffset(S.template.x + step[0], S.template.y + step[1]);
        refreshTemplate();
        S.dirty = true;
        saySelf('Template at ' + S.template.x + ', ' + S.template.y + '.', SAY_SETTLE_MS);
        return;
      }
    }
    if (!e.ctrlKey && !e.metaKey) {
      switch (e.key) {
        case 'ArrowLeft':  e.preventDefault(); moveCursor(-far, 0); return;
        case 'ArrowRight': e.preventDefault(); moveCursor(far, 0); return;
        case 'ArrowUp':    e.preventDefault(); moveCursor(0, -far); return;
        case 'ArrowDown':  e.preventDefault(); moveCursor(0, far); return;
        // Nobody crosses five hundred cells one arrow press at a time. Ten rows
        // a press, the ends of the row, and the corners of the canvas are the
        // three distances that turn a big grid into somewhere you can travel.
        case 'PageUp':     e.preventDefault(); moveCursor(0, -10); return;
        case 'PageDown':   e.preventDefault(); moveCursor(0, 10); return;
      }
    }
    if (e.key === 'Home' || e.key === 'End') {
      e.preventDefault();
      const toEnd = e.key === 'End';
      if (!S.kb.cell) moveCursor(0, 0);
      else if (e.ctrlKey || e.metaKey) jumpCursor(toEnd ? S.W - 1 : 0, toEnd ? S.H - 1 : 0);
      else jumpCursor(toEnd ? S.W - 1 : 0, S.kb.cell.y);
      return;
    }
  }

  // Painting needs the canvas to actually hold focus, not merely for nothing
  // else to. A screen reader in browse mode parks focus on <body> and uses
  // Space to move down the page; painting a pixel because somebody was reading
  // would be a genuinely bad surprise.
  if (document.activeElement === el.board && (e.key === 'Enter' || e.key === ' ') &&
      !e.ctrlKey && !e.metaKey && S.pixels) {
    e.preventDefault();
    if (!S.kb.cell) { moveCursor(0, 0); return; }
    S.kb.shown = true;
    S.dirty = true;
    tryPlace(S.kb.cell.x, S.kb.cell.y);
    return;
  }

  if (canvasHasKeys() && S.pixels && !e.ctrlKey && !e.metaKey) {
    const at = S.kb.cell;
    switch (e.key) {
      case 'r': if (!at) { moveCursor(0, 0); return; } saySelf(describeAround(at.x, at.y)); return;
      case 'R': if (!at) { moveCursor(0, 0); return; } saySelf(describeRow(at.y)); return;
      case 'm': case 'M': saySelf(describeRegions()); return;
      case 'e': case 'E':
        if (!at) { moveCursor(0, 0); return; }
        selectColor(colourAt(at.x, at.y));
        return;
      case 'i': case 'I':
        if (!at) { moveCursor(0, 0); return; }
        inspectCell(at.x, at.y, true);
        return;
    }
  }

  if (e.metaKey || e.ctrlKey) return;
  if (e.key >= '0' && e.key <= '9' && e.key.length === 1) {
    selectColor(e.key === '0' ? 10 : Number(e.key));
    return;
  }
  switch (e.key) {
    case 'f': case 'F': fitToScreen(); break;
    case '+': case '=': zoomAt(1.25); break;
    case '-': case '_': zoomAt(1 / 1.25); break;
    // The digits reach ten colours. A twenty colour palette had the other ten
    // available to a mouse and to nothing else.
    case '[': selectColor((S.color + S.palette.length - 1) % S.palette.length); break;
    case ']': selectColor((S.color + 1) % S.palette.length); break;
    case 't': case 'T': el.tplPanel.hidden ? openTemplate() : closeTemplate(); break;
    case 'n': case 'N': jumpToNextCell(); break;
    case 'h': case 'H':
      if (S.template) { el.btnTplToggle.click(); }
      break;
    // Positioning has to be switchable from the canvas, because that is where
    // focus has to be for the arrows to reach the template at all.
    case 'p': case 'P':
      if (S.template && !el.tplPanel.hidden) { el.btnTplMove.click(); }
      break;
    case '?': helpSheet(); break;
  }
}

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
      // A caret drawn on a canvas somebody is using with a mouse is clutter, and
      // worse, it is a second answer to "where would Enter paint". The pointer
      // takes the cursor with it and stops drawing it; a key press brings it
      // straight back, in the place the pointer left it.
      if (cell) { S.kb.cell = cell; S.kb.shown = false; }
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
    if (e.shiftKey) { inspectCell(cell.x, cell.y, false); return; }
    tryPlace(cell.x, cell.y);
  };
  el.stage.addEventListener('pointerup', endPointer);
  el.stage.addEventListener('pointercancel', (e) => { pointers.delete(e.pointerId); dragging = false; tplGrab = null; el.stage.classList.remove('panning'); });
  el.stage.addEventListener('pointerleave', () => {
    S.hover = null;
    el.coords.textContent = S.kb.shown && S.kb.cell ? S.kb.cell.x + ', ' + S.kb.cell.y : '–, –';
    sendCursor(S.kb.shown && S.kb.cell ? S.kb.cell : null);
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

  window.addEventListener('keydown', onKey);

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

  // Tab wraps inside the open dialog. The `inert` above already achieves this
  // by leaving nothing else focusable in the document, but only in browsers
  // that have it; this is the same guarantee spelled out, and it is the one a
  // test can drive deterministically.
  el.sheet.addEventListener('keydown', (e) => {
    if (e.key !== 'Tab') return;
    const items = focusables(el.sheetInner);
    if (!items.length) { e.preventDefault(); el.sheetInner.focus(); return; }
    const first = items[0], last = items[items.length - 1];
    const active = document.activeElement;
    if (e.shiftKey && (active === first || active === el.sheetInner)) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    }
  });

  // ---- the canvas as a control ----
  el.board.addEventListener('focus', () => {
    if (!S.pixels) return;
    // :focus-visible is precisely the question being asked — did this focus
    // arrive from a keyboard — and the browser has already answered it. A click
    // that focuses the canvas must not flash a caret on the cell it landed on.
    const viaKeyboard = el.board.matches(':focus-visible');
    const at = S.kb.cell || cellInView();
    setCursor(at.x, at.y, { quiet: true });
    S.kb.shown = viaKeyboard;
    if (S.kb.oriented) { announceCursor(0); return; }
    // First arrival gets the shape of the place and how to move in it. Every
    // arrival after that gets the cursor, because by then they know.
    S.kb.oriented = true;
    saySelf(describeRegions() + ' Cursor at ' + cellText(S.kb.cell.x, S.kb.cell.y) +
      '. Arrow keys move it, Enter paints, R describes what is around it.');
  });

  // ---- palette ----
  // A radio group is one tab stop and the arrows move inside it. Without this
  // the twenty swatches were twenty stops, and the ones past the tenth had no
  // keyboard route at all: the digits only reach ten.
  el.palette.addEventListener('keydown', (e) => {
    const n = S.palette.length;
    if (!n) return;
    let next = -1;
    switch (e.key) {
      case 'ArrowRight': case 'ArrowDown': next = (S.color + 1) % n; break;
      case 'ArrowLeft': case 'ArrowUp': next = (S.color + n - 1) % n; break;
      case 'Home': next = 0; break;
      case 'End': next = n - 1; break;
      default: return;
    }
    e.preventDefault();
    selectColor(next);
    el.palette.children[next].focus();
  });

  el.btnTransport.addEventListener('click', () => {
    S.transport = S.transport === 'ws' ? 'sse' : 'ws';
    el.btnTransport.textContent = S.transport;
    el.btnTransport.setAttribute('aria-label', S.transport === 'ws'
      ? 'Realtime transport: WebSocket. Activate to switch to Server-Sent Events.'
      : 'Realtime transport: Server-Sent Events. Activate to switch to WebSocket.');
    S.reconnectDelay = 500;
    toast('switched to ' + (S.transport === 'ws' ? 'WebSocket' : 'Server-Sent Events'));
    connect();
  });

  // ---- template ----
  $('btnTemplate').addEventListener('click', () => {
    el.tplPanel.hidden ? openTemplate() : closeTemplate();
  });
  $('btnTplClose').addEventListener('click', closeTemplate);
  // The drop zone is a <label> around a real file input rather than a div with
  // a click handler, so opening the picker with a pointer, with Enter and with
  // Space are all the browser's job and none of them is reimplemented here.
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
    el.btnTplMove.setAttribute('aria-pressed', String(S.tplMove));
    toast(S.tplMove ? 'drag or use the arrow keys to position the template' : 'positioning off');
    if (!S.tplMove) return;
    // The arrows only reach the template while the canvas holds focus, so send
    // focus there rather than leaving the mode switched on and inert.
    el.board.focus();
    saySelf('Positioning the template. The arrow keys move it; press P again to stop.');
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

// A boot that fails leaves an overlay over the whole page saying why. It is a
// live region so the reason is spoken, and focus goes to it so that somebody
// who arrived on the page by keyboard is not left tabbing around behind a
// message they never heard.
function bootFailed(message) {
  el.bootMsg.className = 'error';
  el.bootMsg.textContent = message;
  el.bootMsg.focus();
}

async function boot() {
  if (!SLUG) {
    bootFailed('This page did not tell the client which canvas to load.');
    return;
  }
  try {
    const res = await fetch(API + '/config');
    if (!res.ok) throw new Error('config request failed: ' + res.status);
    S.cfg = await res.json();
  } catch (e) {
    bootFailed('Could not reach the server. ' + e.message);
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
    bootFailed('Could not load the canvas. ' + e.message);
    return;
  }

  // The canvas is a control now, so it needs a name that says what it is and
  // how big it is. Its keys live in #canvasHelp, which aria-describedby points
  // at, so this stays short enough to hear on every focus.
  el.board.setAttribute('aria-label',
    `Shared pixel canvas, ${S.room.width} by ${S.room.height} cells, ${S.palette.length} colours`);

  await loadStats();
  if (S.cooldownMs === 0) el.cooldownText.textContent = 'no limit';

  bindInput();
  connect();
  render();
  cooldownTick();
  setInterval(digestPeers, DIGEST_MS);
  S.booted = true;

  el.boot.classList.add('gone');
  setTimeout(() => { el.boot.hidden = true; }, 500);
}

boot();

})();
