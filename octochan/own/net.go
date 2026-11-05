package own

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Сеть своей модели — архитектура Llama: эмбеддинги (связаны с выходом),
// слои из RMSNorm, внимания с RoPE и MLP SwiGLU, финальная нормировка.
// Считается на процессоре в float32; кеш KV держит общий префикс
// прошлого запроса, поэтому досчитываются только новые токены.

// Config — config.json модели (поля Llama, которые нужны прямому проходу).
type Config struct {
	VocabSize        int     `json:"vocab_size"`
	HiddenSize       int     `json:"hidden_size"`
	Layers           int     `json:"num_hidden_layers"`
	Heads            int     `json:"num_attention_heads"`
	KVHeads          int     `json:"num_key_value_heads"`
	Intermediate     int     `json:"intermediate_size"`
	HeadDim          int     `json:"head_dim"`
	MaxPositions     int     `json:"max_position_embeddings"`
	RMSNormEps       float64 `json:"rms_norm_eps"`
	RopeTheta        float64 `json:"rope_theta"`
	TieWordEmbedding bool    `json:"tie_word_embeddings"`
	RopeParameters   *struct {
		Theta float64 `json:"rope_theta"`
		Type  string  `json:"rope_type"`
	} `json:"rope_parameters,omitempty"`
	RopeScaling map[string]any `json:"rope_scaling,omitempty"`
	ModelType   string         `json:"model_type"`
	HiddenAct   string         `json:"hidden_act"`
	Bos         int            `json:"bos_token_id"`
	Eos         int            `json:"eos_token_id"`
	PadToken    int            `json:"pad_token_id"`
}

// ConfigFile — config.json в каталоге модели.
const ConfigFile = "config.json"

// Sizes — размеры своей модели: ширина, слои, головы внимания, ширина MLP.
var Sizes = map[string][4]int{
	"tiny":  {256, 6, 4, 704},
	"small": {384, 8, 6, 1024},
	"base":  {512, 12, 8, 1408},
}

// NewConfig — конфигурация новой модели размера size со словарём tok.
func NewConfig(tok *Tokenizer, size string, context int) (Config, error) {
	s, ok := Sizes[size]
	if !ok {
		return Config{}, fmt.Errorf("размер %q: tiny, small или base", size)
	}
	c := Config{VocabSize: tok.Len(), HiddenSize: s[0], Layers: s[1], Heads: s[2], KVHeads: s[2], Intermediate: s[3],
		HeadDim: s[0] / s[2], MaxPositions: context, RMSNormEps: 1e-6, RopeTheta: 10000, TieWordEmbedding: true,
		ModelType: "llama", HiddenAct: "silu", Bos: tok.Bos, Eos: tok.Eos, PadToken: tok.Pad}
	return c, nil
}

// Save пишет config.json — его читает и обучение (llama/train_own.py).
func (c Config) Save(dir string) error {
	out := map[string]any{
		"architectures": []string{"LlamaForCausalLM"}, "model_type": "llama", "hidden_act": "silu",
		"vocab_size": c.VocabSize, "hidden_size": c.HiddenSize, "num_hidden_layers": c.Layers,
		"num_attention_heads": c.Heads, "num_key_value_heads": c.KVHeads, "intermediate_size": c.Intermediate,
		"head_dim": c.HeadDim, "max_position_embeddings": c.MaxPositions, "rms_norm_eps": c.RMSNormEps,
		"rope_theta": c.theta(), "tie_word_embeddings": c.TieWordEmbedding, "attention_bias": false, "mlp_bias": false,
		"bos_token_id": c.Bos, "eos_token_id": c.Eos, "pad_token_id": c.PadToken, "initializer_range": 0.02,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ConfigFile), append(data, '\n'), 0o644)
}

func (c Config) theta() float64 {
	if c.RopeParameters != nil && c.RopeParameters.Theta > 0 {
		return c.RopeParameters.Theta
	}
	if c.RopeTheta > 0 {
		return c.RopeTheta
	}
	return 10000
}

