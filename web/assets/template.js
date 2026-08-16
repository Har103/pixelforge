/* Template overlay: a reference image turned into palette indices so that people
 * can trace it, cell by cell, on a shared canvas.
 *
 * The image never leaves the browser. There is no upload, no fetch, no data URL
 * posted to the server and nothing written to storage — decoding, scaling and
 * quantisation all happen in this tab, and the only thing that ever reaches the
 * network is the ordinary pixel placements the user makes by hand. Somebody's
 * reference picture is their business, and the cheapest way to keep that
 * promise is for the bytes to never exist anywhere we could leak them from.
 *
 * No framework, no build step, no dependencies - the same rule the rest of the
 * front end follows.
 */
'use strict';

(() => {

// --------------------------------------------------------------- constants --

// Decoding is capped at four megapixels, a 2048×2048 equivalent. A modern phone
// photograph is comfortably above that, and none of the extra detail survives
// the scale down to a canvas a few hundred cells wide, so a larger intermediate
// buys nothing but a wedged tab and a few hundred megabytes of RGBA. Sources
// that arrive already decoded are sub-sampled to the same budget instead.
const MAX_SOURCE_PIXELS = 4 * 1024 * 1024;

// Anything below this alpha is not part of the template at all. Partial alpha
// has no meaning on a canvas whose cells are opaque palette indices, and
// compositing it against black — which is what a transparent pixel actually
// stores — would paint a dark halo around every soft edge. Absent is a better
// answer than dark.
const ALPHA_CUTOFF = 128;

// Diffused error is clamped on the way out of the accumulator. A large region
// the palette cannot represent, say a saturated red against a palette with no
// red in it, otherwise builds an error that grows without bound and turns the
// rest of the row into noise long after the offending region has ended.
const ERROR_LIMIT = 96;

// An 8×8 Bayer matrix. Eight rather than four because the coarser grid is
// visible as a pattern at the cell sizes people actually paint at.
const BAYER8 = new Uint8Array([
   0, 32,  8, 40,  2, 34, 10, 42,
  48, 16, 56, 24, 50, 18, 58, 26,
  12, 44,  4, 36, 14, 46,  6, 38,
  60, 28, 52, 20, 62, 30, 54, 22,
   3, 35, 11, 43,  1, 33,  9, 41,
  51, 19, 59, 27, 49, 17, 57, 25,
  15, 47,  7, 39, 13, 45,  5, 37,
  63, 31, 55, 23, 61, 29, 53, 21,
]);

// Error-diffusion kernels, flattened into typed arrays because the inner loop
// walks them once per cell and an array of little arrays costs a pointer chase
// each time.
function kernel(div, taps) {
  const dx = new Int8Array(taps.length);
  const dy = new Int8Array(taps.length);
  const wt = new Float32Array(taps.length);
  let maxDy = 0;
  for (let i = 0; i < taps.length; i++) {
    dx[i] = taps[i][0];
    dy[i] = taps[i][1];
    wt[i] = taps[i][2] / div;
    if (taps[i][1] > maxDy) maxDy = taps[i][1];
  }
  return { dx, dy, wt, rows: maxDy + 1 };
}

const KERNELS = {
  // Floyd–Steinberg spreads all of the error and gives the most faithful tone.
  'floyd-steinberg': kernel(16, [[1, 0, 7], [-1, 1, 3], [0, 1, 5], [1, 1, 1]]),
  // Atkinson deliberately discards three eighths of it. That costs contrast in
  // the extremes but stops a four-colour palette turning into porridge, so it
  // is the one worth reaching for on Game Boy and monochrome rooms.
  'atkinson': kernel(8, [[1, 0, 1], [2, 0, 1], [-1, 1, 1], [0, 1, 1], [1, 1, 1], [0, 2, 1]]),
};

const MODES = [
  { key: 'none', name: 'None', note: 'Nearest colour, flat. Cleanest for line art, logos and lettering.' },
  { key: 'floyd-steinberg', name: 'Floyd–Steinberg', note: 'Error diffusion. Best tone, but a restless speckle up close.' },
  { key: 'atkinson', name: 'Atkinson', note: 'Gentler diffusion. Fewer stray pixels on small palettes.' },
  { key: 'ordered', name: 'Ordered', note: 'An 8×8 Bayer grid. Repeating, so it is far easier to paint by hand.' },
];
MODES.forEach(Object.freeze);
Object.freeze(MODES);

const MODE_KEYS = MODES.map((m) => m.key);

// ------------------------------------------------------------------ colour --

const clamp255 = (v) => (v < 0 ? 0 : v > 255 ? 255 : v);
const clampErr = (v) => (v < -ERROR_LIMIT ? -ERROR_LIMIT : v > ERROR_LIMIT ? ERROR_LIMIT : v);

/* The nearest palette entry under the "redmean" metric: a weighted RGB distance
 * whose weights shift with the average red level of the pair.
 *
 * Plain RGB Euclidean distance treats an error in green as being as visible as
 * the same error in blue, which is not how eyes work, and on a small palette
 * that shows up as skin tones drifting green and dark blues collapsing into
 * black. A proper CIE Lab or CIEDE2000 comparison would be better still, but it
 * costs a gamma expansion and a cube root per channel per candidate and this
 * runs a quarter of a million times per template with the palette scanned each
 * time. Redmean recovers most of the perceptual benefit for a handful of
 * integer operations, so it is what we use until somebody has a measured reason
 * to spend more.
 *
 * `from` is the first index the quantiser is allowed to pick, which is how
 * avoidBackground keeps index 0 out of the result.
 */
function nearestIndex(pr, pg, pb, from, r, g, b) {
  const n = pr.length;
  let best = from;
  let bestD = Infinity;
  for (let i = from; i < n; i++) {
    const cr = pr[i];
    const dr = r - cr, dg = g - pg[i], db = b - pb[i];
    const rm = (r + cr) >> 1;
    const d = (((512 + rm) * dr * dr) >> 8) + 4 * dg * dg + (((767 - rm) * db * db) >> 8);
    if (d < bestD) { bestD = d; best = i; }
  }
  return best;
}

function parsePalette(list) {
  if (!Array.isArray(list) || list.length === 0) {
    throw new Error('a palette of at least one "#rrggbb" colour is required');
  }
  const n = list.length;
  const pr = new Uint8Array(n), pg = new Uint8Array(n), pb = new Uint8Array(n);
  const rgb = new Array(n);
  for (let i = 0; i < n; i++) {
    const hex = typeof list[i] === 'string' ? list[i].trim() : '';
    if (!/^#[0-9a-fA-F]{6}$/.test(hex)) {
      throw new Error('palette entry ' + i + ' is not a "#rrggbb" colour: ' + JSON.stringify(list[i]));
    }
    const v = parseInt(hex.slice(1), 16);
    pr[i] = (v >> 16) & 255;
    pg[i] = (v >> 8) & 255;
    pb[i] = v & 255;
    rgb[i] = [pr[i], pg[i], pb[i]];
  }
  return { pr, pg, pb, rgb, length: n };
}

/* How far apart the palette colours sit, in per-channel units. An ordered
 * dither needs to nudge each cell by roughly half the gap to the next available
 * colour: four Game Boy greens want a large nudge, twenty colours with shading
 * in them want a small one, and a fixed constant is visibly wrong for one of
 * them whichever constant you pick. Measuring the mean distance from each
 * colour to its nearest neighbour adapts to both.
 */
function paletteSpread(pr, pg, pb, from) {
  const n = pr.length;
  if (n - from < 2) return 0;
  let sum = 0;
  for (let i = from; i < n; i++) {
    let best = Infinity;
    for (let j = from; j < n; j++) {
      if (j === i) continue;
      const dr = pr[i] - pr[j], dg = pg[i] - pg[j], db = pb[i] - pb[j];
      const d = dr * dr + dg * dg + db * db;
      if (d < best) best = d;
    }
    sum += Math.sqrt(best / 3);
  }
  // The cap keeps a two-colour palette from producing a bias so large that the
  // Bayer pattern reads as the picture rather than as its texture.
  return Math.min(sum / (n - from), 128);
}

// ------------------------------------------------------------------ source --

function isPixelBag(source) {
  return !!source && source.data && typeof source.width === 'number' && typeof source.height === 'number';
}

/* Decode whatever we were handed into a bitmap we can draw.
 *
 * Returns { bitmap, owned }. Ownership matters: a bitmap we created ourselves
 * should be closed as soon as it has been rasterised so the memory goes back
 * immediately, but a bitmap the caller passed in is theirs and closing it would
 * break the next thing they draw with it.
 */
function toBitmap(source) {
  if (!source) return Promise.reject(new Error('no image was supplied'));

  if (typeof ImageBitmap !== 'undefined' && source instanceof ImageBitmap) {
    return Promise.resolve({ bitmap: source, owned: false });
  }
  if (typeof HTMLCanvasElement !== 'undefined' && source instanceof HTMLCanvasElement) {
    return Promise.resolve({ bitmap: source, owned: false });
  }
  if (typeof HTMLImageElement !== 'undefined' && source instanceof HTMLImageElement) {
    if (source.complete && source.naturalWidth) return Promise.resolve({ bitmap: source, owned: false });
    return new Promise((resolve, reject) => {
      source.addEventListener('load', () => resolve({ bitmap: source, owned: false }), { once: true });
      source.addEventListener('error', () => reject(new Error('that image could not be decoded')), { once: true });
    });
  }
  if (typeof Blob !== 'undefined' && source instanceof Blob) {
    // createImageBitmap keeps the bytes in the browser and, unlike an <img>,
    // produces an origin-clean bitmap even from a page opened over file://, so
    // the readback below is not tainted.
    if (typeof createImageBitmap === 'function') {
      return createImageBitmap(source).then(
        (b) => ({ bitmap: b, owned: true }),
        () => { throw new Error('that file is not an image this browser can decode'); });
    }
    return fromObjectURL(source);
  }
  return Promise.reject(new Error('unsupported image source; expected a File, Blob, ImageBitmap, <img>, <canvas> or ImageData'));
}

// The fallback for browsers without createImageBitmap. An object URL is a
// handle to a blob held by this document: it is never sent anywhere, and it is
// revoked the moment the image has decoded so it cannot outlive the load.
function fromObjectURL(blob) {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(blob);
    const img = new Image();
    img.onload = () => { URL.revokeObjectURL(url); resolve({ bitmap: img, owned: false }); };
    img.onerror = () => { URL.revokeObjectURL(url); reject(new Error('that file is not an image this browser can decode')); };
    img.src = url;
  });
}

