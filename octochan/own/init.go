package own

import (
	"fmt"
	"math/rand"
	"path/filepath"
)

// WriteRandomWeights пишет случайные веса сети конфигурации cfg в каталог
// модели. Такая модель отвечает сразу: ответ пишет её логика, сеть только
// выбирает порядок среди допустимых. Это проверка контура до обучения, а
// не обученная модель.
func WriteRandomWeights(dir string, cfg Config, seed int64) error {
	rng := rand.New(rand.NewSource(seed))
	mat := func(rows, cols int) *Tensor {
		d := make([]float32, rows*cols)
		for i := range d {
			d[i] = float32(rng.NormFloat64() * 0.02)
		}
		return &Tensor{Shape: []int{rows, cols}, Data: d, DType: "F32"}
	}
	ones := func(n int) *Tensor {
		d := make([]float32, n)
		for i := range d {
			d[i] = 1
		}
		return &Tensor{Shape: []int{n}, Data: d, DType: "F32"}
	}
	h, kv := cfg.HiddenSize, cfg.KVHeads*cfg.HeadDim
	w := map[string]*Tensor{"model.embed_tokens.weight": mat(cfg.VocabSize, h), "model.norm.weight": ones(h)}
	for i := 0; i < cfg.Layers; i++ {
		p := fmt.Sprintf("model.layers.%d.", i)
		w[p+"self_attn.q_proj.weight"] = mat(cfg.Heads*cfg.HeadDim, h)
		w[p+"self_attn.k_proj.weight"] = mat(kv, h)
		w[p+"self_attn.v_proj.weight"] = mat(kv, h)
		w[p+"self_attn.o_proj.weight"] = mat(h, cfg.Heads*cfg.HeadDim)
		w[p+"mlp.gate_proj.weight"] = mat(cfg.Intermediate, h)
		w[p+"mlp.up_proj.weight"] = mat(cfg.Intermediate, h)
		w[p+"mlp.down_proj.weight"] = mat(h, cfg.Intermediate)
		w[p+"input_layernorm.weight"] = ones(h)
		w[p+"post_attention_layernorm.weight"] = ones(h)
	}
	return WriteSafetensors(filepath.Join(dir, "model.safetensors"), w)
}
