// A self-contained QR encoder for the pairing panel.
//
// Byte mode only, error correction level M, auto version, auto mask — chosen to
// match Python's `qrcode` package (`QRCode(error_correction=ERROR_CORRECT_M)`),
// which is the reference this implementation is verified against. The pairing
// URL is always ASCII, so byte mode is the only mode it ever needs.
//
// The encoder returns the raw module matrix (true = dark); rendering is a
// separate concern so the matrix can be diffed against a reference without
// getting tangled up in pixels.

/** Module matrix: true is a dark module. */
export type QrMatrix = boolean[][];

export interface QrCode {
  version: number;
  /** matrix edge length, in modules. */
  size: number;
  modules: QrMatrix;
}

// -- GF(256) ----------------------------------------------------------------

const EXP = new Array<number>(256);
const LOG = new Array<number>(256);
for (let i = 0; i < 8; i++) EXP[i] = 1 << i;
for (let i = 8; i < 256; i++) EXP[i] = EXP[i - 4] ^ EXP[i - 5] ^ EXP[i - 6] ^ EXP[i - 8];
for (let i = 0; i < 255; i++) LOG[EXP[i]] = i;

function gexp(n: number): number {
  return EXP[n % 255];
}

/** n must be non-zero. */
function glog(n: number): number {
  return LOG[n];
}

// -- Reed-Solomon block table ----------------------------------------------

