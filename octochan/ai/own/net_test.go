package own

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

type microExpected struct {
	Seqs [][]int       `json:"seqs"`
	Full [][][]float64 `json:"full"`
	Int8 [][]float64   `json:"int8"`
	Int4 [][]float64   `json:"int4"`
}

func loadMicro(t *testing.T) microExpected {
	t.Helper()
	data, err := os.ReadFile("testdata/micro/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var e microExpected
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func closeProbs(t *testing.T, what string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d вероятностей, ожидалось %d", what, len(got), len(want))
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 2e-5 {
			t.Fatalf("%s: токен %d — %.7f, у torch %.7f", what, i, got[i], want[i])
		}
	}
}

// Прямой проход совпадает с torch (transformers LlamaForCausalLM) на каждой
// позиции. Префиксы идут подряд — кеш KV досчитывает только новый токен;
// возврат к короткому префиксу — кеш обрезается.
func TestNetMatchesTorch(t *testing.T) {
	want := loadMicro(t)
	net, err := LoadNet("testdata/micro")
	if err != nil {
		t.Fatal(err)
	}
	if net.Params() != 19104 {
		t.Fatalf("параметров %d", net.Params())
	}
	for s, seq := range want.Seqs {
		for k := 1; k <= len(seq); k++ {
			got, err := net.Probs(seq[:k])
			if err != nil {
				t.Fatal(err)
			}
			closeProbs(t, "последовательность", got, want.Full[s][k-1])
		}
	}
	got, _ := net.Probs(want.Seqs[0][:3])
	closeProbs(t, "кеш назад", got, want.Full[0][2])
	got, _ = net.Probs(want.Seqs[0][:3])
	closeProbs(t, "тот же вход", got, want.Full[0][2])
	if _, err := net.Probs([]int{1, 99}); err == nil {
		t.Fatal("токен вне словаря принят")
	}
}

// Квантованные веса (quant.json, quant.safetensors) читаются, как их
// распаковывал torch; квантование ochan даёт те же числа.
func TestQuantized(t *testing.T) {
	want := loadMicro(t)
	for _, c := range []struct {
		dir   string
		bits  int
		group int
		probs [][]float64
	}{{"testdata/micro-int8", 8, 0, want.Int8}, {"testdata/micro-int4", 4, 16, want.Int4}} {
		net, err := LoadNet(c.dir)
		if err != nil {
			t.Fatal(err)
		}
		for s, seq := range want.Seqs {
			got, _ := net.Probs(seq)
			closeProbs(t, c.dir, got, c.probs[s])
		}
		out := filepath.Join(t.TempDir(), "q")
		res, err := Quantize("testdata/micro", out, c.bits, c.group)
		if err != nil {
			t.Fatal(err)
		}
		if res.After >= res.Before {
			t.Fatalf("%s: веса %d → %d байт", c.dir, res.Before, res.After)
		}
		mine, err := LoadNet(out)
		if err != nil {
			t.Fatal(err)
		}
		for s, seq := range want.Seqs {
			got, _ := mine.Probs(seq)
			closeProbs(t, "квантование ochan "+c.dir, got, c.probs[s])
		}
	}
	if _, err := Quantize("testdata/micro", t.TempDir(), 4, 7); err == nil {
		t.Fatal("группа, на которую ширина не делится, принята")
	}
}

func TestHalf(t *testing.T) {
	for _, v := range []float32{0, 1, -2.5, 0.1, 65504, 1e-5, 3.0517578e-05, 6.1e-5} {
		h := halfToFloat(floatToHalf(v))
		if math.Abs(float64(h-v)) > math.Abs(float64(v))*1e-3+1e-7 {
			t.Fatalf("%g → %g", v, h)
		}
	}
}
