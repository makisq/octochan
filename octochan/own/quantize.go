package own

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// Квантование своей модели ℒ — шаг 4.
//
// Веса матриц (вход, внимание, MLP; выход связан со входом) — симметрично
// по группам строки: int8 (группа — вся строка) или int4 (группа group, по
// два значения в байте, младшая половина — первое); масштаб группы — fp16.
// Нормировки остаются fp32. Каталог — снова модель: config.json,
// fpmtok.json и quant.safetensors с quant.json; при загрузке веса
// распаковываются.

const (
	quantJSON    = "quant.json"
	quantWeights = "quant.safetensors"
)

type quantMeta struct {
	Bits    int     `json:"bits"`
	Group   int     `json:"group"`
	Tensors [][]any `json:"tensors"` // [имя, [строк, столбцов]]
}

// IsQuantized — каталог с квантованными весами.
func IsQuantized(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, quantJSON))
	return err == nil
}

func readQuantized(dir string) (map[string]*Tensor, error) {
	data, err := os.ReadFile(filepath.Join(dir, quantJSON))
	if err != nil {
		return nil, err
	}
	var meta quantMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("%s: %w", quantJSON, err)
	}
	if meta.Bits != 4 && meta.Bits != 8 {
		return nil, fmt.Errorf("%s: %d бит — нужно 4 или 8", quantJSON, meta.Bits)
	}
	raw, err := ReadSafetensors(filepath.Join(dir, quantWeights))
	if err != nil {
		return nil, err
	}
	out := map[string]*Tensor{}
	for name, t := range raw {
		if t.Data != nil {
			out[name] = t
		}
	}
	for _, entry := range meta.Tensors {
		if len(entry) != 2 {
			return nil, fmt.Errorf("%s: запись %v", quantJSON, entry)
		}
		name, _ := entry[0].(string)
		dims, _ := entry[1].([]any)
		if name == "" || len(dims) != 2 {
			return nil, fmt.Errorf("%s: запись %v", quantJSON, entry)
		}
		rows, cols := int(dims[0].(float64)), int(dims[1].(float64))
		q, scale := raw[name+".q"], raw[name+".scale"]
		if q == nil || scale == nil || scale.Data == nil || rows <= 0 || cols <= 0 || len(scale.Data)%rows != 0 {
			return nil, fmt.Errorf("%s: квантованных весов нет или они негодны", name)
		}
		values, err := unpack(q.Raw, rows*cols, meta.Bits)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		groups := len(scale.Data) / rows
		if groups == 0 || cols%groups != 0 {
			return nil, fmt.Errorf("%s: %d групп на строку из %d", name, groups, cols)
		}
		g := cols / groups
		w := make([]float32, rows*cols)
		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				w[r*cols+c] = float32(values[r*cols+c]) * scale.Data[r*groups+c/g]
			}
		}
		out[name] = &Tensor{Shape: []int{rows, cols}, Data: w, DType: "F32"}
	}
	return out, nil
}

func unpack(raw []byte, n, bits int) ([]int8, error) {
	out := make([]int8, n)
	if bits == 8 {
		if len(raw) != n {
			return nil, fmt.Errorf("%d байт на %d значений", len(raw), n)
		}
		for i, b := range raw {
			out[i] = int8(b)
		}
		return out, nil
	}
	if len(raw) != (n+1)/2 {
		return nil, fmt.Errorf("%d байт на %d значений по 4 бита", len(raw), n)
	}
	for i := 0; i < n; i++ {
		u := raw[i/2]
		if i%2 == 1 {
			u >>= 4
		}
		u &= 0xf
		v := int8(u)
		if u > 7 {
			v -= 16
		}
		out[i] = v
	}
	return out, nil
}

// QuantizeResult — что вышло из квантования.
type QuantizeResult struct {
	Bits, Group   int
	Before, After int64
}

