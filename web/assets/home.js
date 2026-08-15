/* Pixelforge home: browse live canvases and make a new one.
 * No framework, no build step — same rule the server follows. */
'use strict';

(() => {

const $ = (id) => document.getElementById(id);
const el = {
  creator: $('creator'), name: $('fName'), sizes: $('fSizes'), palettes: $('fPalettes'),
  cooldowns: $('fCooldowns'), unlisted: $('fUnlisted'), createError: $('createError'),
  grid: $('roomGrid'), empty: $('roomsEmpty'), toast: $('toast'),
  sheet: $('sheet'), sheetBody: $('sheetBody'), whoami: $('whoami'),
  btnAccount: $('btnAccount'), ephemeral: $('ephemeralBadge'),
  heroNote: $('heroNote'), footStats: $('footStats'), cooldownHint: $('cooldownHint'),
};

const S = {
  palettes: [],
  choice: { width: 128, height: 128, palette: 'classic', cooldownMs: 750 },
  signedIn: false,
};

const SIZES = [
  { label: 'Small',  w: 64,  h: 64,  note: 'fills in minutes' },
  { label: 'Medium', w: 128, h: 128, note: 'a good afternoon' },
  { label: 'Large',  w: 256, h: 256, note: 'a proper project' },
  { label: 'Wide',   w: 384, h: 192, note: 'banner shaped' },
];

const COOLDOWNS = [
  { label: 'None',  ms: 0,    note: 'a free-for-all — best with people you trust' },
  { label: '1s',    ms: 1000, note: 'quick, still stops one person carpeting it' },
  { label: '5s',    ms: 5000, note: 'deliberate. every pixel costs something' },
  { label: '30s',   ms: 30000, note: 'a slow build over days' },
];

let toastTimer = null;
function toast(msg, kind) {
  el.toast.textContent = msg;
  el.toast.className = 'toast show' + (kind ? ' ' + kind : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.toast.className = 'toast'; }, 2600);
}

function fmt(n) {
  if (n === null || n === undefined) return '0';
  if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
}

function ago(iso) {
  const then = Date.parse(iso);
  if (!then) return '';
  const s = Math.max(0, (Date.now() - then) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

// --------------------------------------------------------------- chooser ---

function buildChoosers() {
  el.sizes.innerHTML = '';
  SIZES.forEach((s) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chip' + (s.w === S.choice.width && s.h === S.choice.height ? ' on' : '');
    b.setAttribute('role', 'radio');
    b.innerHTML = '';
    const strong = document.createElement('strong');
    strong.textContent = s.label;
    const dim = document.createElement('span');
    dim.textContent = s.w + '×' + s.h;
    b.append(strong, dim);
    b.title = s.note;
    b.addEventListener('click', () => {
      S.choice.width = s.w; S.choice.height = s.h;
      buildChoosers();
    });
    el.sizes.appendChild(b);
  });

  el.cooldowns.innerHTML = '';
  COOLDOWNS.forEach((c) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chip' + (c.ms === S.choice.cooldownMs ? ' on' : '');
    b.setAttribute('role', 'radio');
    b.textContent = c.label;
    b.addEventListener('click', () => { S.choice.cooldownMs = c.ms; buildChoosers(); });
    el.cooldowns.appendChild(b);
  });
  const chosen = COOLDOWNS.find((c) => c.ms === S.choice.cooldownMs);
  el.cooldownHint.textContent = chosen ? chosen.note : '';

  el.palettes.innerHTML = '';
  S.palettes.forEach((p) => {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'palette-card' + (p.key === S.choice.palette ? ' on' : '');
    b.setAttribute('role', 'radio');
    b.title = p.note;

    const strip = document.createElement('span');
    strip.className = 'palette-strip';
    p.colors.forEach((hex) => {
      const sw = document.createElement('i');
      sw.style.background = hex;
      strip.appendChild(sw);
    });
    const label = document.createElement('span');
    label.className = 'palette-name';
    label.textContent = p.name;

    b.append(strip, label);
    b.addEventListener('click', () => { S.choice.palette = p.key; buildChoosers(); });
    el.palettes.appendChild(b);
  });
}

// ----------------------------------------------------------------- rooms ---

function roomCard(r) {
  const a = document.createElement('a');
  a.className = 'room-card';
  a.href = '/r/' + r.slug;

  const thumb = document.createElement('div');
  thumb.className = 'room-thumb';
  const img = document.createElement('img');
  // The server renders the canvas as a PNG, so a thumbnail costs no client
  // work and is always current.
  img.src = '/r/' + r.slug + '/canvas.png?scale=2';
  img.alt = '';
  img.loading = 'lazy';
  thumb.appendChild(img);

  if (r.clients > 0) {
    const live = document.createElement('span');
    live.className = 'room-live';
    live.textContent = r.clients + ' here';
    thumb.appendChild(live);
  }
  if (r.paused) {
    const p = document.createElement('span');
    p.className = 'room-paused';
    p.textContent = 'paused';
    thumb.appendChild(p);
  }

  const body = document.createElement('div');
  body.className = 'room-body';
  const h = document.createElement('h3');
  h.textContent = r.name;
  const meta = document.createElement('p');
  meta.className = 'room-meta';
  meta.textContent = `${r.width}×${r.height} · ${fmt(r.placements)} pixels · ${ago(r.lastActive)}`;
  body.append(h, meta);

  a.append(thumb, body);
  return a;
}

async function loadRooms() {
  try {
    const res = await fetch('/api/rooms');
    if (!res.ok) throw new Error('rooms request failed');
    const { rooms } = await res.json();
    el.grid.innerHTML = '';
    if (!rooms || !rooms.length) {
      el.empty.hidden = false;
      el.footStats.textContent = '';
      return;
    }
    el.empty.hidden = true;
    rooms.forEach((r) => el.grid.appendChild(roomCard(r)));
    const px = rooms.reduce((n, r) => n + (r.placements || 0), 0);
    el.footStats.textContent = `${rooms.length} canvas${rooms.length === 1 ? '' : 'es'} · ${fmt(px)} pixels placed`;
  } catch (e) {
    el.grid.innerHTML = '';
    el.empty.hidden = false;
    el.empty.textContent = 'Could not load canvases right now.';
  }
}

// ---------------------------------------------------------------- create ---

async function createRoom() {
  el.createError.hidden = true;
  const btn = $('btnCreate');
  btn.disabled = true;
  btn.textContent = 'Creating…';
  try {
    const res = await fetch('/api/rooms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        name: el.name.value,
        width: S.choice.width,
        height: S.choice.height,
        palette: S.choice.palette,
        cooldownMs: S.choice.cooldownMs,
        unlisted: el.unlisted.checked,
      }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || 'could not create the canvas');

    // Show the moderator link before navigating: it is the one thing the
    // creator cannot get back if they lose the cookie, so it should not flash
    // past on a redirect.
    showModeratorKey(body);
  } catch (e) {
    el.createError.textContent = e.message;
    el.createError.hidden = false;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Create and open';
  }
}

