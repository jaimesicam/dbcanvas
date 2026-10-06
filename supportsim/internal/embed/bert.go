package embed

import (
	"fmt"
	"math"
	"sync"
)

// all-MiniLM-L6-v2's shape. These are fixed rather than read from config.json
// because the package implements exactly one model; Load checks every weight
// tensor against them so a different checkpoint fails loudly instead of
// producing plausible-looking garbage.
const (
	hidden       = 384
	heads        = 12
	headDim      = hidden / heads // 32
	intermediate = 1536
	layers       = 6
	vocabSize    = 30522
	maxPositions = 512
	maxSeqLen    = 256 // sentence-transformers' max_seq_length for this model
	lnEps        = 1e-12
)

// linear is one nn.Linear: w is [out][in] row-major (PyTorch's own layout), so
// output feature o for token i is dot(x[i], w[o]) + b[o] — both operands
// contiguous, which is what keeps the inner loop cache- and prefetch-friendly.
type linear struct {
	w       []float32
	b       []float32
	in, out int
}

type layerNorm struct {
	gamma, beta []float32
}

type encoderLayer struct {
	q, k, v, attnOut linear
	attnLN           layerNorm
	inter, out       linear
	outLN            layerNorm
}

type bert struct {
	wordEmb, posEmb, typeEmb []float32
	embLN                    layerNorm
	layers                   [layers]encoderLayer
}

// newBERT pulls the named tensors out of the checkpoint. sentence-transformers
// saves the bare BertModel, so names have no "bert." prefix; a checkpoint saved
// from BertFor* does, and we accept either.
func newBERT(ts map[string]tensor) (*bert, error) {
	prefix := ""
	if _, ok := ts["embeddings.word_embeddings.weight"]; !ok {
		if _, ok := ts["bert.embeddings.word_embeddings.weight"]; ok {
			prefix = "bert."
		}
	}
	var firstErr error
	get := func(name string, shape ...int) []float32 {
		t, ok := ts[prefix+name]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("missing tensor %s%s", prefix, name)
			}
			return nil
		}
		if fmt.Sprint(t.shape) != fmt.Sprint(shape) {
			if firstErr == nil {
				firstErr = fmt.Errorf("tensor %s%s: shape %v, want %v", prefix, name, t.shape, shape)
			}
			return nil
		}
		return t.data
	}
	lin := func(name string, out, in int) linear {
		return linear{w: get(name+".weight", out, in), b: get(name+".bias", out), in: in, out: out}
	}
	ln := func(name string) layerNorm {
		return layerNorm{gamma: get(name+".weight", hidden), beta: get(name+".bias", hidden)}
	}

	m := &bert{
		wordEmb: get("embeddings.word_embeddings.weight", vocabSize, hidden),
		posEmb:  get("embeddings.position_embeddings.weight", maxPositions, hidden),
		typeEmb: get("embeddings.token_type_embeddings.weight", 2, hidden),
		embLN:   ln("embeddings.LayerNorm"),
	}
	for l := range m.layers {
		p := fmt.Sprintf("encoder.layer.%d.", l)
		m.layers[l] = encoderLayer{
			q:       lin(p+"attention.self.query", hidden, hidden),
			k:       lin(p+"attention.self.key", hidden, hidden),
			v:       lin(p+"attention.self.value", hidden, hidden),
			attnOut: lin(p+"attention.output.dense", hidden, hidden),
			attnLN:  ln(p + "attention.output.LayerNorm"),
			inter:   lin(p+"intermediate.dense", intermediate, hidden),
			out:     lin(p+"output.dense", hidden, intermediate),
			outLN:   ln(p + "output.LayerNorm"),
		}
	}
	return m, firstErr
}

// workspace holds every activation buffer for one forward pass, sized for the
// longest sequence. Forward passes borrow one from a sync.Pool, so the model
// itself stays read-only (safe for concurrent use) without allocating ~2 MB of
// scratch per sentence.
type workspace struct {
	x, q, k, v, ctx, tmp []float32
	inter                []float32
	scores               []float32
}