// level 0=L, 1=M, 2=Q, 3=H. Each entry is [count, total, data] triples.
const RS_BLOCK_TABLE: number[][][] = [
  [[1, 26, 19], [1, 26, 16], [1, 26, 13], [1, 26, 9]],
  [[1, 44, 34], [1, 44, 28], [1, 44, 22], [1, 44, 16]],
  [[1, 70, 55], [1, 70, 44], [2, 35, 17], [2, 35, 13]],
  [[1, 100, 80], [2, 50, 32], [2, 50, 24], [4, 25, 9]],
  [[1, 134, 108], [2, 67, 43], [2, 33, 15, 2, 34, 16], [2, 33, 11, 2, 34, 12]],
  [[2, 86, 68], [4, 43, 27], [4, 43, 19], [4, 43, 15]],
  [[2, 98, 78], [4, 49, 31], [2, 32, 14, 4, 33, 15], [4, 39, 13, 1, 40, 14]],
  [[2, 121, 97], [2, 60, 38, 2, 61, 39], [4, 40, 18, 2, 41, 19], [4, 40, 14, 2, 41, 15]],
  [[2, 146, 116], [3, 58, 36, 2, 59, 37], [4, 36, 16, 4, 37, 17], [4, 36, 12, 4, 37, 13]],
  [[2, 86, 68, 2, 87, 69], [4, 69, 43, 1, 70, 44], [6, 43, 19, 2, 44, 20], [6, 43, 15, 2, 44, 16]],
  [[4, 101, 81], [1, 80, 50, 4, 81, 51], [4, 50, 22, 4, 51, 23], [3, 36, 12, 8, 37, 13]],
  [[2, 116, 92, 2, 117, 93], [6, 58, 36, 2, 59, 37], [4, 46, 20, 6, 47, 21], [7, 42, 14, 4, 43, 15]],
  [[4, 133, 107], [8, 59, 37, 1, 60, 38], [8, 44, 20, 4, 45, 21], [12, 33, 11, 4, 34, 12]],
  [[3, 145, 115, 1, 146, 116], [4, 64, 40, 5, 65, 41], [11, 36, 16, 5, 37, 17], [11, 36, 12, 5, 37, 13]],
  [[5, 109, 87, 1, 110, 88], [5, 65, 41, 5, 66, 42], [5, 54, 24, 7, 55, 25], [11, 36, 12, 7, 37, 13]],
  [[5, 122, 98, 1, 123, 99], [7, 73, 45, 3, 74, 46], [15, 43, 19, 2, 44, 20], [3, 45, 15, 13, 46, 16]],
  [[1, 135, 107, 5, 136, 108], [10, 74, 46, 1, 75, 47], [1, 50, 22, 15, 51, 23], [2, 42, 14, 17, 43, 15]],
  [[5, 150, 120, 1, 151, 121], [9, 69, 43, 4, 70, 44], [17, 50, 22, 1, 51, 23], [2, 42, 14, 19, 43, 15]],
  [[3, 141, 113, 4, 142, 114], [3, 70, 44, 11, 71, 45], [17, 47, 21, 4, 48, 22], [9, 39, 13, 16, 40, 14]],
  [[3, 135, 107, 5, 136, 108], [3, 67, 41, 13, 68, 42], [15, 54, 24, 5, 55, 25], [15, 43, 15, 10, 44, 16]],
  [[4, 144, 116, 4, 145, 117], [17, 68, 42], [17, 50, 22, 6, 51, 23], [19, 46, 16, 6, 47, 17]],
  [[2, 139, 111, 7, 140, 112], [17, 74, 46], [7, 54, 24, 16, 55, 25], [34, 37, 13]],
  [[4, 151, 121, 5, 152, 122], [4, 75, 47, 14, 76, 48], [11, 54, 24, 14, 55, 25], [16, 45, 15, 14, 46, 16]],
  [[6, 147, 117, 4, 148, 118], [6, 73, 45, 14, 74, 46], [11, 54, 24, 16, 55, 25], [30, 46, 16, 2, 47, 17]],
  [[8, 132, 106, 4, 133, 107], [8, 75, 47, 13, 76, 48], [7, 54, 24, 22, 55, 25], [22, 45, 15, 13, 46, 16]],
  [[10, 142, 114, 2, 143, 115], [19, 74, 46, 4, 75, 47], [28, 50, 22, 6, 51, 23], [33, 46, 16, 4, 47, 17]],
  [[8, 152, 122, 4, 153, 123], [22, 73, 45, 3, 74, 46], [8, 53, 23, 26, 54, 24], [12, 45, 15, 28, 46, 16]],
  [[3, 147, 117, 10, 148, 118], [3, 73, 45, 23, 74, 46], [4, 54, 24, 31, 55, 25], [11, 45, 15, 31, 46, 16]],
  [[7, 146, 116, 7, 147, 117], [21, 73, 45, 7, 74, 46], [1, 53, 23, 37, 54, 24], [19, 45, 15, 26, 46, 16]],
  [[5, 145, 115, 10, 146, 116], [19, 75, 47, 10, 76, 48], [15, 54, 24, 25, 55, 25], [23, 45, 15, 25, 46, 16]],
  [[13, 145, 115, 3, 146, 116], [2, 74, 46, 29, 75, 47], [42, 54, 24, 1, 55, 25], [23, 45, 15, 28, 46, 16]],
  [[17, 145, 115], [10, 74, 46, 23, 75, 47], [10, 54, 24, 35, 55, 25], [19, 45, 15, 35, 46, 16]],
  [[17, 145, 115, 1, 146, 116], [14, 74, 46, 21, 75, 47], [29, 54, 24, 19, 55, 25], [11, 45, 15, 46, 46, 16]],
  [[13, 145, 115, 6, 146, 116], [14, 74, 46, 23, 75, 47], [44, 54, 24, 7, 55, 25], [59, 46, 16, 1, 47, 17]],
  [[12, 151, 121, 7, 152, 122], [12, 75, 47, 26, 76, 48], [39, 54, 24, 14, 55, 25], [22, 45, 15, 41, 46, 16]],
  [[6, 151, 121, 14, 152, 122], [6, 75, 47, 34, 76, 48], [46, 54, 24, 10, 55, 25], [2, 45, 15, 64, 46, 16]],
  [[17, 152, 122, 4, 153, 123], [29, 74, 46, 14, 75, 47], [49, 54, 24, 10, 55, 25], [24, 45, 15, 46, 46, 16]],
  [[4, 152, 122, 18, 153, 123], [13, 74, 46, 32, 75, 47], [48, 54, 24, 14, 55, 25], [42, 45, 15, 32, 46, 16]],
  [[20, 147, 117, 4, 148, 118], [40, 75, 47, 7, 76, 48], [43, 54, 24, 22, 55, 25], [10, 45, 15, 67, 46, 16]],
  [[19, 148, 118, 6, 149, 119], [18, 75, 47, 31, 76, 48], [34, 54, 24, 34, 55, 25], [20, 45, 15, 61, 46, 16]],
];

