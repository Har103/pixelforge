/* Pixelforge home: browse live canvases and make a new one.
 * No framework, no build step — same rule the server follows. */
'use strict';

(() => {

const $ = (id) => document.getElementById(id);
const el = {
  creator: $('creator'), name: $('fName'), sizes: $('fSizes'), palettes: $('fPalettes'),
  cooldowns: $('fCooldowns'), unlisted: $('fUnlisted'), createError: $('createError'),
  grid: $('roomGrid'), empty: $('roomsEmpty'), toast: $('toast'),
  sheet: $('sheet'), sheetInner: $('sheetInner'), sheetBody: $('sheetBody'), whoami: $('whoami'),
  btnAccount: $('btnAccount'), ephemeral: $('ephemeralBadge'),
  heroNote: $('heroNote'), footStats: $('footStats'), cooldownHint: $('cooldownHint'),
};

// Somebody who has asked their operating system to stop moving things has asked
// this page too, and a scroll animation started from JavaScript is not covered
// by the stylesheet's prefers-reduced-motion rule.
const stillness = window.matchMedia('(prefers-reduced-motion: reduce)');
const scrollBehaviour = () => (stillness.matches ? 'auto' : 'smooth');

const S = {
  palettes: [],
  // The cooldown has to be one of the values COOLDOWNS actually offers. It was
  // 750, which is not among them, so the group rendered with nothing selected
  // and no hint under it while quietly submitting a number the form never
  // showed anybody.
  choice: { width: 128, height: 128, palette: 'classic', cooldownMs: 1000 },
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

// A radio in a radio group carries its own state and only the chosen one is a
// tab stop. These were buttons wearing role="radio" with no aria-checked
// anywhere, so a screen reader announced four unchecked options and no way of
// telling which size the form was actually going to submit. `chosen` also
// decides where focus lands when Tab reaches the group.
function radio(group, chosen, label, onPick) {
  const b = document.createElement('button');
  b.type = 'button';
  b.setAttribute('role', 'radio');
  b.setAttribute('aria-checked', String(chosen));
  b.tabIndex = chosen ? 0 : -1;
  if (label) b.setAttribute('aria-label', label);
  b.addEventListener('click', onPick);
  group.appendChild(b);
  return b;
}

// Arrow keys move the selection within a group, which is what makes the roving
// tabindex above navigable rather than a cage around the first option.
function bindRadioKeys(group) {
  group.addEventListener('keydown', (e) => {
    const items = [...group.children];
    if (!items.length) return;
    const at = items.indexOf(document.activeElement);
    let next = -1;
    switch (e.key) {
      case 'ArrowRight': case 'ArrowDown': next = (Math.max(at, 0) + 1) % items.length; break;
      case 'ArrowLeft': case 'ArrowUp': next = (Math.max(at, 0) + items.length - 1) % items.length; break;
      case 'Home': next = 0; break;
      case 'End': next = items.length - 1; break;
      default: return;
    }
    e.preventDefault();
    // Selection follows focus, which is the pattern for a radio group: the
    // alternative is arrowing to an option and then having to press Space.
    items[next].click();
    // click() rebuilds the group, so the element to focus is the one now at
    // that index rather than the one that was there a moment ago.
    const rebuilt = group.children[next];
    if (rebuilt) rebuilt.focus();
  });
}

function buildChoosers() {
  el.sizes.innerHTML = '';
  SIZES.forEach((s) => {
    const on = s.w === S.choice.width && s.h === S.choice.height;
    const b = radio(el.sizes, on, s.label + ', ' + s.w + ' by ' + s.h + ', ' + s.note, () => {
      S.choice.width = s.w; S.choice.height = s.h;
      buildChoosers();
    });
    b.className = 'chip' + (on ? ' on' : '');
    const strong = document.createElement('strong');
    strong.textContent = s.label;
    const dim = document.createElement('span');
    dim.textContent = s.w + '×' + s.h;
    b.append(strong, dim);
    b.title = s.note;
  });

  el.cooldowns.innerHTML = '';
  COOLDOWNS.forEach((c) => {
    const on = c.ms === S.choice.cooldownMs;
    const b = radio(el.cooldowns, on, c.label + ' cooldown, ' + c.note, () => {
      S.choice.cooldownMs = c.ms;
      buildChoosers();
    });
    b.className = 'chip' + (on ? ' on' : '');
    b.textContent = c.label;
    b.title = c.note;
  });
  const chosen = COOLDOWNS.find((c) => c.ms === S.choice.cooldownMs);
  el.cooldownHint.textContent = chosen ? chosen.note : '';

  el.palettes.innerHTML = '';
  S.palettes.forEach((p) => {
    const on = p.key === S.choice.palette;
    const b = radio(el.palettes, on, p.name + ', ' + p.colors.length + ' colours. ' + p.note, () => {
      S.choice.palette = p.key;
      buildChoosers();
    });
    b.className = 'palette-card' + (on ? ' on' : '');
    b.title = p.note;

    // The strip is the palette drawn. Its twenty <i> elements have nothing to
    // say that the label and the colour count do not.
    const strip = document.createElement('span');
    strip.className = 'palette-strip';
    strip.setAttribute('aria-hidden', 'true');
    p.colors.forEach((hex) => {
      const sw = document.createElement('i');
      sw.style.background = hex;
      strip.appendChild(sw);
    });
    const label = document.createElement('span');
    label.className = 'palette-name';
    label.textContent = p.name;

    b.append(strip, label);
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
    el.grid.setAttribute('aria-busy', 'false');
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
    el.grid.setAttribute('aria-busy', 'false');
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
    <div class="copyrow"><input readonly id="shareUrl" aria-label="Link to this canvas" value="${escapeAttr(body.url)}"><button class="ghost" data-copy="shareUrl">copy<span class="sr-only"> the link to this canvas</span></button></div>

    <h3>Keep this one to yourself</h3>
    <p>This is the moderator link. It is what lets you pause, clear, lock and
    block. It is stored in this browser already — save it somewhere if you might
    open the canvas from a different device, because it cannot be reissued.</p>
    <div class="copyrow"><input readonly id="modUrl" aria-label="Moderator link" value="${escapeAttr(body.moderatorUrl)}"><button class="ghost" data-copy="modUrl">copy<span class="sr-only"> the moderator link</span></button></div>

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
    <p class="error" id="acError" role="alert" hidden></p>
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

// While a modal sheet is open the rest of the page stops existing: not
// focusable, not clickable, not in the accessibility tree. Without it the
// dialog is a visual effect - a screen reader reads straight past it into the
// page behind and somebody answers a question they cannot see.
const KEEP_LIVE = ['sheet', 'toast'];
function setBackgroundInert(on) {
  [...document.body.children].forEach((node) => {
    if (KEEP_LIVE.indexOf(node.id) >= 0) return;
    node.inert = on;
  });
}

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), ' +
  'select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

function focusables(root) {
  return [...root.querySelectorAll(FOCUSABLE)]
    .filter((n) => !n.hidden && n.getClientRects().length > 0);
}

let sheetReturn = null;

function openSheet(html) {
  const opening = el.sheet.hidden;
  if (opening) sheetReturn = document.activeElement;
  el.sheetBody.innerHTML = html;
  // The dialog is named by whatever heading it was just given, so the first
  // thing announced is which dialog this is.
  const heading = el.sheetBody.querySelector('h2');
  if (heading) heading.id = 'sheetTitle';
  el.sheet.hidden = false;
  if (opening) setBackgroundInert(true);
  el.sheetInner.focus();
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

function closeSheet() {
  if (el.sheet.hidden) return;
  el.sheet.hidden = true;
  setBackgroundInert(false);
  const back = sheetReturn;
  sheetReturn = null;
  // getClientRects rather than offsetParent: the header is position:sticky and
  // its buttons report no offsetParent at all.
  if (back && document.contains(back) && !back.hidden && back.getClientRects().length) back.focus();
}

// ------------------------------------------------------------------ boot ---

// Everything the page can do without the network is wired before anything is
// fetched. The buttons used to be inert until three requests had come back,
// which nobody notices with a fast connection and a mouse — and which a
// keyboard user meets head on every time, because they Tab straight to "Create
// a canvas", press Enter, and nothing whatsoever happens.
function wire() {
  buildChoosers();
  [el.sizes, el.cooldowns, el.palettes].forEach(bindRadioKeys);

  const start = $('btnStart');
  start.addEventListener('click', () => {
    el.creator.hidden = false;
    start.setAttribute('aria-expanded', 'true');
    el.creator.scrollIntoView({ behavior: scrollBehaviour(), block: 'center' });
    el.name.focus();
  });
  $('btnCancel').addEventListener('click', () => {
    el.creator.hidden = true;
    start.setAttribute('aria-expanded', 'false');
    // The form that had focus has just been removed from the page, so put it
    // back on the control that opened it rather than dropping to <body>.
    start.focus();
  });
  $('btnCreate').addEventListener('click', createRoom);
  el.name.addEventListener('keydown', (e) => { if (e.key === 'Enter') createRoom(); });
  $('btnRefresh').addEventListener('click', loadRooms);
  $('btnSheetClose').addEventListener('click', closeSheet);
  el.sheet.addEventListener('click', (e) => { if (e.target === el.sheet) closeSheet(); });
  window.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeSheet(); });

  // Tab wraps inside the open dialog. The `inert` above already achieves this
  // wherever it is supported; this is the same guarantee spelled out.
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

  el.btnAccount.addEventListener('click', async () => {
    if (S.signedIn) {
      await fetch('/api/auth/logout', { method: 'POST' });
      await refreshAccount();
      toast('signed out');
      return;
    }
    accountSheet('login');
  });
}

async function boot() {
  wire();

  try {
    const cfg = await (await fetch('/api/palettes')).json();
    S.palettes = cfg.palettes || [];
    // The palette chooser is the one part of the form that cannot be drawn
    // before the server has said which palettes exist.
    buildChoosers();
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

  await Promise.all([loadRooms(), refreshAccount()]);

  // Keep the browse list roughly current without hammering the server.
  setInterval(loadRooms, 30000);
}

boot();

})();
