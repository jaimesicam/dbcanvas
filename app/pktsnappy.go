package main

// pktsnappy.go — Snappy raw-block decompression, because a MongoDB capture is mostly
// snappy and a decoder that cannot read it is a decoder that cannot read MongoDB.
//
// The first capture taken against the testbed's replica set came back with almost every
// message reading "OP_MSG compressed with snappy … (not decoded)". Percona Server for
// MongoDB negotiates snappy by default for both internal and driver connections, so this
// is not an edge case: without it, the feature would describe MongoDB traffic rather than
// decode it.
//
// The alternative was a dependency (github.com/golang/snappy). The format did not justify
// one: a raw snappy block is a varint length followed by a sequence of two element kinds,
// literals and back-references, and the whole decoder is the function below. zstd is a
// different matter — Huffman plus FSE plus a dictionary format — and stays described
// rather than decoded, the same honest line Galera's SST stream gets.
//
// This is the raw block format (the one the MongoDB wire protocol uses), not the
// framed stream format with its CRC-32C chunks.

import "fmt"

// snappyMaxOut bounds an allocation from a declared length. A corrupted or misframed
// length must not turn into a memory problem: the caller already knows the size the
// wire claimed, and a mismatch means the bytes are not what they say they are.
const snappyMaxOut = 128 << 20

// snappyDecode decompresses a raw snappy block. want is the size the caller expects
// (from OP_COMPRESSED's uncompressedSize field); it is checked against the block's own
// preamble, because two independent statements of the same length disagreeing is the
// cheapest possible corruption check.
func snappyDecode(src []byte, want int) ([]byte, error) {
	n, hdr, err := snappyLength(src)
	if err != nil {
		return nil, err
	}
	if n > snappyMaxOut {
		return nil, fmt.Errorf("snappy: declared length %d is implausible", n)
	}
	if want >= 0 && n != want {
		return nil, fmt.Errorf("snappy: block says %d bytes, the message header says %d", n, want)
	}
	dst := make([]byte, 0, n)
	src = src[hdr:]
	for len(src) > 0 {
		tag := src[0]
		switch tag & 0x03 {
		case 0x00: // literal
			length := int(tag >> 2)
			var lenBytes int
			switch {
			case length < 60:
				lenBytes = 0
				length++
			case length == 60:
				lenBytes = 1
			case length == 61:
				lenBytes = 2
			case length == 62:
				lenBytes = 3
			default: // 63
				lenBytes = 4
			}
			if lenBytes > 0 {
				if len(src) < 1+lenBytes {
					return nil, fmt.Errorf("snappy: truncated literal length")
				}
				v := 0
				for i := 0; i < lenBytes; i++ {
					v |= int(src[1+i]) << (8 * i)
				}
				length = v + 1
			}
			off := 1 + lenBytes
			if length < 0 || off+length > len(src) {
				return nil, fmt.Errorf("snappy: literal of %d bytes runs past the block", length)
			}
			dst = append(dst, src[off:off+length]...)
			src = src[off+length:]

		case 0x01: // copy, 1-byte offset, 3-bit length
			if len(src) < 2 {
				return nil, fmt.Errorf("snappy: truncated copy-1")
			}
			length := 4 + int(tag>>2&0x07)
			offset := int(tag>>5&0x07)<<8 | int(src[1])
			var e error
			if dst, e = snappyCopy(dst, offset, length); e != nil {
				return nil, e
			}
			src = src[2:]

		case 0x02: // copy, 2-byte offset
			if len(src) < 3 {
				return nil, fmt.Errorf("snappy: truncated copy-2")
			}
			length := 1 + int(tag>>2)
			offset := int(src[1]) | int(src[2])<<8
			var e error
			if dst, e = snappyCopy(dst, offset, length); e != nil {
				return nil, e
			}
			src = src[3:]

		default: // 0x03 — copy, 4-byte offset
			if len(src) < 5 {
				return nil, fmt.Errorf("snappy: truncated copy-4")
			}
			length := 1 + int(tag>>2)
			offset := int(src[1]) | int(src[2])<<8 | int(src[3])<<16 | int(src[4])<<24
			var e error
			if dst, e = snappyCopy(dst, offset, length); e != nil {
				return nil, e
			}
			src = src[5:]
		}
		if len(dst) > n {
			return nil, fmt.Errorf("snappy: output overran its declared length")
		}
	}
	if len(dst) != n {
		return nil, fmt.Errorf("snappy: decoded %d bytes, expected %d", len(dst), n)
	}
	return dst, nil
}

// snappyLength reads the block's varint preamble and returns the length and its size.
func snappyLength(src []byte) (int, int, error) {
	v, shift := 0, 0
	for i := 0; i < len(src) && i < 5; i++ {
		b := src[i]
		v |= int(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, fmt.Errorf("snappy: no length preamble")
}

// snappyCopy appends a back-reference. Overlapping copies are legal and common — they
// are how snappy encodes a run — so this copies byte by byte rather than with copy(),
// which would read the source before the destination was written.
func snappyCopy(dst []byte, offset, length int) ([]byte, error) {
	if offset <= 0 || offset > len(dst) {
		return nil, fmt.Errorf("snappy: copy offset %d is outside the %d bytes decoded so far", offset, len(dst))
	}
	if length < 0 {
		return nil, fmt.Errorf("snappy: negative copy length")
	}
	start := len(dst) - offset
	for i := 0; i < length; i++ {
		dst = append(dst, dst[start+i])
	}
	return dst, nil
}