// Alignment pattern centres per version; index = version - 1.
const PATTERN_POSITION_TABLE: number[][] = [
  [], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34], [6, 22, 38], [6, 24, 42],
  [6, 26, 46], [6, 28, 50], [6, 30, 54], [6, 32, 58], [6, 34, 62], [6, 26, 46, 66],
  [6, 26, 48, 70], [6, 26, 50, 74], [6, 30, 54, 78], [6, 30, 56, 82], [6, 30, 58, 86],
  [6, 34, 62, 90], [6, 28, 50, 72, 94], [6, 26, 50, 74, 98], [6, 30, 54, 78, 102],
  [6, 28, 54, 80, 106], [6, 32, 58, 84, 110], [6, 30, 58, 86, 114], [6, 34, 62, 90, 118],
  [6, 26, 50, 74, 98, 122], [6, 30, 54, 78, 102, 126], [6, 26, 52, 78, 104, 130],
  [6, 30, 56, 82, 108, 134], [6, 34, 60, 86, 112, 138], [6, 30, 58, 86, 114, 142],
  [6, 34, 62, 90, 118, 146], [6, 30, 54, 78, 102, 126, 150], [6, 24, 50, 76, 102, 128, 154],
  [6, 28, 54, 80, 106, 132, 158], [6, 32, 58, 84, 110, 136, 162], [6, 26, 54, 82, 110, 138, 166],
  [6, 30, 58, 86, 114, 142, 170],
];

// Error correction level -> 2-bit format-info indicator (L, M, Q, H).
const EC_BITS = [1, 0, 3, 2];

const G15 = 0x537;
const G15_MASK = 0x5412;
const G18 = 0x1f25;
const PAD0 = 0xec;
const PAD1 = 0x11;

// -- BCH --------------------------------------------------------------------

function bchDigit(n: number): number {
  let d = 0;
  while (n !== 0) {
    d++;
    n >>>= 1;
  }
  return d;
}

function bchTypeInfo(data: number): number {
  let d = data << 10;
  while (bchDigit(d) - bchDigit(G15) >= 0) {
    d ^= G15 << (bchDigit(d) - bchDigit(G15));
  }
  return ((data << 10) | d) ^ G15_MASK;
}

function bchTypeNumber(data: number): number {
  let d = data << 12;
  while (bchDigit(d) - bchDigit(G18) >= 0) {
    d ^= G18 << (bchDigit(d) - bchDigit(G18));
  }
  return (data << 12) | d;
}

// -- Reed-Solomon -----------------------------------------------------------

function stripLeadingZeros(poly: number[]): number[] {
  let i = 0;
  while (i < poly.length && poly[i] === 0) i++;
  return poly.slice(i);
}

function rsGeneratorPoly(ecCount: number): number[] {
  let poly = [1];
  for (let i = 0; i < ecCount; i++) {
    poly = polyMul(poly, [1, gexp(i)]);
  }
  return poly;
}

/** a and b are coefficient arrays, highest degree first; all coefficients of b non-zero. */
function polyMul(a: number[], b: number[]): number[] {
  const out = new Array<number>(a.length + b.length - 1).fill(0);
  for (let i = 0; i < a.length; i++) {
    for (let j = 0; j < b.length; j++) {
      out[i + j] ^= gexp(glog(a[i]) + glog(b[j]));
    }
  }
  return out;
}

/** Remainder of dividend mod divisor; both highest degree first, divisor monic. */
function polyMod(dividend: number[], divisor: number[]): number[] {
  let rem = stripLeadingZeros(dividend);
  while (rem.length >= divisor.length) {
    const ratio = glog(rem[0]) - glog(divisor[0]);
    const out = new Array<number>(rem.length);
    for (let i = 0; i < divisor.length; i++) {
      out[i] = rem[i] ^ gexp(glog(divisor[i]) + ratio);
    }
    for (let i = divisor.length; i < rem.length; i++) {
      out[i] = rem[i];
    }
    rem = stripLeadingZeros(out);
  }
  return rem;
}

function rsEcCodewords(data: number[], ecCount: number): number[] {
  const gen = rsGeneratorPoly(ecCount);
  const dividend = data.concat(new Array<number>(ecCount).fill(0));
  const rem = polyMod(dividend, gen);
  const out = new Array<number>(ecCount).fill(0);
  for (let i = 0; i < rem.length; i++) {
    out[ecCount - rem.length + i] = rem[i];
  }
  return out;
}

interface RsBlock {
  total: number;
  data: number;
}

function rsBlocks(version: number, ecLevel: number): RsBlock[] {
  const entry = RS_BLOCK_TABLE[version - 1][ecLevel];
  const blocks: RsBlock[] = [];
  for (let i = 0; i < entry.length; i += 3) {
    const count = entry[i];
    const total = entry[i + 1];
    const data = entry[i + 2];
    for (let k = 0; k < count; k++) {
      blocks.push({ total, data });
    }
  }
  return blocks;
}