function scratchCanvas(w, h) {
  // OffscreenCanvas so this module still works if it is ever moved into a
  // worker; the DOM canvas is the ordinary path.
  if (typeof OffscreenCanvas !== 'undefined') return new OffscreenCanvas(w, h);
  const c = document.createElement('canvas');
  c.width = w;
  c.height = h;
  return c;
}

function rasterise(bitmap, owned) {
  // naturalWidth first: on an <img> that the page has sized with CSS, .width is
  // the layout width and would quietly resample the template to whatever the
  // stylesheet happened to say. ImageBitmap and <canvas> have no natural size
  // and fall through to .width, which for them is the pixel count.
  const sw = bitmap.naturalWidth || bitmap.width || 0;
  const sh = bitmap.naturalHeight || bitmap.height || 0;
  if (!sw || !sh) throw new Error('that image has no pixels: it decoded to ' + sw + '×' + sh);

  let w = sw, h = sh;
  const total = sw * sh;
  if (total > MAX_SOURCE_PIXELS) {
    const k = Math.sqrt(MAX_SOURCE_PIXELS / total);
    w = Math.max(1, Math.round(sw * k));
    h = Math.max(1, Math.round(sh * k));
  }

  const c = scratchCanvas(w, h);
  const g = c.getContext('2d', { willReadFrequently: true });
  // Smoothing on, because this first reduction is a plain resample and the
  // browser's is better than anything worth hand-rolling. The cell-level box
  // sampling below is the part that has to be exact.
  g.imageSmoothingEnabled = true;
  if ('imageSmoothingQuality' in g) g.imageSmoothingQuality = 'high';
  g.clearRect(0, 0, w, h);
  g.drawImage(bitmap, 0, 0, sw, sh, 0, 0, w, h);
  const img = g.getImageData(0, 0, w, h);
  if (owned && typeof bitmap.close === 'function') bitmap.close();
  return { data: img.data, width: w, height: h };
}

