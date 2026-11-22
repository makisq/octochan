package own

import (
	"math"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

// Обучение весов сети — градиентный спуск по токенам корпуса, на Go и на
// процессоре: прямой проход с памятью для обратного, обратный проход
// (эмбеддинги, RMSNorm, внимание с RoPE, MLP SwiGLU, связанный выход) и
// AdamW. Счёт тот же, что у torch в llama/train_own.py: градиенты и шаг
// сверены с ним на микромодели (testdata/train).
//
// Пара считается целиком за один проход: вход — токены без последнего,
// цели — токены без первого. Параллельность — внутри пары: позиции и строки
// матриц делятся между ядрами, а каждый элемент считает одно ядро в одном
// порядке, поэтому результат не зависит от числа ядер.

// weight — обучаемый тензор: значения, градиент и моменты AdamW.
type weight struct {
	name       string
	rows, cols int // вектор нормировки — одна строка
	w, g, m, v []float32
	decay      bool // убывание весов — только у матриц
}

type trainLayer struct {
	inNorm, postNorm *weight
	q, k, v, o       *weight
	gate, up, down   *weight
}

// Trainer — сеть в обучении: её веса — те же срезы, что у Net.
type Trainer struct {
	Config
	weights           []*weight
	embed, head, norm *weight
	layers            []trainLayer
	invFreq           []float64
	steps             int // сделано шагов AdamW
	tp                *tape
}

// AdamW, как в train_own.py.
const (
	adamBeta1   = 0.9
	adamBeta2   = 0.95
	adamEps     = 1e-8
	weightDecay = 0.1
	clipNorm    = 1.0
)

// NewTrainer — обучение сети n: веса меняются на месте.
func NewTrainer(n *Net) *Trainer {
	t := &Trainer{Config: n.Config, invFreq: n.invFreq}
	add := func(name string, rows, cols int, w []float32, decay bool) *weight {
		p := &weight{name: name, rows: rows, cols: cols, w: w, g: make([]float32, len(w)), m: make([]float32, len(w)),
			v: make([]float32, len(w)), decay: decay}
		t.weights = append(t.weights, p)
		return p
	}
	mat := func(name string, m matrix) *weight { return add(name, m.rows, m.cols, m.w, true) }
	vec := func(name string, w []float32) *weight { return add(name, 1, len(w), w, false) }
	t.embed = mat("model.embed_tokens.weight", n.embed)
	t.head = t.embed
	if &n.head.w[0] != &n.embed.w[0] {
		t.head = mat("lm_head.weight", n.head)
	}
	for i := range n.layers {
		l, p := &n.layers[i], "model.layers."+strconv.Itoa(i)+"."
		t.layers = append(t.layers, trainLayer{
			inNorm: vec(p+"input_layernorm.weight", l.inNorm), postNorm: vec(p+"post_attention_layernorm.weight", l.postNorm),
			q: mat(p+"self_attn.q_proj.weight", l.q), k: mat(p+"self_attn.k_proj.weight", l.k),
			v: mat(p+"self_attn.v_proj.weight", l.v), o: mat(p+"self_attn.o_proj.weight", l.o),
			gate: mat(p+"mlp.gate_proj.weight", l.gate), up: mat(p+"mlp.up_proj.weight", l.up),
			down: mat(p+"mlp.down_proj.weight", l.down)})
	}
	t.norm = vec("model.norm.weight", n.norm)
	return t
}

// tensors — веса для записи: имена Llama, как их читает и torch.
func (t *Trainer) tensors() map[string]*Tensor {
	out := map[string]*Tensor{}
	for _, p := range t.weights {
		shape := []int{p.rows, p.cols}
		if !p.decay {
			shape = []int{p.cols}
		}
		out[p.name] = &Tensor{Shape: shape, Data: p.w, DType: "F32"}
	}
	return out
}

// tape — рабочая память одной пары: что прямой проход оставляет обратному
// и куда обратный складывает градиенты по позициям.
type tape struct {
	n        int // вместимость, позиций
	T        int
	cos, sin []float64
	xs       [][]float32 // остаточный поток: вход слоя l; последний — выход сети
	lay      []layerTape
	f, rf    []float32 // выход финальной нормировки и её множители
	logits   []float32 // логиты, затем их градиент
	lossAt   []float64

	dx, dn, dq, dk, dv, dz []float32
	dm, dgate, dup         []float32
	ds                     []float32 // градиент оценок внимания: головы × T × T
}

type layerTape struct {
	n1, q, k, v, z, mid, n2 []float32
	r1, r2                  []float32
	gate, up, m             []float32
	alpha                   []float32 // веса внимания: головы × T × T
}

func (t *Trainer) tapeFor(T int) *tape {
	if t.tp != nil && t.tp.n >= T {
		t.tp.T = T
		return t.tp
	}
	h, half := t.HiddenSize, t.HeadDim/2
	qd, kd, im := t.Heads*t.HeadDim, t.KVHeads*t.HeadDim, t.Intermediate
	f32 := func(n int) []float32 { return make([]float32, n) }
	tp := &tape{n: T, T: T, cos: make([]float64, T*half), sin: make([]float64, T*half), f: f32(T * h), rf: f32(T),
		logits: f32(T * t.VocabSize), lossAt: make([]float64, T), dx: f32(T * h), dn: f32(T * h), dq: f32(T * qd),
		dk: f32(T * kd), dv: f32(T * kd), dz: f32(T * qd), dm: f32(T * im), dgate: f32(T * im), dup: f32(T * im),
		ds: f32(t.Heads * T * T)}
	for i := 0; i < T; i++ {
		for j, f := range t.invFreq {
			a := float64(i) * f
			tp.cos[i*half+j], tp.sin[i*half+j] = math.Cos(a), math.Sin(a)
		}
	}
	for l := 0; l <= len(t.layers); l++ {
		tp.xs = append(tp.xs, f32(T*h))
	}
	for range t.layers {
		tp.lay = append(tp.lay, layerTape{n1: f32(T * h), q: f32(T * qd), k: f32(T * kd), v: f32(T * kd), z: f32(T * qd),
			mid: f32(T * h), n2: f32(T * h), r1: f32(T), r2: f32(T), gate: f32(T * im), up: f32(T * im), m: f32(T * im),
			alpha: f32(t.Heads * T * T)})
	}
	t.tp = tp
	return tp
}

// forward — прямой проход входа ids: логиты каждой позиции в ленте.
func (t *Trainer) forward(ids []int) *tape {
	T := len(ids)
	tp := t.tapeFor(T)
	h, hd, nh, kvh, im := t.HiddenSize, t.HeadDim, t.Heads, t.KVHeads, t.Intermediate
	qd, kd, half, group := nh*hd, kvh*hd, hd/2, nh/kvh
	eps, scale := t.RMSNormEps, 1/math.Sqrt(float64(hd))
	for i, id := range ids {
		copy(tp.xs[0][i*h:(i+1)*h], t.embed.w[id*h:(id+1)*h])
	}
	for li := range t.layers {
		L, lt := &t.layers[li], &tp.lay[li]
		x, out := tp.xs[li], tp.xs[li+1]
		parallelFor(T, 4, func(from, to int) {
			for i := from; i < to; i++ {
				n1 := lt.n1[i*h : (i+1)*h]
				lt.r1[i] = rmsNormR(n1, x[i*h:(i+1)*h], L.inNorm.w, eps)
				q, k := lt.q[i*qd:(i+1)*qd], lt.k[i*kd:(i+1)*kd]
				L.q.mul(q, n1)
				L.k.mul(k, n1)
				L.v.mul(lt.v[i*kd:(i+1)*kd], n1)
				cos, sin := tp.cos[i*half:(i+1)*half], tp.sin[i*half:(i+1)*half]
				rope(q, nh, hd, cos, sin)
				rope(k, kvh, hd, cos, sin)
			}
		})
		parallelFor(T, 2, func(from, to int) {
			scores := make([]float64, T)
			tmp := make([]float32, h)
			for i := from; i < to; i++ {
				z := lt.z[i*qd : (i+1)*qd]
				clear(z)
				for head := 0; head < nh; head++ {
					kvHead := head / group
					qh := lt.q[i*qd+head*hd : i*qd+(head+1)*hd]
					top := math.Inf(-1)
					for p := 0; p <= i; p++ {
						kp := lt.k[p*kd+kvHead*hd : p*kd+(kvHead+1)*hd]
						var sum float64
						for j := range qh {
							sum += float64(qh[j]) * float64(kp[j])
						}
						sum *= scale
						scores[p] = sum
						top = math.Max(top, sum)
					}
					var total float64
					for p := 0; p <= i; p++ {
						scores[p] = math.Exp(scores[p] - top)
						total += scores[p]
					}
					alpha := lt.alpha[(head*T+i)*T : (head*T+i)*T+i+1]
					zh := z[head*hd : (head+1)*hd]
					for p := range alpha {
						w := float32(scores[p] / total)
						alpha[p] = w
						axpy(zh, w, lt.v[p*kd+kvHead*hd:p*kd+(kvHead+1)*hd])
					}
				}
				mid := lt.mid[i*h : (i+1)*h]
				L.o.mul(tmp, z)
				xi := x[i*h : (i+1)*h]
				for j := range mid {
					mid[j] = xi[j] + tmp[j]
				}
				n2 := lt.n2[i*h : (i+1)*h]
				lt.r2[i] = rmsNormR(n2, mid, L.postNorm.w, eps)
				gate, up, m := lt.gate[i*im:(i+1)*im], lt.up[i*im:(i+1)*im], lt.m[i*im:(i+1)*im]
				L.gate.mul(gate, n2)
				L.up.mul(up, n2)
				for j := range m {
					g := float64(gate[j])
					m[j] = float32(g/(1+math.Exp(-g))) * up[j]
				}
				L.down.mul(tmp, m)
				o := out[i*h : (i+1)*h]
				for j := range o {
					o[j] = mid[j] + tmp[j]
				}
			}
		})
	}
	last, V := tp.xs[len(t.layers)], t.VocabSize
	parallelFor(T, 2, func(from, to int) {
		for i := from; i < to; i++ {
			f := tp.f[i*h : (i+1)*h]
			tp.rf[i] = rmsNormR(f, last[i*h:(i+1)*h], t.norm.w, eps)
			t.head.mul(tp.logits[i*V:(i+1)*V], f)
		}
	})
	return tp
}

// pairLoss — прямой проход пары и её потери: сумма по всем целям, сумма по
// целям ответа (с позиции start) и их число. С grad в ленте остаётся
// градиент логитов с множителем scale — доля пары в среднем по батчу.
func (t *Trainer) pairLoss(ids []int, start int, grad bool, scale float64) (all, answer float64, answers int) {
	targets := ids[1:]
	tp := t.forward(ids[:len(ids)-1])
	V := t.VocabSize
	parallelFor(tp.T, 4, func(from, to int) {
		for i := from; i < to; i++ {
			row := tp.logits[i*V : (i+1)*V]
			top := math.Inf(-1)
			for _, l := range row {
				top = math.Max(top, float64(l))
			}
			var sum float64
			for _, l := range row {
				sum += math.Exp(float64(l) - top)
			}
			logZ := top + math.Log(sum)
			tp.lossAt[i] = logZ - float64(row[targets[i]])
			if grad {
				for v, l := range row {
					row[v] = float32(math.Exp(float64(l)-logZ) * scale)
				}
				row[targets[i]] -= float32(scale)
			}
		}
	})
	for i := 0; i < tp.T; i++ {
		all += tp.lossAt[i]
		if i+1 >= start {
			answer += tp.lossAt[i]
			answers++
		}
	}
	return all, answer, answers
}

// backward — обратный проход по ленте: градиент логитов → градиенты весов
// (прибавляются к накопленным за батч).
func (t *Trainer) backward(ids []int) {
	tp := t.tp
	T := tp.T
	h, hd, nh, kvh, im := t.HiddenSize, t.HeadDim, t.Heads, t.KVHeads, t.Intermediate
	qd, kd, half, group := nh*hd, kvh*hd, hd/2, nh/kvh
	scale := 1 / math.Sqrt(float64(hd))

	t.head.backW(tp.logits, tp.f, T)
	t.head.backX(tp.dn, tp.logits, T, false)
	rmsBack(tp.dx, tp.dn, tp.xs[len(t.layers)], t.norm, tp.rf, T, false)

	for li := len(t.layers) - 1; li >= 0; li-- {
		L, lt := &t.layers[li], &tp.lay[li]
		// MLP: out = mid + down(silu(gate)·up).
		L.down.backW(tp.dx, lt.m, T)
		L.down.backX(tp.dm, tp.dx, T, false)
		parallelFor(T, 16, func(from, to int) {
			for i := from * im; i < to*im; i++ {
				g := float64(lt.gate[i])
				sig := 1 / (1 + math.Exp(-g))
				d := float64(tp.dm[i])
				tp.dup[i] = float32(d * g * sig)
				tp.dgate[i] = float32(d * float64(lt.up[i]) * sig * (1 + g*(1-sig)))
			}
		})
		L.gate.backW(tp.dgate, lt.n2, T)
		L.up.backW(tp.dup, lt.n2, T)
		L.gate.backX(tp.dn, tp.dgate, T, false)
		L.up.backX(tp.dn, tp.dup, T, true)
		rmsBack(tp.dx, tp.dn, lt.mid, L.postNorm, lt.r2, T, true)

		// Внимание: mid = x + o(z), z — смесь значений по весам внимания.
		L.o.backW(tp.dx, lt.z, T)
		L.o.backX(tp.dz, tp.dx, T, false)
		clear(tp.dq[:T*qd])
		clear(tp.dk[:T*kd])
		clear(tp.dv[:T*kd])
		// По позициям запроса: градиент оценок и запроса.
		parallelFor(T, 2, func(from, to int) {
			dal := make([]float64, T)
			for i := from; i < to; i++ {
				for head := 0; head < nh; head++ {
					kvHead := head / group
					dzh := tp.dz[i*qd+head*hd : i*qd+(head+1)*hd]
					alpha := lt.alpha[(head*T+i)*T : (head*T+i)*T+i+1]
					ds := tp.ds[(head*T+i)*T : (head*T+i)*T+i+1]
					var mean float64
					for p := range alpha {
						vp := lt.v[p*kd+kvHead*hd : p*kd+(kvHead+1)*hd]
						var d float64
						for j := range dzh {
							d += float64(dzh[j]) * float64(vp[j])
						}
						dal[p] = d
						mean += float64(alpha[p]) * d
					}
					dqh := tp.dq[i*qd+head*hd : i*qd+(head+1)*hd]
					for p := range alpha {
						g := float32(float64(alpha[p]) * (dal[p] - mean) * scale)
						ds[p] = g
						axpy(dqh, g, lt.k[p*kd+kvHead*hd:p*kd+(kvHead+1)*hd])
					}
				}
			}
		})
		// По позициям ключа: всё, что смотрело на позицию, — в её ключ и значение.
		parallelFor(T, 2, func(from, to int) {
			for p := from; p < to; p++ {
				for head := 0; head < nh; head++ {
					kvHead := head / group
					dvp := tp.dv[p*kd+kvHead*hd : p*kd+(kvHead+1)*hd]
					dkp := tp.dk[p*kd+kvHead*hd : p*kd+(kvHead+1)*hd]
					for i := p; i < T; i++ {
						at := (head*T+i)*T + p
						axpy(dvp, lt.alpha[at], tp.dz[i*qd+head*hd:i*qd+(head+1)*hd])
						axpy(dkp, tp.ds[at], lt.q[i*qd+head*hd:i*qd+(head+1)*hd])
					}
				}
			}
		})
		parallelFor(T, 16, func(from, to int) {
			for i := from; i < to; i++ {
				cos, sin := tp.cos[i*half:(i+1)*half], tp.sin[i*half:(i+1)*half]
				ropeBack(tp.dq[i*qd:(i+1)*qd], nh, hd, cos, sin)
				ropeBack(tp.dk[i*kd:(i+1)*kd], kvh, hd, cos, sin)
			}
		})
		L.q.backW(tp.dq, lt.n1, T)
		L.k.backW(tp.dk, lt.n1, T)
		L.v.backW(tp.dv, lt.n1, T)
		L.q.backX(tp.dn, tp.dq, T, false)
		L.k.backX(tp.dn, tp.dk, T, true)
		L.v.backX(tp.dn, tp.dv, T, true)
		rmsBack(tp.dx, tp.dn, tp.xs[li], L.inNorm, lt.r1, T, true)
	}
	for i, id := range ids {
		axpy(t.embed.g[id*h:(id+1)*h], 1, tp.dx[i*h:(i+1)*h])
	}
}

// apply — шаг AdamW со скоростью lr по накопленному градиенту; градиент
// обнуляется. Норма градиента обрезается до 1, как clip_grad_norm_.
// Возвращает норму до обрезки.
func (t *Trainer) apply(lr float64) float64 {
	const chunk = 1 << 14
	var total float64
	for _, p := range t.weights {
		parts := make([]float64, (len(p.g)+chunk-1)/chunk)
		parallelFor(len(parts), 1, func(from, to int) {
			for c := from; c < to; c++ {
				var s float64
				for _, g := range p.g[c*chunk : min((c+1)*chunk, len(p.g))] {
					s += float64(g) * float64(g)
				}
				parts[c] = s
			}
		})
		for _, s := range parts {
			total += s
		}
	}
	norm := math.Sqrt(total)
	clip := float32(1)
	if c := clipNorm / (norm + 1e-6); c < 1 {
		clip = float32(c)
	}
	t.steps++
	bc1 := 1 - math.Pow(adamBeta1, float64(t.steps))
	bc2 := 1 - math.Pow(adamBeta2, float64(t.steps))
	stepSize, inv := float32(lr/bc1), float32(1/math.Sqrt(bc2))
	keep := float32(1 - lr*weightDecay)
	for _, p := range t.weights {
		parallelFor((len(p.g)+chunk-1)/chunk, 1, func(from, to int) {
			for i := from * chunk; i < min(to*chunk, len(p.g)); i++ {
				g := p.g[i] * clip
				p.g[i] = 0
				m := p.m[i] + (g-p.m[i])*(1-adamBeta1)
				v := p.v[i]*adamBeta2 + g*g*(1-adamBeta2)
				p.m[i], p.v[i] = m, v
				w := p.w[i]
				if p.decay {
					w *= keep
				}
				p.w[i] = w - stepSize*m/(float32(math.Sqrt(float64(v)))*inv+adamEps)
			}
		})
	}
	return norm
}

func (t *Trainer) zeroGrad() {
	for _, p := range t.weights {
		clear(p.g)
	}
}

// mul — y = W·x; счёт тот же, что у matrix.mul прямого прохода модели.
func (p *weight) mul(y, x []float32) {
	for r := 0; r < p.rows; r++ {
		y[r] = dot(p.w[r*p.cols:(r+1)*p.cols], x)
	}
}

// backW — градиент весов: G += dYᵀ·X по T позициям. Строка матрицы — у
// одного ядра.
func (p *weight) backW(dY, X []float32, T int) {
	rows, cols := p.rows, p.cols
	parallelFor(rows, 4, func(from, to int) {
		for r := from; r < to; r++ {
			g := p.g[r*cols : (r+1)*cols]
			for i := 0; i < T; i++ {
				if c := dY[i*rows+r]; c != 0 {
					axpy(g, c, X[i*cols:(i+1)*cols])
				}
			}
		}
	})
}

// backX — градиент входа: dX = Wᵀ·dY по T позициям; add — прибавить.
func (p *weight) backX(dX, dY []float32, T int, add bool) {
	rows, cols := p.rows, p.cols
	parallelFor(T, 2, func(from, to int) {
		for i := from; i < to; i++ {
			dx := dX[i*cols : (i+1)*cols]
			if !add {
				clear(dx)
			}
			for r := 0; r < rows; r++ {
				if c := dY[i*rows+r]; c != 0 {
					axpy(dx, c, p.w[r*cols:(r+1)*cols])
				}
			}
		}
	})
}

// rmsNormR — нормировка RMSNorm, как rmsNorm; возвращает множитель r.
func rmsNormR(dst, x, w []float32, eps float64) float32 {
	var ss float64
	for _, v := range x {
		ss += float64(v) * float64(v)
	}
	r := float32(1 / math.Sqrt(ss/float64(len(x))+eps))
	for i, v := range x {
		dst[i] = w[i] * (v * r)
	}
	return r
}

// rmsBack — обратный проход RMSNorm a = g ⊙ x·r, r = (mean(x²) + ε)^(−½):
// dx = r·(da ⊙ g) − x·r³·⟨da ⊙ g, x⟩/n; градиент g — da ⊙ x·r. add —
// прибавить dx к уже лежащему (остаточная связь).
func rmsBack(dX, dA, X []float32, p *weight, r []float32, T int, add bool) {
	n := p.cols
	parallelFor(T, 16, func(from, to int) {
		for i := from; i < to; i++ {
			x, da, dx := X[i*n:(i+1)*n], dA[i*n:(i+1)*n], dX[i*n:(i+1)*n]
			var s float64
			for j := range x {
				s += float64(da[j]) * float64(p.w[j]) * float64(x[j])
			}
			ri := float64(r[i])
			k := ri * ri * ri * s / float64(n)
			for j := range x {
				d := float32(ri*float64(da[j])*float64(p.w[j]) - float64(x[j])*k)
				if add {
					dx[j] += d
				} else {
					dx[j] = d
				}
			}
		}
	})
	for i := 0; i < T; i++ {
		x, da, ri := X[i*n:(i+1)*n], dA[i*n:(i+1)*n], r[i]
		for j := range x {
			p.g[j] += da[j] * (x[j] * ri)
		}
	}
}

// ropeBack — обратный поворот RoPE: градиент выхода → градиент входа.
func ropeBack(d []float32, heads, hd int, cos, sin []float64) {
	half := hd / 2
	for h := 0; h < heads; h++ {
		v := d[h*hd : (h+1)*hd]
		for i := 0; i < half; i++ {
			a, b := float64(v[i]), float64(v[i+half])
			v[i] = float32(a*cos[i] + b*sin[i])
			v[i+half] = float32(b*cos[i] - a*sin[i])
		}
	}
}

// dot — скалярное произведение в четыре потока, как в matrix.mul.
func dot(a, b []float32) float32 {
	b = b[:len(a)]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}

// axpy — y += a·x.
func axpy(y []float32, a float32, x []float32) {
	x = x[:len(y)]
	i := 0
	for ; i+4 <= len(y); i += 4 {
		y[i] += a * x[i]
		y[i+1] += a * x[i+1]
		y[i+2] += a * x[i+2]
		y[i+3] += a * x[i+3]
	}
	for ; i < len(y); i++ {
		y[i] += a * x[i]
	}
}

// parallelFor — f по кускам [from, to) диапазона 0..n−1 между ядрами;
// grain — сколько индексов ядро берёт за раз.
func parallelFor(n, grain int, f func(from, to int)) {
	workers := min(runtime.GOMAXPROCS(0), (n+grain-1)/grain)
	if workers < 2 {
		if n > 0 {
			f(0, n)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				from := int(next.Add(int64(grain))) - grain
				if from >= n {
					return
				}
				f(from, min(from+grain, n))
			}
		}()
	}
	wg.Wait()
}