// -- data encoding ----------------------------------------------------------

function charCountBits(version: number): number {
  return version < 10 ? 8 : 16;
}

function capacityBits(version: number, ecLevel: number): number {
  return rsBlocks(version, ecLevel).reduce((sum, b) => sum + b.data, 0) * 8;
}

function chooseVersion(data: number[], ecLevel: number): number {
  let version = 1;
  for (;;) {
    const needed = 4 + charCountBits(version) + data.length * 8;
    let v = version;
    while (v <= 40 && capacityBits(v, ecLevel) < needed) v++;
    if (v > 40) throw new Error('qr: data too long');
    if (charCountBits(v) === charCountBits(version)) return v;
    version = v;
  }
}

function createBytes(codewords: number[], blocks: RsBlock[]): number[] {
  let offset = 0;
  let maxDc = 0;
  let maxEc = 0;
  const dcdata: number[][] = [];
  const ecdata: number[][] = [];

  for (const b of blocks) {
    const dc = codewords.slice(offset, offset + b.data);
    offset += b.data;
    const ec = rsEcCodewords(dc, b.total - b.data);
    maxDc = Math.max(maxDc, dc.length);
    maxEc = Math.max(maxEc, ec.length);
    dcdata.push(dc);
    ecdata.push(ec);
  }

  const out: number[] = [];
  for (let i = 0; i < maxDc; i++) {
    for (const dc of dcdata) if (i < dc.length) out.push(dc[i]);
  }
  for (let i = 0; i < maxEc; i++) {
    for (const ec of ecdata) if (i < ec.length) out.push(ec[i]);
  }
  return out;
}

function encodeData(data: number[], version: number, ecLevel: number): number[] {
  const blocks = rsBlocks(version, ecLevel);
  const bitLimit = blocks.reduce((sum, b) => sum + b.data, 0) * 8;

  const bits: boolean[] = [];
  const put = (n: number, length: number): void => {
    for (let i = length - 1; i >= 0; i--) bits.push(((n >> i) & 1) === 1);
  };

  put(0b0100, 4); // byte mode
  put(data.length, charCountBits(version));
  for (const b of data) put(b, 8);

  // terminator, then pad to a byte boundary, then alternate pad bytes.
  for (let i = 0; i < Math.min(bitLimit - bits.length, 4); i++) bits.push(false);
  while (bits.length % 8 !== 0) bits.push(false);
  for (let i = 0; bits.length < bitLimit; i++) put(i % 2 === 0 ? PAD0 : PAD1, 8);

  const codewords: number[] = [];
  for (let i = 0; i < bits.length; i += 8) {
    let b = 0;
    for (let j = 0; j < 8; j++) b = (b << 1) | (bits[i + j] ? 1 : 0);
    codewords.push(b);
  }
  return createBytes(codewords, blocks);
}

// -- module placement -------------------------------------------------------

type Cell = boolean | null;

function setupFinder(mods: Cell[][], row: number, col: number): void {
  const n = mods.length;
  for (let r = -1; r <= 7; r++) {
    if (row + r <= -1 || n <= row + r) continue;
    for (let c = -1; c <= 7; c++) {
      if (col + c <= -1 || n <= col + c) continue;
      const dark =
        (r >= 0 && r <= 6 && (c === 0 || c === 6)) ||
        (c >= 0 && c <= 6 && (r === 0 || r === 6)) ||
        (r >= 2 && r <= 4 && c >= 2 && c <= 4);
      mods[row + r][col + c] = dark;
    }
  }
}

function setupAlignment(mods: Cell[][], version: number): void {
  const pos = PATTERN_POSITION_TABLE[version - 1];
  for (const row of pos) {
    for (const col of pos) {
      if (mods[row][col] !== null) continue;
      for (let r = -2; r <= 2; r++) {
        for (let c = -2; c <= 2; c++) {
          mods[row + r][col + c] =
            r === -2 || r === 2 || c === -2 || c === 2 || (r === 0 && c === 0);
        }
      }
    }
  }
}

function setupTiming(mods: Cell[][]): void {
  const n = mods.length;
  for (let r = 8; r < n - 8; r++) {
    if (mods[r][6] !== null) continue;
    mods[r][6] = r % 2 === 0;
  }
  for (let c = 8; c < n - 8; c++) {
    if (mods[6][c] !== null) continue;
    mods[6][c] = c % 2 === 0;
  }
}