function showModeratorKey(body) {
  openSheet(`
    <h2>Your canvas is ready</h2>
    <p>Send this link to anyone you want painting on it.</p>
    <div class="copyrow"><input readonly id="shareUrl" value="${escapeAttr(body.url)}"><button class="ghost" data-copy="shareUrl">copy</button></div>

    <h3>Keep this one to yourself</h3>
    <p>This is the moderator link. It is what lets you pause, clear, lock and
    block. It is stored in this browser already — save it somewhere if you might
    open the canvas from a different device, because it cannot be reissued.</p>
    <div class="copyrow"><input readonly id="modUrl" value="${escapeAttr(body.moderatorUrl)}"><button class="ghost" data-copy="modUrl">copy</button></div>

    <div class="sheet-actions">
      <a class="primary linkish" href="${escapeAttr(body.url)}">Open the canvas</a>
    </div>
  `);
}

function escapeAttr(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// -------------------------------------------------------------- accounts ---

async function refreshAccount() {
  try {
    const me = await (await fetch('/api/auth/me')).json();
    S.signedIn = !!me.signedIn;
    if (S.signedIn) {
      el.whoami.hidden = false;
      el.whoami.textContent = me.username;
      el.btnAccount.textContent = 'sign out';
    } else {
      el.whoami.hidden = true;
      el.btnAccount.textContent = 'sign in';
    }
  } catch (e) { /* accounts are optional; the page works without them */ }
}

function accountSheet(mode) {
  const isRegister = mode === 'register';
  openSheet(`
    <h2>${isRegister ? 'Create an account' : 'Sign in'}</h2>
    <p>Accounts are optional. They exist so your canvases stay together and you
    can moderate them from any device — painting never needs one.</p>
    <label class="field"><span class="field-label">Username</span>
      <input type="text" id="acName" autocomplete="username" maxlength="24"></label>
    <label class="field"><span class="field-label">Password</span>
      <input type="password" id="acPass" autocomplete="${isRegister ? 'new-password' : 'current-password'}"></label>
    <p class="error" id="acError" hidden></p>
    <div class="sheet-actions">
      <button class="primary" id="acGo">${isRegister ? 'Create account' : 'Sign in'}</button>
      <button class="ghost" id="acSwap">${isRegister ? 'I already have one' : 'Create one instead'}</button>
    </div>
  `);

  $('acSwap').addEventListener('click', () => accountSheet(isRegister ? 'login' : 'register'));
  $('acGo').addEventListener('click', async () => {
    const err = $('acError');
    err.hidden = true;
    try {
      const res = await fetch('/api/auth/' + (isRegister ? 'register' : 'login'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username: $('acName').value, password: $('acPass').value }),
      });
      const body = await res.json();
      if (!res.ok) throw new Error(body.error || 'that did not work');
      closeSheet();
      await refreshAccount();
      toast('signed in as ' + body.username);
    } catch (e) {
      err.textContent = e.message;
      err.hidden = false;
    }
  });
  const pass = $('acPass');
  pass.addEventListener('keydown', (e) => { if (e.key === 'Enter') $('acGo').click(); });
}