var wsPool = sync.Pool{New: func() any {
	return &workspace{
		x:      make([]float32, maxSeqLen*hidden),
		q:      make([]float32, maxSeqLen*hidden),
		k:      make([]float32, maxSeqLen*hidden),
		v:      make([]float32, maxSeqLen*hidden),
		ctx:    make([]float32, maxSeqLen*hidden),
		tmp:    make([]float32, maxSeqLen*hidden),
		inter:  make([]float32, maxSeqLen*intermediate),
		scores: make([]float32, heads*maxSeqLen*maxSeqLen),
	}
}}

// forward runs the encoder over one unpadded sequence and returns the mean-pooled,
// L2-normalised sentence embedding. workers is how many goroutines each matmul
// may fan out to: NumCPU for a single Embed call (latency), 1 inside EmbedBatch
// (which already has one goroutine per CPU working on different sentences, and
// nesting would only add scheduling overhead).
func (m *bert) forward(ids []int, workers int) []float32 {
	n := len(ids)
	ws := wsPool.Get().(*workspace)
	defer wsPool.Put(ws)
	x := ws.x[:n*hidden]

	// Embeddings: word + position + token_type(0). Position ids are simply 0..n-1
	// and the single-sentence segment id is always 0, so neither needs an input.
	for i, id := range ids {
		row := x[i*hidden : (i+1)*hidden]
		we := m.wordEmb[id*hidden : (id+1)*hidden]
		pe := m.posEmb[i*hidden : (i+1)*hidden]
		te := m.typeEmb[:hidden]
		for j := range row {
			row[j] = we[j] + pe[j] + te[j]
		}
	}
	m.embLN.apply(x, nil, n)

	for l := range m.layers {
		L := &m.layers[l]
		q, k, v := ws.q[:n*hidden], ws.k[:n*hidden], ws.v[:n*hidden]
		L.q.forward(q, x, n, workers)
		L.k.forward(k, x, n, workers)
		L.v.forward(v, x, n, workers)
		ctx := ws.ctx[:n*hidden]
		attention(ctx, q, k, v, ws.scores, n, workers)

		// Attention output projection, then residual + LayerNorm (post-LN, as in
		// the original BERT — the norm comes after the residual add).
		tmp := ws.tmp[:n*hidden]
		L.attnOut.forward(tmp, ctx, n, workers)
		L.attnLN.apply(x, tmp, n)

		// Feed-forward: 384 → 1536 with GELU → 384, residual + LayerNorm.
		inter := ws.inter[:n*intermediate]
		L.inter.forward(inter, x, n, workers)
		gelu(inter)
		L.out.forward(tmp, inter, n, workers)
		L.outLN.apply(x, tmp, n)
	}

	// Mean pooling over every token — [CLS] and [SEP] included, exactly as
	// sentence-transformers' Pooling(mode=mean) does with an all-ones attention
	// mask — then L2 normalisation, so cosine similarity is just a dot product.
	out := make([]float32, hidden)
	acc := make([]float64, hidden)
	for i := 0; i < n; i++ {
		row := x[i*hidden : (i+1)*hidden]
		for j, val := range row {
			acc[j] += float64(val)
		}
	}
	var norm float64
	for j := range acc {
		acc[j] /= float64(n)
		norm += acc[j] * acc[j]
	}
	norm = math.Sqrt(norm)
	if norm < 1e-12 {
		norm = 1e-12 // torch.nn.functional.normalize's eps
	}
	for j := range out {
		out[j] = float32(acc[j] / norm)
	}
	return out
}

