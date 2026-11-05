package own

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Веса в формате safetensors: восемь байт длины заголовка, заголовок JSON
// (имя → dtype, shape, data_offsets), затем данные подряд, little-endian.

// Tensor — тензор весов: форма и значения в float32. Квантованные
// (I8, U8) хранятся как есть в Raw.
type Tensor struct {
	Shape []int
	Data  []float32
	DType string
	Raw   []byte
}

// Len — число значений по форме.
func (t *Tensor) Len() int {
	n := 1
	for _, d := range t.Shape {
		n *= d
	}
	return n
}

type tensorHeader struct {
	DType   string `json:"dtype"`
	Shape   []int  `json:"shape"`
	Offsets [2]int `json:"data_offsets"`
}

// maxHeader — предел заголовка safetensors.
const maxHeader = 100 << 20

// ReadSafetensors — тензоры файла.
func ReadSafetensors(path string) (map[string]*Tensor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, fmt.Errorf("%s: не safetensors", path)
	}
	n := binary.LittleEndian.Uint64(data[:8])
	if n > maxHeader || 8+n > uint64(len(data)) {
		return nil, fmt.Errorf("%s: заголовок safetensors негоден", path)
	}
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data[8:8+n], &head); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	body := data[8+n:]
	out := map[string]*Tensor{}
	for name, raw := range head {
		if name == "__metadata__" {
			continue
		}
		var h tensorHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, name, err)
		}
		if h.Offsets[0] < 0 || h.Offsets[1] < h.Offsets[0] || h.Offsets[1] > len(body) {
			return nil, fmt.Errorf("%s: %s: смещения вне файла", path, name)
		}
		t := &Tensor{Shape: h.Shape, DType: h.DType}
		chunk := body[h.Offsets[0]:h.Offsets[1]]
		size := map[string]int{"F32": 4, "F16": 2, "BF16": 2, "I8": 1, "U8": 1}[h.DType]
		if size == 0 {
			return nil, fmt.Errorf("%s: %s: тип %s не поддерживается", path, name, h.DType)
		}
		if len(chunk) != t.Len()*size {
			return nil, fmt.Errorf("%s: %s: %d байт на форму %v", path, name, len(chunk), h.Shape)
		}
		switch h.DType {
		case "F32":
			t.Data = make([]float32, t.Len())
			for i := range t.Data {
				t.Data[i] = math.Float32frombits(binary.LittleEndian.Uint32(chunk[4*i:]))
			}
		case "F16":
			t.Data = make([]float32, t.Len())
			for i := range t.Data {
				t.Data[i] = halfToFloat(binary.LittleEndian.Uint16(chunk[2*i:]))
			}
		case "BF16":
			t.Data = make([]float32, t.Len())
			for i := range t.Data {
				t.Data[i] = math.Float32frombits(uint32(binary.LittleEndian.Uint16(chunk[2*i:])) << 16)
			}
		default:
			t.Raw = append([]byte(nil), chunk...)
		}
		out[name] = t
	}
	return out, nil
}

// WriteSafetensors пишет тензоры: F32 и F16 — из Data, I8 и U8 — из Raw.
func WriteSafetensors(path string, tensors map[string]*Tensor) error {
	names := make([]string, 0, len(tensors))
	for name := range tensors {
		names = append(names, name)
	}
	sort.Strings(names)
	head := map[string]any{"__metadata__": map[string]string{"format": "pt"}}
	var body []byte
	for _, name := range names {
		t := tensors[name]
		var chunk []byte
		switch t.DType {
		case "F32", "":
			chunk = make([]byte, 4*len(t.Data))
			for i, v := range t.Data {
				binary.LittleEndian.PutUint32(chunk[4*i:], math.Float32bits(v))
			}
		case "F16":
			chunk = make([]byte, 2*len(t.Data))
			for i, v := range t.Data {
				binary.LittleEndian.PutUint16(chunk[2*i:], floatToHalf(v))
			}
		case "I8", "U8":
			chunk = t.Raw
		default:
			return fmt.Errorf("%s: тип %s не пишется", name, t.DType)
		}
		dtype := t.DType
		if dtype == "" {
			dtype = "F32"
		}
		head[name] = tensorHeader{DType: dtype, Shape: t.Shape, Offsets: [2]int{len(body), len(body) + len(chunk)}}
		body = append(body, chunk...)
	}
	h, err := json.Marshal(head)
	if err != nil {
		return err
	}
	for len(h)%8 != 0 {
		h = append(h, ' ')
	}
	out := make([]byte, 8, 8+len(h)+len(body))
	binary.LittleEndian.PutUint64(out, uint64(len(h)))
	out = append(append(out, h...), body...)
	return os.WriteFile(path, out, 0o644)
}

// readWeights — веса каталога модели: model.safetensors или шарды по
// model.safetensors.index.json.
func readWeights(dir string) (map[string]*Tensor, error) {
	single := filepath.Join(dir, "model.safetensors")
	if _, err := os.Stat(single); err == nil {
		return ReadSafetensors(single)
	}
	data, err := os.ReadFile(filepath.Join(dir, "model.safetensors.index.json"))
	if err != nil {
		return nil, fmt.Errorf("весов нет: ни model.safetensors, ни model.safetensors.index.json в %s", dir)
	}
	var index struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, f := range index.WeightMap {
		files[f] = true
	}
	out := map[string]*Tensor{}
	for f := range files {
		if filepath.Base(f) != f {
			return nil, fmt.Errorf("шард %q — не файл каталога модели", f)
		}
		part, err := ReadSafetensors(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		for k, v := range part {
			out[k] = v
		}
	}
	return out, nil
}

// halfToFloat — IEEE 754 half → float32.
func halfToFloat(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	frac := uint32(h) & 0x3ff
	switch {
	case exp == 0 && frac == 0:
		return math.Float32frombits(sign)
	case exp == 0:
		// Денормализованное: frac · 2⁻²⁴.
		v := float32(frac) * float32(math.Pow(2, -24))
		if sign != 0 {
			v = -v
		}
		return v
	case exp == 0x1f:
		return math.Float32frombits(sign | 0x7f800000 | frac<<13)
	}
	return math.Float32frombits(sign | (exp+112)<<23 | frac<<13)
}

// floatToHalf — float32 → IEEE 754 half, округление к ближайшему чётному.
func floatToHalf(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16(b>>16) & 0x8000
	exp := int32(b>>23) & 0xff
	frac := b & 0x7fffff
	if exp == 0xff {
		if frac != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	}
	e := exp - 127 + 15
	switch {
	case e >= 0x1f:
		return sign | 0x7c00
	case e <= 0:
		if e < -10 {
			return sign
		}
		frac |= 0x800000
		shift := uint32(14 - e)
		half := frac >> shift
		rem := frac & (1<<shift - 1)
		mid := uint32(1) << (shift - 1)
		if rem > mid || rem == mid && half&1 == 1 {
			half++
		}
		return sign | uint16(half)
	}
	half := uint32(e)<<10 | frac>>13
	rem := frac & 0x1fff
	if rem > 0x1000 || rem == 0x1000 && half&1 == 1 {
		half++
	}
	return sign | uint16(half)
}