// LoadConfig — config.json каталога модели.
func LoadConfig(dir string) (Config, error) {
	var c Config
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if c.KVHeads == 0 {
		c.KVHeads = c.Heads
	}
	if c.HeadDim == 0 && c.Heads > 0 {
		c.HeadDim = c.HiddenSize / c.Heads
	}
	if c.RMSNormEps == 0 {
		c.RMSNormEps = 1e-6
	}
	switch {
	case c.ModelType != "" && c.ModelType != "llama":
		return c, fmt.Errorf("%s: модель %q — своя модель ℒ архитектуры llama", ConfigFile, c.ModelType)
	case c.HiddenAct != "" && c.HiddenAct != "silu":
		return c, fmt.Errorf("%s: активация %q — нужна silu", ConfigFile, c.HiddenAct)
	case c.RopeParameters != nil && c.RopeParameters.Type != "" && c.RopeParameters.Type != "default",
		len(c.RopeScaling) > 0:
		return c, fmt.Errorf("%s: масштабирование RoPE не поддерживается", ConfigFile)
	case c.VocabSize <= 0 || c.HiddenSize <= 0 || c.Layers <= 0 || c.Heads <= 0 || c.Intermediate <= 0 || c.HeadDim%2 != 0:
		return c, fmt.Errorf("%s: размеры модели негодны", ConfigFile)
	case c.Heads%c.KVHeads != 0:
		return c, fmt.Errorf("%s: голов внимания %d не делится на головы KV %d", ConfigFile, c.Heads, c.KVHeads)
	}
	return c, nil
}

type layer struct {
	inNorm, postNorm []float32
	q, k, v, o       matrix
	gate, up, down   matrix
}

// matrix — веса линейного слоя: rows выходов × cols входов, y = W·x.
type matrix struct {
	rows, cols int
	w          []float32
}

// Net — сеть модели.
type Net struct {
	Config
	embed   matrix
	head    matrix
	norm    []float32
	layers  []layer
	invFreq []float64

	mu    sync.Mutex
	cache []int       // токены, для которых посчитан кеш
	keys  [][]float32 // по слоям: позиции × KVHeads·HeadDim
	vals  [][]float32
	last  []float32 // скрытое состояние последней позиции кеша
}

// LoadNet — сеть каталога модели: полные веса или квантованные.
func LoadNet(dir string) (*Net, error) {
	cfg, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	var weights map[string]*Tensor
	if IsQuantized(dir) {
		weights, err = readQuantized(dir)
	} else {
		weights, err = readWeights(dir)
	}
	if err != nil {
		return nil, err
	}
	return newNet(cfg, weights)
}

func newNet(cfg Config, w map[string]*Tensor) (*Net, error) {
	n := &Net{Config: cfg}
	get := func(name string, shape ...int) ([]float32, error) {
		t, ok := w[name]
		if !ok {
			return nil, fmt.Errorf("в весах нет %s", name)
		}
		if t.Data == nil {
			return nil, fmt.Errorf("%s: тип %s без распаковки", name, t.DType)
		}
		want := 1
		for _, d := range shape {
			want *= d
		}
		if len(t.Data) != want {
			return nil, fmt.Errorf("%s: форма %v, ожидалась %v", name, t.Shape, shape)
		}
		return t.Data, nil
	}
	mat := func(name string, rows, cols int) (matrix, error) {
		d, err := get(name, rows, cols)
		return matrix{rows, cols, d}, err
	}
	h, kv := cfg.HiddenSize, cfg.KVHeads*cfg.HeadDim
	var err error
	if n.embed, err = mat("model.embed_tokens.weight", cfg.VocabSize, h); err != nil {
		return nil, err
	}
	if _, ok := w["lm_head.weight"]; ok && !cfg.TieWordEmbedding {
		if n.head, err = mat("lm_head.weight", cfg.VocabSize, h); err != nil {
			return nil, err
		}
	} else {
		n.head = n.embed
	}
	if n.norm, err = get("model.norm.weight", h); err != nil {
		return nil, err
	}
	for i := 0; i < cfg.Layers; i++ {
		p := fmt.Sprintf("model.layers.%d.", i)
		var l layer
		for _, m := range []struct {
			dst        *matrix
			name       string
			rows, cols int
		}{
			{&l.q, "self_attn.q_proj.weight", cfg.Heads * cfg.HeadDim, h},
			{&l.k, "self_attn.k_proj.weight", kv, h},
			{&l.v, "self_attn.v_proj.weight", kv, h},
			{&l.o, "self_attn.o_proj.weight", h, cfg.Heads * cfg.HeadDim},
			{&l.gate, "mlp.gate_proj.weight", cfg.Intermediate, h},
			{&l.up, "mlp.up_proj.weight", cfg.Intermediate, h},
			{&l.down, "mlp.down_proj.weight", h, cfg.Intermediate},
		} {
			if *m.dst, err = mat(p+m.name, m.rows, m.cols); err != nil {
				return nil, err
			}
		}
		if l.inNorm, err = get(p+"input_layernorm.weight", h); err != nil {
			return nil, err
		}
		if l.postNorm, err = get(p+"post_attention_layernorm.weight", h); err != nil {
			return nil, err
		}
		n.layers = append(n.layers, l)
	}
	half := cfg.HeadDim / 2
	theta := cfg.theta()
	for i := 0; i < half; i++ {
		n.invFreq = append(n.invFreq, 1/math.Pow(theta, float64(2*i)/float64(cfg.HeadDim)))
	}
	n.keys = make([][]float32, cfg.Layers)
	n.vals = make([][]float32, cfg.Layers)
	return n, nil
}