function readSource(source) {
  // Anything already shaped like an ImageData is used as it stands. That is the
  // cheap path for a caller holding pixels, and it is what lets this module be
  // tested without a DOM.
  if (isPixelBag(source)) {
    return Promise.resolve({ data: source.data, width: source.width | 0, height: source.height | 0 });
  }
  return toBitmap(source).then(({ bitmap, owned }) => rasterise(bitmap, owned));
}

// ---------------------------------------------------------------- sampling --

/* Average the source pixels that fall in each cell.
 *
 * Averaging first, rather than picking one source pixel per cell, is what makes
 * a photograph still readable at 128 cells across: detail too fine to survive
 * becomes tone, and the dither then has something honest to work with. Nearest
 * neighbour turns the same photograph into confetti.
 *
 * Fills `cells` with r,g,b per cell and `mask` with 1 where the cell is opaque
 * enough to be part of the template.
 */
function boxSample(src, tw, th, cells, mask) {
  const data = src.data;
  const sw = src.width, sh = src.height;

  // Work stays bounded even for an enormous ImageData handed straight to us:
  // past the budget we stride through each cell's box instead of reading every
  // pixel in it. Averaging sixteen samples rather than four hundred is not a
  // visible difference at these sizes.
  const step = Math.max(1, Math.ceil(Math.sqrt((sw * sh) / MAX_SOURCE_PIXELS)));
  const rowStep = step * sw * 4;
  const colStep = step * 4;

  for (let ty = 0; ty < th; ty++) {
    let y0 = Math.floor((ty * sh) / th);
    if (y0 >= sh) y0 = sh - 1;
    let y1 = Math.floor(((ty + 1) * sh) / th);
    if (y1 <= y0) y1 = y0 + 1;
    if (y1 > sh) y1 = sh;

    for (let tx = 0; tx < tw; tx++) {
      let x0 = Math.floor((tx * sw) / tw);
      if (x0 >= sw) x0 = sw - 1;
      let x1 = Math.floor(((tx + 1) * sw) / tw);
      if (x1 <= x0) x1 = x0 + 1;
      if (x1 > sw) x1 = sw;

      let ar = 0, ag = 0, ab = 0, aa = 0, n = 0;
      for (let sy = y0, row = (y0 * sw + x0) * 4; sy < y1; sy += step, row += rowStep) {
        for (let sx = x0, o = row; sx < x1; sx += step, o += colStep) {
          const a = data[o + 3];
          // Colour is weighted by alpha, so a soft edge averages towards its own
          // colour rather than towards the black a transparent pixel happens to
          // hold. This is the "composite against nothing" rule at cell scale.
          ar += data[o] * a;
          ag += data[o + 1] * a;
          ab += data[o + 2] * a;
          aa += a;
          n++;
        }
      }

      const cell = ty * tw + tx;
      if (aa === 0 || aa / n < ALPHA_CUTOFF) {
        mask[cell] = 0;
        continue;
      }
      mask[cell] = 1;
      const o = cell * 3;
      cells[o] = ar / aa;
      cells[o + 1] = ag / aa;
      cells[o + 2] = ab / aa;
    }
  }
}