function setupTypeInfo(mods: Cell[][], ecLevel: number, mask: number, test: boolean): void {
  const n = mods.length;
  const bits = bchTypeInfo((EC_BITS[ecLevel] << 3) | mask);

  for (let i = 0; i < 15; i++) {
    const mod = !test && ((bits >> i) & 1) === 1;
    if (i < 6) mods[i][8] = mod;
    else if (i < 8) mods[i + 1][8] = mod;
    else mods[n - 15 + i][8] = mod;
  }
  for (let i = 0; i < 15; i++) {
    const mod = !test && ((bits >> i) & 1) === 1;
    if (i < 8) mods[8][n - i - 1] = mod;
    else if (i < 9) mods[8][15 - i - 1 + 1] = mod;
    else mods[8][15 - i - 1] = mod;
  }

  // The fixed dark module.
  mods[n - 8][8] = !test;
}

function setupTypeNumber(mods: Cell[][], version: number, test: boolean): void {
  const n = mods.length;
  const bits = bchTypeNumber(version);
  for (let i = 0; i < 18; i++) {
    mods[Math.floor(i / 3)][(i % 3) + n - 8 - 3] = !test && ((bits >> i) & 1) === 1;
  }
  for (let i = 0; i < 18; i++) {
    mods[(i % 3) + n - 8 - 3][Math.floor(i / 3)] = !test && ((bits >> i) & 1) === 1;
  }
}

function maskFunc(pattern: number): (row: number, col: number) => boolean {
  switch (pattern) {
    case 0: return (i, j) => (i + j) % 2 === 0;
    case 1: return (i) => i % 2 === 0;
    case 2: return (_i, j) => j % 3 === 0;
    case 3: return (i, j) => (i + j) % 3 === 0;
    case 4: return (i, j) => (Math.floor(i / 2) + Math.floor(j / 3)) % 2 === 0;
    case 5: return (i, j) => ((i * j) % 2) + ((i * j) % 3) === 0;
    case 6: return (i, j) => (((i * j) % 2) + ((i * j) % 3)) % 2 === 0;
    case 7: return (i, j) => (((i * j) % 3) + ((i + j) % 2)) % 2 === 0;
    default: throw new Error(`qr: bad mask ${pattern}`);
  }
}

function mapData(mods: Cell[][], data: number[], mask: number): void {
  const n = mods.length;
  const maskFn = maskFunc(mask);
  let inc = -1;
  let row = n - 1;
  let bitIndex = 7;
  let byteIndex = 0;

  for (let col = n - 1; col > 0; col -= 2) {
    const right = col <= 6 ? col - 1 : col;
    const colRange = [right, right - 1];
    for (;;) {
      for (const c of colRange) {
        if (mods[row][c] === null) {
          let dark = false;
          if (byteIndex < data.length) {
            dark = ((data[byteIndex] >> bitIndex) & 1) === 1;
          }
          if (maskFn(row, c)) dark = !dark;
          mods[row][c] = dark;
          bitIndex -= 1;
          if (bitIndex === -1) {
            byteIndex += 1;
            bitIndex = 7;
          }
        }
      }
      row += inc;
      if (row < 0 || n <= row) {
        row -= inc;
        inc = -inc;
        break;
      }
    }
  }
}

// -- penalty scoring --------------------------------------------------------

function lostPointLevel1(mods: QrMatrix): number {
  const n = mods.length;
  let pts = 0;
  const scan = (get: (i: number) => boolean[]): void => {
    for (let i = 0; i < n; i++) {
      const line = get(i);
      let prev = line[0];
      let len = 0;
      for (let j = 0; j < n; j++) {
        if (line[j] === prev) {
          len++;
        } else {
          if (len >= 5) pts += len - 2;
          len = 1;
          prev = line[j];
        }
      }
      if (len >= 5) pts += len - 2;
    }
  };
  scan((r) => mods[r]);
  scan((c) => {
    const col: boolean[] = [];
    for (let r = 0; r < n; r++) col.push(mods[r][c]);
    return col;
  });
  return pts;
}

function lostPointLevel2(mods: QrMatrix): number {
  const n = mods.length;
  let pts = 0;
  for (let r = 0; r < n - 1; r++) {
    for (let c = 0; c < n - 1; c++) {
      const a = mods[r][c];
      if (a === mods[r][c + 1] && a === mods[r + 1][c] && a === mods[r + 1][c + 1]) {
        pts += 3;
      }
    }
  }
  return pts;
}