// ---------------------------------------------------------------- sheets ---

function openSheet(html) {
  el.sheetBody.innerHTML = html;
  el.sheet.hidden = false;
  el.sheetBody.querySelectorAll('[data-copy]').forEach((b) => {
    b.addEventListener('click', () => {
      const input = document.getElementById(b.dataset.copy);
      input.select();
      navigator.clipboard.writeText(input.value)
        .then(() => toast('copied'))
        .catch(() => toast('press ⌘/Ctrl+C to copy'));
    });
  });
}
function closeSheet() { el.sheet.hidden = true; }

// ------------------------------------------------------------------ boot ---

async function boot() {
  try {
    const cfg = await (await fetch('/api/palettes')).json();
    S.palettes = cfg.palettes || [];
  } catch (e) { /* the creator falls back to whatever the server accepts */ }

  try {
    const health = await (await fetch('/healthz')).json();
    if (health.ephemeral) {
      el.ephemeral.hidden = false;
      $('btnStart').disabled = true;
      el.heroNote.textContent = 'This instance is running without a database, so canvases cannot be created right now.';
    } else if (health.rooms > 0) {
      el.heroNote.textContent = `${health.rooms} canvas${health.rooms === 1 ? '' : 'es'} warm right now, ${health.clients} ${health.clients === 1 ? 'person' : 'people'} painting.`;
    }
  } catch (e) { /* cosmetic */ }

  buildChoosers();
  await Promise.all([loadRooms(), refreshAccount()]);

  $('btnStart').addEventListener('click', () => {
    el.creator.hidden = false;
    el.creator.scrollIntoView({ behavior: 'smooth', block: 'center' });
    el.name.focus();
  });
  $('btnCancel').addEventListener('click', () => { el.creator.hidden = true; });
  $('btnCreate').addEventListener('click', createRoom);
  el.name.addEventListener('keydown', (e) => { if (e.key === 'Enter') createRoom(); });
  $('btnRefresh').addEventListener('click', loadRooms);
  $('btnSheetClose').addEventListener('click', closeSheet);
  el.sheet.addEventListener('click', (e) => { if (e.target === el.sheet) closeSheet(); });
  window.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeSheet(); });

  el.btnAccount.addEventListener('click', async () => {
    if (S.signedIn) {
      await fetch('/api/auth/logout', { method: 'POST' });
      await refreshAccount();
      toast('signed out');
      return;
    }
    accountSheet('login');
  });

  // Keep the browse list roughly current without hammering the server.
  setInterval(loadRooms, 30000);
}

boot();

})();