// -------------------------------------------------------------- quantising --

function quantiseFlat(cells, mask, count, pr, pg, pb, from, out) {
  for (let i = 0; i < count; i++) {
    if (!mask[i]) continue;
    const o = i * 3;
    out[i] = nearestIndex(pr, pg, pb, from, cells[o], cells[o + 1], cells[o + 2]);
  }
}

function quantiseOrdered(cells, mask, w, h, pr, pg, pb, from, strength, out) {
  const spread = paletteSpread(pr, pg, pb, from) * strength;
  for (let y = 0; y < h; y++) {
    const brow = (y & 7) * 8;
    for (let x = 0; x < w; x++) {
      const i = y * w + x;
      if (!mask[i]) continue;
      const bias = (BAYER8[brow + (x & 7)] / 64 - 0.5) * spread;
      const o = i * 3;
      out[i] = nearestIndex(pr, pg, pb, from,
        clamp255(cells[o] + bias), clamp255(cells[o + 1] + bias), clamp255(cells[o + 2] + bias));
    }
  }
}

function quantiseDiffused(cells, mask, w, h, pr, pg, pb, from, k, out) {
  const { dx, dy, wt, rows } = k;
  const taps = dx.length;

  // One error row per row the kernel can reach forward into, recycled as we go.
  const err = new Array(rows);
  for (let i = 0; i < rows; i++) err[i] = new Float32Array(w * 3);

  for (let y = 0; y < h; y++) {
    const cur = err[y % rows];
    // Serpentine: alternate rows run right to left. Always sweeping the same
    // way pushes error consistently in one direction and leaves a visible
    // diagonal grain, which on a grid people paint by hand is very obvious.
    const forward = (y & 1) === 0;

    for (let step = 0; step < w; step++) {
      const x = forward ? step : w - 1 - step;
      const i = y * w + x;
      // A cell outside the template is not painted, so it neither accepts error
      // nor produces any.
      if (!mask[i]) continue;

      const o = i * 3;
      const e = x * 3;
      const r = clamp255(cells[o] + clampErr(cur[e]));
      const g = clamp255(cells[o + 1] + clampErr(cur[e + 1]));
      const b = clamp255(cells[o + 2] + clampErr(cur[e + 2]));

      const idx = nearestIndex(pr, pg, pb, from, r, g, b);
      out[i] = idx;

      // Taking the error against the clamped value, not the raw sum, is what
      // bounds it: an out-of-gamut region can never manufacture more error than
      // one channel's worth per cell.
      const er = r - pr[idx], eg = g - pg[idx], eb = b - pb[idx];

      for (let t = 0; t < taps; t++) {
        const nx = x + (forward ? dx[t] : -dx[t]);
        const ny = y + dy[t];
        if (nx < 0 || nx >= w || ny >= h) continue;
        // Error stops at the edge of the template. Letting it bleed into cells
        // that will never be painted, or across a hole into an unrelated part
        // of the picture, draws a bright fringe along every cut-out. The share
        // aimed at a masked-out neighbour is dropped rather than shared out
        // among the survivors: redistributing dumps a whole cell's error into
        // whichever neighbour is left, which is the same artefact, louder.
        if (!mask[ny * w + nx]) continue;
        const row = err[ny % rows];
        const p = nx * 3;
        const f = wt[t];
        row[p] += er * f;
        row[p + 1] += eg * f;
        row[p + 2] += eb * f;
      }
    }

    // This buffer is about to become row y + rows, so it starts clean.
    cur.fill(0);
  }
}

