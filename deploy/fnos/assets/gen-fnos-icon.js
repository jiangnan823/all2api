// Generates fnOS app icons (PNG, RGBA) without external deps.
// Motif: fan-in gateway — three source dots converge into one endpoint.
const fs = require('fs');
const path = require('path');
const zlib = require('zlib');

const SS = 4; // supersample factor for anti-aliased edges

function crc32(buf) {
  let c, table = crc32.table;
  if (!table) {
    table = crc32.table = new Int32Array(256);
    for (let n = 0; n < 256; n++) {
      c = n;
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      table[n] = c;
    }
  }
  c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) c = table[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const len = Buffer.alloc(4); len.writeUInt32BE(data.length, 0);
  const t = Buffer.from(type, 'ascii');
  const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(Buffer.concat([t, data])), 0);
  return Buffer.concat([len, t, data, crc]);
}

function encodePng(size, rgba) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(size, 0); ihdr.writeUInt32BE(size, 4);
  ihdr[8] = 8; ihdr[9] = 6; // 8-bit RGBA
  const stride = size * 4;
  const raw = Buffer.alloc((stride + 1) * size);
  for (let y = 0; y < size; y++) {
    raw[y * (stride + 1)] = 0; // filter: none
    rgba.copy(raw, y * (stride + 1) + 1, y * stride, y * stride + stride);
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', zlib.deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ]);
}

// --- signed distances -------------------------------------------------------
const sdCircle = (px, py, cx, cy, r) => Math.hypot(px - cx, py - cy) - r;
function sdSegment(px, py, ax, ay, bx, by) {
  const vx = bx - ax, vy = by - ay, wx = px - ax, wy = py - ay;
  const t = Math.max(0, Math.min(1, (wx * vx + wy * vy) / (vx * vx + vy * vy || 1e-9)));
  return Math.hypot(px - (ax + vx * t), py - (ay + vy * t));
}
const sdRoundRect = (px, py, w, h, r) => {
  const qx = Math.abs(px - w / 2) - (w / 2 - r), qy = Math.abs(py - h / 2) - (h / 2 - r);
  return Math.hypot(Math.max(qx, 0), Math.max(qy, 0)) + Math.min(Math.max(qx, qy), 0) - r;
};
const cover = (d, px) => Math.max(0, Math.min(1, 0.5 - d / (2 / px))); // AA over ~1 device px

function render(size) {
  const hi = size * SS, rgba = Buffer.alloc(hi * hi * 4);
  const bgTop = [0x4d, 0x84, 0xff], bgBot = [0x25, 0x59, 0xc4], fg = [0xff, 0xff, 0xff];
  const rad = hi * 0.225;
  const target = [hi * 0.66, hi * 0.5], targetR = hi * 0.115;
  const sources = [[hi * 0.3, hi * 0.27], [hi * 0.3, hi * 0.5], [hi * 0.3, hi * 0.73]];
  const srcR = hi * 0.082, stroke = hi * 0.052;

  for (let y = 0; y < hi; y++) {
    for (let x = 0; x < hi; x++) {
      const px = x + 0.5, py = y + 0.5;
      const aBg = cover(sdRoundRect(px, py, hi, hi, rad), SS);
      if (aBg <= 0) continue;
      const mix = py / hi;
      let r = bgTop[0] + (bgBot[0] - bgTop[0]) * mix;
      let g = bgTop[1] + (bgBot[1] - bgTop[1]) * mix;
      let b = bgTop[2] + (bgBot[2] - bgTop[2]) * mix;

      // glyph union: connectors + target ring + source dots
      let d = hi;
      for (const [sx, sy] of sources) {
        d = Math.min(d, sdSegment(px, py, sx, sy, target[0], target[1]) - stroke / 2);
        d = Math.min(d, sdCircle(px, py, sx, sy, srcR));
      }
      d = Math.min(d, sdCircle(px, py, target[0], target[1], targetR));
      const aFg = cover(d, SS);

      const o = (y * hi + x) * 4;
      rgba[o] = r + (fg[0] - r) * aFg;
      rgba[o + 1] = g + (fg[1] - g) * aFg;
      rgba[o + 2] = b + (fg[2] - b) * aFg;
      rgba[o + 3] = 255 * aBg;
    }
  }

  // box downsample SS x SS -> size
  const out = Buffer.alloc(size * size * 4);
  const n2 = SS * SS;
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      let r = 0, g = 0, b = 0, a = 0;
      for (let dy = 0; dy < SS; dy++) {
        for (let dx = 0; dx < SS; dx++) {
          const o = ((y * SS + dy) * hi + (x * SS + dx)) * 4;
          const al = rgba[o + 3] / 255;
          r += rgba[o] * al; g += rgba[o + 1] * al; b += rgba[o + 2] * al; a += rgba[o + 3];
        }
      }
      const A = a / n2 / 255, ao = (y * size + x) * 4;
      out[ao] = Math.round(A > 0 ? (r / n2) / A : 0);
      out[ao + 1] = Math.round(A > 0 ? (g / n2) / A : 0);
      out[ao + 2] = Math.round(A > 0 ? (b / n2) / A : 0);
      out[ao + 3] = Math.round(A * 255);
    }
  }
  return encodePng(size, out);
}

const dir = process.argv[2];
fs.mkdirSync(dir, { recursive: true });
for (const s of [64, 256]) {
  const file = path.join(dir, `icon_${s}.png`);
  fs.writeFileSync(file, render(s));
  console.log(`wrote ${file} (${fs.statSync(file).size} bytes)`);
}
