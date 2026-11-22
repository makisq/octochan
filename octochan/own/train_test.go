package own

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type trainExpected struct {
	Pairs []struct {
		IDs   []int `json:"ids"`
		Start int   `json:"start"`
	} `json:"pairs"`
	Loss   float64   `json:"loss"`
	Answer float64   `json:"answer"`
	Norm   float64   `json:"norm"`
	LR     float64   `json:"lr"`
	Steps  int       `json:"steps"`
	Losses []float64 `json:"losses"`
}

func loadTrainFixture(t *testing.T) (*Trainer, []example, trainExpected) {
	t.Helper()
	data, err := os.ReadFile("testdata/train/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var want trainExpected
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	net, err := LoadNet("testdata/train")
	if err != nil {
		t.Fatal(err)
	}
	var pairs []example
	for _, p := range want.Pairs {
		pairs = append(pairs, example{p.IDs, p.Start})
	}
	return NewTrainer(net), pairs, want
}

// closeTensors — наибольшее расхождение с эталоном torch, в долях
// наибольшего значения тензора.
func closeTensors(t *testing.T, what string, tr *Trainer, got func(*weight) []float32, file string, tolerance float64) {
	t.Helper()
	want, err := ReadSafetensors(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != len(tr.weights) {
		t.Fatalf("%s: тензоров у torch %d, обучаемых на Go %d", what, len(want), len(tr.weights))
	}
	for _, p := range tr.weights {
		w := want[p.name]
		g := got(p)
		if w == nil || len(w.Data) != len(g) {
			t.Fatalf("%s: у torch нет %s той же формы", what, p.name)
		}
		var top, worst float64
		for i := range g {
			top = math.Max(top, math.Abs(float64(w.Data[i])))
			worst = math.Max(worst, math.Abs(float64(g[i])-float64(w.Data[i])))
		}
		if worst > tolerance*top {
			t.Errorf("%s %s: расхождение %.3g при наибольшем значении %.3g", what, p.name, worst, top)
		}
	}
}

// Потери, градиент каждого тензора, обрезка нормы и три шага AdamW с
// расписанием совпадают с torch (llama/train_own.py на микромодели с
// группами голов KV; эталон — testdata/train/make.py).
func TestTrainMatchesTorch(t *testing.T) {
	tr, pairs, want := loadTrainFixture(t)
	targets := 0
	for _, p := range pairs {
		targets += len(p.ids) - 1
	}
	var all, ans float64
	answers := 0
	for _, p := range pairs {
		a, b, c := tr.pairLoss(p.ids, p.start, true, 1/float64(targets))
		tr.backward(p.ids[:len(p.ids)-1])
		all, ans, answers = all+a, ans+b, answers+c
	}
	if got := all / float64(targets); math.Abs(got-want.Loss) > 2e-5 {
		t.Fatalf("потери %.6f, у torch %.6f", got, want.Loss)
	}
	if got := ans / float64(answers); math.Abs(got-want.Answer) > 2e-5 {
		t.Fatalf("потери на ответе %.6f, у torch %.6f", got, want.Answer)
	}
	closeTensors(t, "градиент", tr, func(p *weight) []float32 { return p.g }, "testdata/train/grads.safetensors", 2e-4)
	if norm := tr.apply(0); math.Abs(norm-want.Norm) > 1e-3 {
		t.Fatalf("норма градиента %.4f, у torch %.4f", norm, want.Norm)
	}

	tr, pairs, _ = loadTrainFixture(t)
	batch := []int{0, 1, 2}
	for step := 0; step < want.Steps; step++ {
		loss, n, err := tr.trainStep(context.Background(), pairs, batch, want.LR*lrFactor(step, want.Steps))
		if err != nil || n != targets {
			t.Fatalf("шаг %d: %v, целей %d", step, err, n)
		}
		if math.Abs(loss-want.Losses[step]) > 2e-4 {
			t.Fatalf("шаг %d: потери %.6f, у torch %.6f", step, loss, want.Losses[step])
		}
	}
	closeTensors(t, "веса после шагов", tr, func(p *weight) []float32 { return p.w }, "testdata/train/after.safetensors", 2e-4)
}

// Прямой проход обучения — тот же счёт, что у модели при ответе.
func TestTrainForwardIsInference(t *testing.T) {
	for _, dir := range []string{"testdata/train", "testdata/micro"} {
		net, err := LoadNet(dir)
		if err != nil {
			t.Fatal(err)
		}
		tr := NewTrainer(net)
		ids := []int{1, 7, 19, 4, 33, 12, 8, 41, 5, 22, 3}
		tp := tr.forward(ids)
		for k := 1; k <= len(ids); k++ {
			want, err := net.Probs(ids[:k])
			if err != nil {
				t.Fatal(err)
			}
			row := tp.logits[(k-1)*tr.VocabSize : k*tr.VocabSize]
			var sum float64
			for _, l := range row {
				sum += math.Exp(float64(l))
			}
			for v, l := range row {
				if got := math.Exp(float64(l)) / sum; math.Abs(got-want[v]) > 1e-6 {
					t.Fatalf("%s: позиция %d, токен %d — %.7f, у модели %.7f", dir, k, v, got, want[v])
				}
			}
		}
	}
}

// trainCorpus — пары корпуса с токенами словарём модели каталога dir.
func trainCorpus(t *testing.T, dir string, count int) []Pair {
	t.Helper()
	tok, err := LoadTokenizer(dir)
	if err != nil {
		t.Fatal(err)
	}
	pairs, _ := Corpus(CorpusOptions{Count: count, Seed: 3, Test: 0.2, H3: 0.5})
	for i := range pairs {
		if err := pairs[i].Tokenize(tok); err != nil {
			t.Fatal(err)
		}
	}
	return pairs
}

// newModelDir — словарь и конфигурация крошечной сети без весов, как после
// ochan llm init.
func newModelDir(t *testing.T) string {
	t.Helper()
	dir := testModel(t, nil)
	if err := os.Remove(filepath.Join(dir, weightsFile)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func weightsOf(t *testing.T, dir string) map[string]*Tensor {
	t.Helper()
	w, err := ReadSafetensors(filepath.Join(dir, weightsFile))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// Обучение с нуля: потери на отложенных падают, каталог — рабочая модель,
// состояние обучения после конца не остаётся.
func TestTrainFromScratch(t *testing.T) {
	dir := newModelDir(t)
	pairs := trainCorpus(t, dir, 24)
	var log []string
	res, err := Train(context.Background(), TrainOptions{Dir: dir, Pairs: pairs, Epochs: 8, BatchTokens: 2500, LR: 1e-2, Seed: 7,
		Log: func(line string) { log = append(log, line) }})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(log, "\n")
	if res.Done != res.Steps || res.Steps < 6 || !res.Measured || !(res.After[0] < res.Before[0]-0.5) || !(res.After[1] < res.Before[1]) {
		t.Fatalf("обучение: %+v\n%s", res, text)
	}
	for _, part := range []string{"Пар: обучение ", "Потери на отложенных до обучения: пара ", "Потери на отложенных после обучения: пара ",
		"Сохранено: " + dir} {
		if !strings.Contains(text, part) {
			t.Fatalf("нет строки %q:\n%s", part, text)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, trainMomentsFile)); err == nil {
		t.Fatal("моменты AdamW остались после конца обучения")
	}
	m, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if a := generate(t, m, engineTask); a.Refused || !strings.Contains(a.Text, "set Approval.limit = 200") {
		t.Fatalf("обученная модель: %+v", a)
	}
	// Отказы: корпус без токенов, чужой словарь, квантованный каталог.
	if _, err := Train(context.Background(), TrainOptions{Dir: dir, Pairs: []Pair{{Task: "x", Answer: "y"}}, Epochs: 1,
		BatchTokens: 100, LR: 1e-3}); err == nil || !strings.Contains(err.Error(), "нет токенов") {
		t.Fatalf("корпус без токенов: %v", err)
	}
	if _, err := Train(context.Background(), TrainOptions{Dir: dir, Pairs: []Pair{{IDs: []int{1, 99999, 3}, Start: 1}}, Epochs: 1,
		BatchTokens: 100, LR: 1e-3}); err == nil || !strings.Contains(err.Error(), "вне словаря") {
		t.Fatalf("чужой словарь: %v", err)
	}
}

// Прерванное обучение с Resume даёт те же веса, что непрерывное: до бита.
func TestTrainResume(t *testing.T) {
	whole := newModelDir(t)
	pairs := trainCorpus(t, whole, 24)
	opts := TrainOptions{Pairs: pairs, Epochs: 2, BatchTokens: 2500, LR: 3e-3, Seed: 7, SaveEvery: 2}
	opts.Dir = whole
	if _, err := Train(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	parts := newModelDir(t)
	opts.Dir = parts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Первое сохранение — на шаге 2; сразу после него обучение прерывается.
	opts.Log = func(line string) {
		if strings.Contains(line, "отложенные") {
			cancel()
		}
	}
	res, err := Train(ctx, opts)
	if err != nil || !res.Interrupted || res.Done != 2 {
		t.Fatalf("прерывание: %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(parts, trainMomentsFile)); err != nil {
		t.Fatalf("состояние прерванного обучения: %v", err)
	}
	opts.Log, opts.Resume = nil, true
	other := opts
	other.LR = 1e-3
	if _, err := Train(context.Background(), other); err == nil || !strings.Contains(err.Error(), "сохранено другое обучение") {
		t.Fatalf("продолжение с другими параметрами: %v", err)
	}
	res, err = Train(context.Background(), opts)
	if err != nil || res.Interrupted || res.Done != res.Steps {
		t.Fatalf("продолжение: %+v, %v", res, err)
	}
	a, b := weightsOf(t, whole), weightsOf(t, parts)
	for name, w := range a {
		for i, v := range w.Data {
			if b[name].Data[i] != v {
				t.Fatalf("%s[%d]: после продолжения %g, без прерывания %g", name, i, b[name].Data[i], v)
			}
		}
	}
}

// Дообучение: веса и словарь готовой модели (в том числе квантованной) — в
// новый каталог, параметры колец переходят с ними, прежняя модель цела.
func TestTrainInit(t *testing.T) {
	base := testModel(t, DefaultParams())
	pairs := trainCorpus(t, base, 12)
	before := weightsOf(t, base)["model.norm.weight"].Data[0]
	quant := filepath.Join(t.TempDir(), "int8")
	if _, err := Quantize(base, quant, 8, 0); err != nil {
		t.Fatal(err)
	}
	opts := TrainOptions{Pairs: pairs, Epochs: 1, BatchTokens: 2500, LR: 1e-3, Seed: 7}
	for _, init := range []string{base, quant} {
		opts.Init, opts.Dir = init, filepath.Join(t.TempDir(), "next")
		res, err := Train(context.Background(), opts)
		if err != nil || res.Done != res.Steps {
			t.Fatalf("дообучение с %s: %+v, %v", init, res, err)
		}
		// Версии параметров колец есть у полной модели: квантование их не переносит.
		if IsQuantized(opts.Dir) || !fileExists(filepath.Join(opts.Dir, ParamsFile)) ||
			init == base && !fileExists(filepath.Join(opts.Dir, VersionsDir, versionsLog)) {
			t.Fatalf("каталог модели, дообученной с %s: квантован %v", init, IsQuantized(opts.Dir))
		}
		if _, err := Load(opts.Dir, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := Train(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "уже с весами") {
			t.Fatalf("дообучение поверх готовых весов: %v", err)
		}
	}
	if weightsOf(t, base)["model.norm.weight"].Data[0] != before {
		t.Fatal("дообучение изменило прежнюю модель")
	}
	opts.Dir = base
	opts.Init = base
	if _, err := Train(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "новым каталогом") {
		t.Fatalf("дообучение в тот же каталог: %v", err)
	}
	opts.Init, opts.Dir = "", quant
	if _, err := Train(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "квантованы") {
		t.Fatalf("обучение квантованного каталога: %v", err)
	}
}