// ---------------------------------------------------------------- template --

function makeTemplate(width, height, indices, mask, ownRGB) {
  let frame = null;         // reused by rgba()
  let lastArg = null;       // memo so a per-frame caller does not re-parse hex
  let lastRGB = ownRGB;

  function resolveRGB(paletteRGB) {
    if (paletteRGB === undefined || paletteRGB === null) return ownRGB;
    if (paletteRGB === lastArg) return lastRGB;
    let out = paletteRGB;
    if (Array.isArray(paletteRGB) && typeof paletteRGB[0] === 'string') {
      out = paletteRGB.map((hex) => {
        const v = parseInt(String(hex).slice(1), 16) | 0;
        return [(v >> 16) & 255, (v >> 8) & 255, v & 255];
      });
    }
    lastArg = paletteRGB;
    lastRGB = out;
    return out;
  }

  const t = {
    width,
    height,
    indices,
    mask,
    x: 0,
    y: 0,

    setOffset(x, y) {
      t.x = Math.round(x) | 0;
      t.y = Math.round(y) | 0;
    },

    /* RGBA for the whole template, transparent where the mask is 0.
     *
     * The buffer is allocated once and rewritten in place. An overlay is
     * redrawn on every pan, every zoom and every remote placement, and handing
     * out a fresh megabyte each time is exactly the churn that makes a canvas
     * feel sticky. A caller that needs to hold on to the pixels should take its
     * own copy.
     */
    rgba(paletteRGB) {
      const rgb = resolveRGB(paletteRGB);
      if (!frame) frame = new Uint8ClampedArray(width * height * 4);
      for (let i = 0, o = 0; i < indices.length; i++, o += 4) {
        if (!mask[i]) { frame[o + 3] = 0; continue; }
        const c = rgb[indices[i]] || rgb[0];
        frame[o] = c[0];
        frame[o + 1] = c[1];
        frame[o + 2] = c[2];
        frame[o + 3] = 255;
      }
      return frame;
    },

    /* How much of the template is already on the canvas.
     *
     * `pixels` is the whole canvas as palette indices and `canvasW` its width;
     * the height follows from the length. Cells outside the mask are not
     * counted, and neither are cells the current offset has pushed off the edge
     * of the canvas, because a template dragged half over the border should
     * report on the half that can actually be painted rather than pinning
     * itself below 100% forever.
     */
    progress(pixels, canvasW) {
      const cw = canvasW | 0;
      const ch = cw > 0 ? Math.floor(pixels.length / cw) : 0;
      const ox = t.x, oy = t.y;

      // The next target is the mismatched cell nearest the middle of the
      // template, not the first one in scan order. Filling outward from the
      // centre makes a picture recognisable far sooner than filling from a
      // corner, it keeps the painted region contiguous so it is easy to see and
      // defend, and it stops every client that asks being sent to the same
      // top-left cell where they collide on one placement.
      const cx = (width - 1) / 2;
      const cy = (height - 1) / 2;

      let total = 0, done = 0;
      let bestD = Infinity, bx = 0, by = 0, bWant = 0, found = false;

      for (let ty = 0; ty < height; ty++) {
        const gy = oy + ty;
        if (gy < 0 || gy >= ch) continue;
        const dy = ty - cy;
        const dy2 = dy * dy;
        const rowT = ty * width;
        const rowC = gy * cw;
        for (let tx = 0; tx < width; tx++) {
          const i = rowT + tx;
          if (!mask[i]) continue;
          const gx = ox + tx;
          if (gx < 0 || gx >= cw) continue;
          total++;
          const want = indices[i];
          if (pixels[rowC + gx] === want) { done++; continue; }
          const dx = tx - cx;
          const d = dx * dx + dy2;
          if (d < bestD) { bestD = d; bx = gx; by = gy; bWant = want; found = true; }
        }
      }

      return {
        total,
        done,
        // Left unrounded on purpose: a caller that wants "99%" rather than a
        // premature "100%" needs the fraction, not our idea of how to round it.
        percent: total === 0 ? 100 : (done / total) * 100,
        nextMismatch: found ? { x: bx, y: by, want: bWant } : null,
      };
    },
  };

  return t;
}