// forward computes dst[n][out] = src[n][in] · wᵀ + b. Work is split by output
// feature across goroutines: each worker streams its slice of weight rows once
// and reuses each against all n token rows, which (for the 384-wide inputs of
// a short sentence, ~23 KB) stay hot in L1/L2.
func (l *linear) forward(dst, src []float32, n, workers int) {
	parallelFor(l.out/4, workers, func(lo, hi int) {
		in := l.in
		for o := lo * 4; o < hi*4; o += 4 {
			w0 := l.w[(o+0)*in : (o+1)*in]
			w1 := l.w[(o+1)*in : (o+2)*in]
			w2 := l.w[(o+2)*in : (o+3)*in]
			w3 := l.w[(o+3)*in : (o+4)*in]
			b0, b1, b2, b3 := l.b[o], l.b[o+1], l.b[o+2], l.b[o+3]
			i := 0
			for ; i+1 < n; i += 2 {
				r := dot4x2(src[i*in:(i+1)*in], src[(i+1)*in:(i+2)*in], w0, w1, w2, w3)
				d := dst[i*l.out+o : i*l.out+o+4]
				d[0], d[1], d[2], d[3] = r[0]+b0, r[1]+b1, r[2]+b2, r[3]+b3
				d = dst[(i+1)*l.out+o : (i+1)*l.out+o+4]
				d[0], d[1], d[2], d[3] = r[4]+b0, r[5]+b1, r[6]+b2, r[7]+b3
			}
			if i < n {
				s0, s1, s2, s3 := dot4(src[i*in:(i+1)*in], w0, w1, w2, w3)
				d := dst[i*l.out+o : i*l.out+o+4]
				d[0], d[1], d[2], d[3] = s0+b0, s1+b1, s2+b2, s3+b3
			}
		}
	})
}

// dot4x2 is a 2-token × 4-row register tile: eight dot products per pass over
// the inner dimension. Each step does 6 loads for 8 multiply-adds (dot4 does 5
// for 4), which is what matters once the token rows no longer fit in L1 (~10%
// faster end to end on a full 256-token sequence; neutral on short ones).
func dot4x2(x, y, w0, w1, w2, w3 []float32) [8]float32 {
	n := len(x)
	y, w0, w1, w2, w3 = y[:n], w0[:n], w1[:n], w2[:n], w3[:n] // bounds-check elimination
	var a0, a1, a2, a3, c0, c1, c2, c3 float32
	for j := 0; j < n; j++ {
		xj, yj := x[j], y[j]
		v0, v1, v2, v3 := w0[j], w1[j], w2[j], w3[j]
		a0 += xj * v0
		a1 += xj * v1
		a2 += xj * v2
		a3 += xj * v3
		c0 += yj * v0
		c1 += yj * v1
		c2 += yj * v2
		c3 += yj * v3
	}
	return [8]float32{a0, a1, a2, a3, c0, c1, c2, c3}
}

// dot4 computes four dot products of x against four weight rows at once. Loading
// x once for four rows halves the memory traffic of four separate dots, and the
// eight independent accumulators (4 rows × 2-way unroll) hide floating-point add
// latency — Go doesn't auto-vectorise, so this ILP is most of the speed there is.
func dot4(x, w0, w1, w2, w3 []float32) (float32, float32, float32, float32) {
	n := len(x)
	w0, w1, w2, w3 = w0[:n], w1[:n], w2[:n], w3[:n] // bounds-check elimination
	var a0, a1, a2, a3, c0, c1, c2, c3 float32
	j := 0
	for ; j+1 < n; j += 2 {
		x0, x1 := x[j], x[j+1]
		a0 += x0 * w0[j]
		a1 += x0 * w1[j]
		a2 += x0 * w2[j]
		a3 += x0 * w3[j]
		c0 += x1 * w0[j+1]
		c1 += x1 * w1[j+1]
		c2 += x1 * w2[j+1]
		c3 += x1 * w3[j+1]
	}
	for ; j < n; j++ {
		a0 += x[j] * w0[j]
		a1 += x[j] * w1[j]
		a2 += x[j] * w2[j]
		a3 += x[j] * w3[j]
	}
	return a0 + c0, a1 + c1, a2 + c2, a3 + c3
}

// dot is a single 8-way-unrolled dot product, used for attention scores.
func dot(a, b []float32) float32 {
	n := len(a)
	b = b[:n]
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	j := 0
	for ; j+7 < n; j += 8 {
		s0 += a[j] * b[j]
		s1 += a[j+1] * b[j+1]
		s2 += a[j+2] * b[j+2]
		s3 += a[j+3] * b[j+3]
		s4 += a[j+4] * b[j+4]
		s5 += a[j+5] * b[j+5]
		s6 += a[j+6] * b[j+6]
		s7 += a[j+7] * b[j+7]
	}
	for ; j < n; j++ {
		s0 += a[j] * b[j]
	}
	return (s0 + s1) + (s2 + s3) + (s4 + s5) + (s6 + s7)
}

