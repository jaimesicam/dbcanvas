package embed

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// tensor is one float32 tensor out of a safetensors file, kept in the file's
// own row-major layout. For a PyTorch nn.Linear that layout is [out][in], which
// is exactly what the matmul wants: each output feature is one contiguous dot
// product against a weight row, so no transpose is needed at load time.
type tensor struct {
	shape []int
	data  []float32
}

// safetensorsEntry is one value of the JSON header. data_offsets are relative to
// the first byte after the header, [begin, end).
type safetensorsEntry struct {
	DType       string   `json:"dtype"`
	Shape       []int    `json:"shape"`
	DataOffsets [2]int64 `json:"data_offsets"`
}

// readSafetensors parses the whole file into memory. The format is deliberately
// trivial — an 8-byte little-endian header length, a JSON header naming every
// tensor's dtype/shape/byte range, then the raw little-endian bytes — which is
// why a standard-library-only loader is a few dozen lines rather than a
// dependency. Only F32 tensors are decoded; anything else (this checkpoint
// carries an I64 embeddings.position_ids buffer we recompute anyway) is skipped.
func readSafetensors(path string) (map[string]tensor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("%s: too short for a safetensors file", path)
	}
	hlen := binary.LittleEndian.Uint64(raw[:8])
	if hlen > uint64(len(raw)-8) {
		return nil, fmt.Errorf("%s: header length %d exceeds file size", path, hlen)
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+hlen], &header); err != nil {
		return nil, fmt.Errorf("%s: header: %w", path, err)
	}
	body := raw[8+hlen:]
	out := make(map[string]tensor, len(header))
	for name, msg := range header {
		if name == "__metadata__" {
			continue
		}
		var e safetensorsEntry
		if err := json.Unmarshal(msg, &e); err != nil {
			return nil, fmt.Errorf("%s: tensor %q: %w", path, name, err)
		}
		if e.DType != "F32" {
			continue
		}
		begin, end := e.DataOffsets[0], e.DataOffsets[1]
		if begin < 0 || end < begin || end > int64(len(body)) || (end-begin)%4 != 0 {
			return nil, fmt.Errorf("%s: tensor %q: bad data_offsets %v", path, name, e.DataOffsets)
		}
		n := 1
		for _, d := range e.Shape {
			n *= d
		}
		if int64(n*4) != end-begin {
			return nil, fmt.Errorf("%s: tensor %q: shape %v does not match %d bytes", path, name, e.Shape, end-begin)
		}
		b := body[begin:end]
		data := make([]float32, n)
		for i := range data {
			data[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
		}
		out[name] = tensor{shape: e.Shape, data: data}
	}
	return out, nil
}