// --------------------------------------------------------------------- api --

function build(src, pal, maxW, maxH, opts) {
  const sw = src.width | 0, sh = src.height | 0;
  if (sw <= 0 || sh <= 0) {
    throw new Error('that image has no area: it measured ' + sw + '×' + sh);
  }
  if (!src.data || src.data.length < sw * sh * 4) {
    throw new Error('that image is short of pixel data: expected ' + (sw * sh * 4) +
      ' bytes for ' + sw + '×' + sh + ', got ' + (src.data ? src.data.length : 0));
  }

  // Fit, never stretch. Upscaling is off by default because blowing a 32×32
  // sprite up to fill a 500-cell canvas is almost never what somebody meant,
  // and the caller can ask for it when it is.
  let k = Math.min(maxW / sw, maxH / sh);
  if (!opts.upscale && k > 1) k = 1;
  const tw = Math.max(1, Math.min(maxW, Math.round(sw * k)));
  const th = Math.max(1, Math.min(maxH, Math.round(sh * k)));

  const d = typeof opts.dither === 'string' ? { mode: opts.dither } : (opts.dither || {});
  const mode = d.mode === undefined || d.mode === null ? 'none' : String(d.mode);
  if (MODE_KEYS.indexOf(mode) < 0) {
    throw new Error('unknown dither mode ' + JSON.stringify(mode) + '; expected one of ' + MODE_KEYS.join(', '));
  }

  // Index 0 is the canvas background. Quantising a large flat region to it
  // leaves a hole in the middle of the template rather than a shape, which
  // reads as a mistake, so rooms that care can take index 0 off the table.
  const from = d.avoidBackground === true ? 1 : 0;
  if (from >= pal.length) {
    throw new Error('avoidBackground needs a palette with more than one colour');
  }
  const strength = typeof d.strength === 'number' && isFinite(d.strength) ? Math.max(0, d.strength) : 1;

  const count = tw * th;
  const cells = new Uint8ClampedArray(count * 3);
  const mask = new Uint8Array(count);
  boxSample(src, tw, th, cells, mask);

  // Masked-out cells keep the first legal index rather than a zero that
  // avoidBackground has just promised nobody would see. Their value is never
  // painted, but an array that is uniformly a valid palette index is one fewer
  // thing for a caller to get wrong.
  const indices = new Uint8Array(count);
  if (from !== 0) indices.fill(from);

  const { pr, pg, pb } = pal;
  if (mode === 'ordered') {
    quantiseOrdered(cells, mask, tw, th, pr, pg, pb, from, strength, indices);
  } else if (KERNELS[mode]) {
    quantiseDiffused(cells, mask, tw, th, pr, pg, pb, from, KERNELS[mode], indices);
  } else {
    quantiseFlat(cells, mask, count, pr, pg, pb, from, indices);
  }

  return makeTemplate(tw, th, indices, mask, pal.rgb);
}

