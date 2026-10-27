// md5.js — MD5 (RFC 1321) in plain JavaScript.
//
// WebCrypto has no MD5, and pt-query-digest names a query class by the MD5 of its fingerprint, so
// the Query digest card carries its own. Strings are hashed as their UTF-8 bytes, which is what
// Digest::MD5 sees when Perl reads a log file as bytes.

const S = [
  7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
  5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
  4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
  6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
]

// K[i] = floor(|sin(i + 1)| * 2^32), written out so no engine's sin() is involved.
const K = [
  0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee, 0xf57c0faf, 0x4787c62a, 0xa8304613, 0xfd469501,
  0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be, 0x6b901122, 0xfd987193, 0xa679438e, 0x49b40821,
  0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa, 0xd62f105d, 0x02441453, 0xd8a1e681, 0xe7d3fbc8,
  0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed, 0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a,
  0xfffa3942, 0x8771f681, 0x6d9d6122, 0xfde5380c, 0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70,
  0x289b7ec6, 0xeaa127fa, 0xd4ef3085, 0x04881d05, 0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665,
  0xf4292244, 0x432aff97, 0xab9423a7, 0xfc93a039, 0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
  0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1, 0xf7537e82, 0xbd3af235, 0x2ad7d2bb, 0xeb86d391,
]

// md5Hex returns the lowercase hex digest of a string (as UTF-8) or a Uint8Array.
export function md5Hex(input) {
  const bytes = typeof input === 'string' ? new TextEncoder().encode(input) : input
  const n = bytes.length
  const words = new Uint32Array((((n + 8) >>> 6) + 1) * 16)
  for (let i = 0; i < n; i++) words[i >> 2] |= bytes[i] << ((i % 4) * 8)
  words[n >> 2] |= 0x80 << ((n % 4) * 8)
  words[words.length - 2] = (n * 8) >>> 0
  words[words.length - 1] = Math.floor(n / 0x20000000)

  let a0 = 0x67452301, b0 = 0xefcdab89, c0 = 0x98badcfe, d0 = 0x10325476
  for (let off = 0; off < words.length; off += 16) {
    let a = a0, b = b0, c = c0, d = d0
    for (let i = 0; i < 64; i++) {
      let f, g
      if (i < 16) { f = (b & c) | (~b & d); g = i }
      else if (i < 32) { f = (d & b) | (~d & c); g = (5 * i + 1) % 16 }
      else if (i < 48) { f = b ^ c ^ d; g = (3 * i + 5) % 16 }
      else { f = c ^ (b | ~d); g = (7 * i) % 16 }
      const x = (a + f + K[i] + words[off + g]) | 0
      a = d; d = c; c = b
      b = (b + ((x << S[i]) | (x >>> (32 - S[i])))) | 0
    }
    a0 = (a0 + a) | 0; b0 = (b0 + b) | 0; c0 = (c0 + c) | 0; d0 = (d0 + d) | 0
  }
  let hex = ''
  for (const w of [a0, b0, c0, d0]) {
    for (let i = 0; i < 4; i++) hex += ((w >>> (i * 8)) & 0xff).toString(16).padStart(2, '0')
  }
  return hex
}