// Quantize пишет квантованную модель каталога src в out: int8 или int4 с
// группой group (0 — вся строка; у int4 по умолчанию 32).
func Quantize(src, out string, bits, group int) (QuantizeResult, error) {
	res := QuantizeResult{Bits: bits, Group: group}
	if bits != 4 && bits != 8 {
		return res, fmt.Errorf("--bits %d: 4 или 8", bits)
	}
	if bits == 4 && group == 0 {
		res.Group, group = 32, 32
	}
	if IsQuantized(src) {
		return res, fmt.Errorf("%s уже квантована", src)
	}
	cfg, err := LoadConfig(src)
	if err != nil {
		return res, err
	}
	weights, err := readWeights(src)
	if err != nil {
		return res, err
	}
	tensors := map[string]*Tensor{}
	var listed [][]any
	for name, t := range weights {
		if name == "lm_head.weight" && cfg.TieWordEmbedding {
			continue
		}
		if t.Data == nil {
			return res, fmt.Errorf("%s: тип %s не квантуется", name, t.DType)
		}
		if len(t.Shape) != 2 {
			tensors[name] = &Tensor{Shape: t.Shape, Data: t.Data, DType: "F32"}
			continue
		}
		rows, cols := t.Shape[0], t.Shape[1]
		g := group
		if g <= 0 || g >= cols {
			g = cols
		}
		if cols%g != 0 {
			return res, fmt.Errorf("матрица %s %d×%d: ширина не делится на группу %d — другая --group", name, rows, cols, g)
		}
		q, scale := quantizeRows(t.Data, rows, cols, g, bits)
		tensors[name+".scale"] = &Tensor{Shape: []int{rows, cols / g}, Data: scale, DType: "F16"}
		if bits == 4 {
			packed := make([]byte, (len(q)+1)/2)
			for i, v := range q {
				u := byte(v) & 0xf
				if i%2 == 1 {
					u <<= 4
				}
				packed[i/2] |= u
			}
			tensors[name+".q"] = &Tensor{Shape: []int{len(packed)}, Raw: packed, DType: "U8"}
		} else {
			raw := make([]byte, len(q))
			for i, v := range q {
				raw[i] = byte(v)
			}
			tensors[name+".q"] = &Tensor{Shape: []int{rows, cols}, Raw: raw, DType: "I8"}
		}
		listed = append(listed, []any{name, []int{rows, cols}})
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return res, err
	}
	if err := WriteSafetensors(filepath.Join(out, quantWeights), tensors); err != nil {
		return res, err
	}
	meta, _ := json.Marshal(map[string]any{"bits": bits, "group": group, "tensors": listed})
	if err := os.WriteFile(filepath.Join(out, quantJSON), meta, 0o644); err != nil {
		return res, err
	}
	for _, name := range []string{ConfigFile, TokenizerFile, ParamsFile} {
		if err := copyFile(filepath.Join(src, name), filepath.Join(out, name)); err != nil && !os.IsNotExist(err) {
			return res, err
		}
	}
	res.Before, res.After = WeightsSize(src), WeightsSize(out)
	return res, nil
}

// quantizeRows — целые в [−2^(bits−1), 2^(bits−1) − 1] и масштаб группы
// (хранится в fp16). Счёт — в float32, округление к чётному, как у torch.
func quantizeRows(w []float32, rows, cols, g, bits int) ([]int8, []float32) {
	top := float32(int(1)<<(bits-1) - 1)
	q := make([]int8, len(w))
	scales := make([]float32, 0, rows*cols/g)
	for r := 0; r < rows; r++ {
		for start := 0; start < cols; start += g {
			chunk := w[r*cols+start : r*cols+start+g]
			var amax float32
			for _, v := range chunk {
				if v < 0 {
					v = -v
				}
				if v > amax {
					amax = v
				}
			}
			scale := amax / top
			if scale < 1e-12 {
				scale = 1e-12
			}
			for i, v := range chunk {
				x := float32(math.RoundToEven(float64(v / scale)))
				q[r*cols+start+i] = int8(max(-top-1, min(top, x)))
			}
			scales = append(scales, halfToFloat(floatToHalf(scale)))
		}
	}
	return q, scales
}

// WeightsSize — размер весов каталога: все *.safetensors и *.bin — целый
// model.safetensors или шарды.
func WeightsSize(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if (ext == ".safetensors" || ext == ".bin") && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	return total
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// QuantBits — бит на вес квантованной модели; 0 — веса полные.
func QuantBits(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, quantJSON))
	if err != nil {
		return 0
	}
	var meta quantMeta
	if json.Unmarshal(data, &meta) != nil {
		return 0
	}
	return meta.Bits
}

// Compare — сверка двух сетей на парах корпуса с подстановкой верного
// префикса: доля позиций ответа, где самый вероятный токен тот же, и
// средние потери на ответе у обеих.
func Compare(a, b *Net, pairs []Pair) (agree, lossA, lossB float64, err error) {
	same, total := 0, 0
	for _, p := range pairs {
		if p.Start < 1 || p.Start > len(p.IDs) {
			return 0, 0, 0, fmt.Errorf("пара без токенов ответа")
		}
		for pos := p.Start; pos < len(p.IDs); pos++ {
			pa, err := a.Probs(p.IDs[:pos])
			if err != nil {
				return 0, 0, 0, err
			}
			pb, err := b.Probs(p.IDs[:pos])
			if err != nil {
				return 0, 0, 0, err
			}
			if argmax(pa) == argmax(pb) {
				same++
			}
			target := p.IDs[pos]
			lossA -= math.Log(math.Max(pa[target], 1e-45))
			lossB -= math.Log(math.Max(pb[target], 1e-45))
			total++
		}
	}
	if total == 0 {
		return 0, 0, 0, fmt.Errorf("позиций ответа нет")
	}
	return float64(same) / float64(total), lossA / float64(total), lossB / float64(total), nil
}

func argmax(p []float64) int {
	best := 0
	for i, v := range p {
		if v > p[best] {
			best = i
		}
	}
	return best
}