// Params — число параметров сети.
func (n *Net) Params() int {
	total := len(n.embed.w) + len(n.norm)
	if &n.head.w[0] != &n.embed.w[0] {
		total += len(n.head.w)
	}
	for _, l := range n.layers {
		total += len(l.q.w) + len(l.k.w) + len(l.v.w) + len(l.o.w) + len(l.gate.w) + len(l.up.w) + len(l.down.w) +
			len(l.inNorm) + len(l.postNorm)
	}
	return total
}

// Probs — распределение следующего токена после ids (после softmax, до
// любой выборки).
func (n *Net) Probs(ids []int) ([]float64, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("пустой вход сети")
	}
	if n.MaxPositions > 0 && len(ids) > n.MaxPositions {
		return nil, fmt.Errorf("вход %d токенов длиннее контекста модели %d", len(ids), n.MaxPositions)
	}
	for _, id := range ids {
		if id < 0 || id >= n.VocabSize {
			return nil, fmt.Errorf("токен %d вне словаря сети %d", id, n.VocabSize)
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	common := 0
	for common < len(n.cache) && common < len(ids) && n.cache[common] == ids[common] {
		common++
	}
	// Последнюю позицию надо досчитать, чтобы был её выход.
	if common == len(ids) {
		if common == len(n.cache) && n.last != nil {
			return n.output(n.last), nil
		}
		common--
	}
	kv := n.KVHeads * n.HeadDim
	for l := range n.keys {
		n.keys[l] = n.keys[l][:common*kv]
		n.vals[l] = n.vals[l][:common*kv]
	}
	n.cache = append(n.cache[:common], ids[common:]...)
	x := n.block(ids[common:], common)
	n.last = x
	return n.output(x), nil
}

// block — токены tokens с позиции start через все слои; кеш KV
// пополняется. Слой считается для всех токенов сразу: проекции и MLP
// токенов независимы и идут параллельно, внимание токена — по кешу до него.
// Возвращает скрытое состояние последнего токена.
func (n *Net) block(tokens []int, start int) []float32 {
	h, hd, nh, kvh := n.HiddenSize, n.HeadDim, n.Heads, n.KVHeads
	group, T := nh/kvh, len(tokens)
	type state struct {
		x, norm, q, k, v, att, gate, up, out []float32
		cos, sin                             []float64
		scores                               []float64
	}
	st := make([]*state, T)
	for i, tok := range tokens {
		s := &state{x: make([]float32, h), norm: make([]float32, h), q: make([]float32, nh*hd), k: make([]float32, kvh*hd),
			v: make([]float32, kvh*hd), att: make([]float32, nh*hd), gate: make([]float32, n.Intermediate),
			up: make([]float32, n.Intermediate), out: make([]float32, h), cos: make([]float64, hd/2), sin: make([]float64, hd/2),
			scores: make([]float64, start+T)}
		copy(s.x, n.embed.w[tok*h:(tok+1)*h])
		for j, f := range n.invFreq {
			a := float64(start+i) * f
			s.cos[j], s.sin[j] = math.Cos(a), math.Sin(a)
		}
		st[i] = s
	}
	// Одного токена хватает на всё ядро только в больших матрицах — там
	// параллельно считаются строки; иначе — токены.
	inner := T == 1
	scale := 1 / math.Sqrt(float64(hd))
	for li := range n.layers {
		l := &n.layers[li]
		parallel(T, func(i int) {
			s := st[i]
			rmsNorm(s.norm, s.x, l.inNorm, n.RMSNormEps)
			l.q.mul(s.q, s.norm, inner)
			l.k.mul(s.k, s.norm, inner)
			l.v.mul(s.v, s.norm, inner)
			rope(s.q, nh, hd, s.cos, s.sin)
			rope(s.k, kvh, hd, s.cos, s.sin)
		})
		for _, s := range st {
			n.keys[li] = append(n.keys[li], s.k...)
			n.vals[li] = append(n.vals[li], s.v...)
		}
		keys, vals := n.keys[li], n.vals[li]
		parallel(T, func(i int) {
			s := st[i]
			positions := start + i + 1
			scores := s.scores[:positions]
			for head := 0; head < nh; head++ {
				kvHead := head / group
				qh := s.q[head*hd : (head+1)*hd]
				top := math.Inf(-1)
				for p := 0; p < positions; p++ {
					kp := keys[p*kvh*hd+kvHead*hd : p*kvh*hd+(kvHead+1)*hd]
					var sum float64
					for j := range qh {
						sum += float64(qh[j]) * float64(kp[j])
					}
					sum *= scale
					scores[p] = sum
					top = math.Max(top, sum)
				}
				var total float64
				for p := range scores {
					scores[p] = math.Exp(scores[p] - top)
					total += scores[p]
				}
				ah := s.att[head*hd : (head+1)*hd]
				clear(ah)
				for p := 0; p < positions; p++ {
					w := float32(scores[p] / total)
					vp := vals[p*kvh*hd+kvHead*hd : p*kvh*hd+(kvHead+1)*hd]
					for j := range ah {
						ah[j] += w * vp[j]
					}
				}
			}
			l.o.mul(s.out, s.att, inner)
			for j := range s.x {
				s.x[j] += s.out[j]
			}
			rmsNorm(s.norm, s.x, l.postNorm, n.RMSNormEps)
			l.gate.mul(s.gate, s.norm, inner)
			l.up.mul(s.up, s.norm, inner)
			for j := range s.gate {
				g := float64(s.gate[j])
				s.gate[j] = float32(g/(1+math.Exp(-g))) * s.up[j]
			}
			l.down.mul(s.out, s.gate, inner)
			for j := range s.x {
				s.x[j] += s.out[j]
			}
		})
	}
	return st[T-1].x
}

// parallel — f(0..n−1) по ядрам; один — без горутин.
func parallel(n int, f func(i int)) {
	workers := min(runtime.GOMAXPROCS(0), n)
	if workers < 2 {
		for i := 0; i < n; i++ {
			f(i)
		}
		return
	}
	var wg sync.WaitGroup
	next := make(chan int, n)
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	wg.Wait()
}

// output — softmax выхода сети по скрытому состоянию последней позиции.
func (n *Net) output(x []float32) []float64 {
	norm := make([]float32, len(x))
	rmsNorm(norm, x, n.norm, n.RMSNormEps)
	logits := make([]float32, n.VocabSize)
	n.head.mul(logits, norm, true)
	probs := make([]float64, len(logits))
	top := math.Inf(-1)
	for _, l := range logits {
		top = math.Max(top, float64(l))
	}
	var sum float64
	for i, l := range logits {
		probs[i] = math.Exp(float64(l) - top)
		sum += probs[i]
	}
	for i := range probs {
		probs[i] /= sum
	}
	return probs
}

func rmsNorm(dst, x, w []float32, eps float64) {
	var ss float64
	for _, v := range x {
		ss += float64(v) * float64(v)
	}
	r := float32(1 / math.Sqrt(ss/float64(len(x))+eps))
	for i, v := range x {
		dst[i] = w[i] * (v * r)
	}
}

// rope — поворот RoPE по головам: половины головы — (x₁, x₂) → (x₁·cos −
// x₂·sin, x₂·cos + x₁·sin), как rotate_half у Llama.
func rope(x []float32, heads, hd int, cos, sin []float64) {
	half := hd / 2
	for h := 0; h < heads; h++ {
		v := x[h*hd : (h+1)*hd]
		for i := 0; i < half; i++ {
			a, b := float64(v[i]), float64(v[i+half])
			v[i] = float32(a*cos[i] - b*sin[i])
			v[i+half] = float32(b*cos[i] + a*sin[i])
		}
	}
}

// mul — y = W·x; строки делятся между ядрами, когда матрица большая и
// split — ядра свободны.
func (m matrix) mul(y, x []float32, split bool) {
	work := func(from, to int) {
		for r := from; r < to; r++ {
			row := m.w[r*m.cols : (r+1)*m.cols]
			var s0, s1, s2, s3 float32
			c := 0
			for ; c+4 <= len(row); c += 4 {
				s0 += row[c] * x[c]
				s1 += row[c+1] * x[c+1]
				s2 += row[c+2] * x[c+2]
				s3 += row[c+3] * x[c+3]
			}
			for ; c < len(row); c++ {
				s0 += row[c] * x[c]
			}
			y[r] = (s0 + s1) + (s2 + s3)
		}
	}
	workers := runtime.GOMAXPROCS(0)
	if !split || m.rows*m.cols < 1<<17 || workers < 2 {
		work(0, m.rows)
		return
	}
	var wg sync.WaitGroup
	chunk := (m.rows + workers - 1) / workers
	for from := 0; from < m.rows; from += chunk {
		to := min(from+chunk, m.rows)
		wg.Add(1)
		go func() {
			defer wg.Done()
			work(from, to)
		}()
	}
	wg.Wait()
}