// attention is multi-head scaled dot-product self-attention. q/k/v are [n][384]
// with head h occupying columns h*32 … h*32+31, so no reshaping is needed — each
// head just reads its own column window. The attention mask is all ones (a
// single unpadded sequence), so every token attends to every token and no
// masking step exists. Heads are independent and run in parallel.
func attention(ctx, q, k, v, scores []float32, n, workers int) {
	scale := float32(1 / math.Sqrt(headDim))
	parallelFor(heads, workers, func(lo, hi int) {
		for h := lo; h < hi; h++ {
			off := h * headDim
			p := scores[h*maxSeqLen*maxSeqLen : h*maxSeqLen*maxSeqLen+n*n]
			for i := 0; i < n; i++ {
				qi := q[i*hidden+off : i*hidden+off+headDim]
				row := p[i*n : (i+1)*n]
				maxv := float32(math.Inf(-1))
				for j := 0; j < n; j++ {
					s := dot(qi, k[j*hidden+off:j*hidden+off+headDim]) * scale
					row[j] = s
					if s > maxv {
						maxv = s
					}
				}
				// Softmax, max-subtracted for numerical stability.
				var sum float32
				for j := range row {
					e := float32(math.Exp(float64(row[j] - maxv)))
					row[j] = e
					sum += e
				}
				inv := 1 / sum
				out := ctx[i*hidden+off : i*hidden+off+headDim]
				for d := range out {
					out[d] = 0
				}
				for j, pj := range row {
					pj *= inv
					vj := v[j*hidden+off : j*hidden+off+headDim]
					for d := range out {
						out[d] += pj * vj[d]
					}
				}
			}
		}
	})
}

// apply sets x = LayerNorm(x + residual) row by row (residual may be nil). Mean
// and variance are accumulated in float64: it costs nothing at width 384 and
// keeps us within float32 rounding of PyTorch's fused kernel.
func (ln *layerNorm) apply(x, residual []float32, n int) {
	for i := 0; i < n; i++ {
		row := x[i*hidden : (i+1)*hidden]
		if residual != nil {
			r := residual[i*hidden : (i+1)*hidden]
			for j := range row {
				row[j] += r[j]
			}
		}
		var mean float64
		for _, val := range row {
			mean += float64(val)
		}
		mean /= hidden
		var variance float64
		for _, val := range row {
			d := float64(val) - mean
			variance += d * d
		}
		variance /= hidden // biased (population) variance, as nn.LayerNorm uses
		inv := 1 / math.Sqrt(variance+lnEps)
		for j, val := range row {
			row[j] = float32((float64(val)-mean)*inv)*ln.gamma[j] + ln.beta[j]
		}
	}
}

// gelu is the exact erf form, x·Φ(x) = 0.5·x·(1+erf(x/√2)) — BERT's "gelu"
// activation (hidden_act="gelu" in its config). The tanh approximation is
// cheaper but is a different function, and the point of this package is to
// match the reference numerically, not approximately.
func gelu(x []float32) {
	for i, val := range x {
		v := float64(val)
		x[i] = float32(0.5 * v * (1 + math.Erf(v/math.Sqrt2)))
	}
}

// parallelFor splits [0, total) into up to workers contiguous chunks and runs fn
// on each in its own goroutine (the calling goroutine takes the last chunk).
// With workers <= 1 it is a plain function call — no goroutines at all.
func parallelFor(total, workers int, fn func(lo, hi int)) {
	if workers > total {
		workers = total
	}
	if workers <= 1 {
		fn(0, total)
		return
	}
	var wg sync.WaitGroup
	chunk := (total + workers - 1) / workers
	lo := 0
	for ; lo+chunk < total; lo += chunk {
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			fn(lo, hi)
		}(lo, lo+chunk)
	}
	fn(lo, total)
	wg.Wait()
}