window.PFTemplate = {
  /* Turn a reference image into a Template.
   *
   * `source` may be a File or Blob from a drop or an <input type=file>, an
   * ImageBitmap, an <img>, a <canvas>, or anything already shaped like an
   * ImageData. `palette` is the room's colours as "#rrggbb", `maxW`/`maxH` the
   * canvas dimensions to fit inside, and `dither` either a mode key or
   * { mode, avoidBackground, strength }. `upscale: true` allows a source
   * smaller than the canvas to be enlarged.
   *
   * Everything after the decode is synchronous work over typed arrays; the
   * promise exists because decoding an image is asynchronous, not because any
   * of this touches the network.
   */
  load(source, options) {
    const opts = options || {};
    // Validation runs inside the promise so a bad palette rejects rather than
    // throwing past a caller who quite reasonably only wrote a .catch().
    return Promise.resolve().then(() => {
      const pal = parsePalette(opts.palette);
      const maxW = Math.floor(opts.maxW);
      const maxH = Math.floor(opts.maxH);
      if (!(maxW > 0) || !(maxH > 0)) {
        throw new Error('maxW and maxH must be positive: got ' + opts.maxW + '×' + opts.maxH);
      }
      return readSource(source).then((src) => build(src, pal, maxW, maxH, opts));
    });
  },

  modes: MODES,
};

})();