function lostPointLevel3(mods: QrMatrix): number {
  const n = mods.length;
  let pts = 0;
  const scan = (get: (i: number) => boolean[]): void => {
    for (let i = 0; i < n; i++) {
      const line = get(i);
      for (let c = 0; c + 11 <= n; c++) {
        const p1 =
          !line[c + 1] && line[c + 4] && !line[c + 5] && line[c + 6] && !line[c + 9] &&
          ((line[c] && line[c + 2] && line[c + 3] && !line[c + 7] && !line[c + 8] && !line[c + 10]) ||
            (!line[c] && !line[c + 2] && !line[c + 3] && line[c + 7] && line[c + 8] && line[c + 10]));
        if (p1) pts += 40;
      }
    }
  };
  scan((r) => mods[r]);
  scan((c) => {
    const col: boolean[] = [];
    for (let r = 0; r < n; r++) col.push(mods[r][c]);
    return col;
  });
  return pts;
}

function lostPointLevel4(mods: QrMatrix): number {
  const n = mods.length;
  let dark = 0;
  for (const row of mods) for (const m of row) if (m) dark++;
  const rating = Math.floor(Math.abs((dark / (n * n)) * 100 - 50) / 5);
  return rating * 10;
}

function lostPoint(mods: QrMatrix): number {
  return (
    lostPointLevel1(mods) +
    lostPointLevel2(mods) +
    lostPointLevel3(mods) +
    lostPointLevel4(mods)
  );
}

// -- top level --------------------------------------------------------------

function buildMatrix(
  version: number,
  ecLevel: number,
  codewords: number[],
  mask: number,
  test: boolean,
): QrMatrix {
  const n = 17 + 4 * version;
  const mods: Cell[][] = Array.from({ length: n }, () => new Array<Cell>(n).fill(null));

  setupFinder(mods, 0, 0);
  setupFinder(mods, n - 7, 0);
  setupFinder(mods, 0, n - 7);
  setupAlignment(mods, version);
  setupTiming(mods);
  setupTypeInfo(mods, ecLevel, mask, test);
  if (version >= 7) setupTypeNumber(mods, version, test);
  mapData(mods, codewords, mask);

  return mods.map((row) => row.map((m) => m === true));
}

/**
 * Encodes text as a QR code at error correction level M, choosing the smallest
 * version that fits and the mask with the lowest penalty — the same choices
 * Python's `qrcode` package makes, so the matrix is byte-for-byte comparable.
 */
export function encodeQr(text: string): QrCode {
  const data = Array.from(Buffer.from(text, 'utf8'));
  const ecLevel = 1; // M
  const version = chooseVersion(data, ecLevel);
  const codewords = encodeData(data, version, ecLevel);

  let bestMask = 0;
  let bestPenalty = Number.POSITIVE_INFINITY;
  for (let mask = 0; mask < 8; mask++) {
    const mods = buildMatrix(version, ecLevel, codewords, mask, true);
    const penalty = lostPoint(mods);
    if (mask === 0 || penalty < bestPenalty) {
      bestPenalty = penalty;
      bestMask = mask;
    }
  }

  return {
    version,
    size: 17 + 4 * version,
    modules: buildMatrix(version, ecLevel, codewords, bestMask, false),
  };
}

/**
 * Renders a module matrix as a black-on-white SVG with a four-module quiet
 * zone. Static markup only, so it works in a webview with scripts disabled.
 */
export function renderQrSvg(modules: QrMatrix, scale = 4): string {
  const n = modules.length;
  const border = 4;
  const size = n + border * 2;
  const px = size * scale;

  let rects = '';
  for (let r = 0; r < n; r++) {
    for (let c = 0; c < n; c++) {
      if (modules[r][c]) {
        const x = (c + border) * scale;
        const y = (r + border) * scale;
        rects += `<rect x="${x}" y="${y}" width="${scale}" height="${scale}"/>`;
      }
    }
  }

  return (
    `<svg xmlns="http://www.w3.org/2000/svg" width="${px}" height="${px}" ` +
    `viewBox="0 0 ${px} ${px}" shape-rendering="crispEdges">` +
    `<rect width="${px}" height="${px}" fill="#ffffff"/>` +
    `<g fill="#000000">${rects}</g></svg>`
  );
}
