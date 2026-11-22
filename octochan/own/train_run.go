package own

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// Цикл обучения — тот же, что в llama/train_own.py: потери по всей паре
// <bos> задача <answer> ответ <eos>, батч по числу токенов из пар близкой
// длины, AdamW с убыванием весов матриц, обрезка нормы градиента, разгон и
// косинусный спад скорости до десятой доли. Каталог модели после обучения —
// рабочая модель для ochan; torch читает его так же.

// TrainOptions — обучение весов сети.
type TrainOptions struct {
	Dir         string  // каталог модели: сюда пишутся веса
	Init        string  // дообучение: веса и словарь этой модели; Dir — новый каталог
	Pairs       []Pair  // корпус с токенами (ochan llm corpus --model)
	Epochs      float64 // проходов по корпусу
	BatchTokens int     // токенов в батче с выравниванием по самой длинной паре
	LR          float64
	Seed        int64
	SaveEvery   int  // шагов между сохранениями
	EvalPairs   int  // сколько отложенных пар мерить; 0 — все
	Resume      bool // продолжить с сохранения в Dir
	Log         func(line string)
}

// TrainResult — что вышло из обучения.
type TrainResult struct {
	Steps, Done   int        // шагов всего и сделано
	Before, After [2]float64 // потери на отложенных до и после: пара, ответ
	Measured      bool       // отложенные пары были
	Interrupted   bool       // прервано: состояние сохранено, Resume продолжит
}

const (
	trainStateFile   = "train_state.json"
	trainMomentsFile = "train_state.moments"
	weightsFile      = "model.safetensors"
)

// trainState — где остановилось обучение и с какими параметрами оно шло.
type trainState struct {
	Step        int     `json:"step"`
	Steps       int     `json:"steps"`
	Pairs       int     `json:"pairs"`
	BatchTokens int     `json:"batch_tokens"`
	Epochs      float64 `json:"epochs"`
	LR          float64 `json:"lr"`
	Seed        int64   `json:"seed"`
}

type example struct {
	ids   []int
	start int
}

// trainExamples — пары части split с токенами; длиннее контекста — пропуск.
func trainExamples(pairs []Pair, split string, context, vocab int) (out []example, skipped int, err error) {
	for n, p := range pairs {
		s := p.Split
		if s == "" {
			s = "train"
		}
		if s != split {
			continue
		}
		if len(p.IDs) < 2 || p.Start < 1 || p.Start >= len(p.IDs) {
			return nil, 0, fmt.Errorf("пара %d: у пары нет токенов — корпус ochan llm corpus --model <каталог модели>", n+1)
		}
		for _, id := range p.IDs {
			if id < 0 || id >= vocab {
				return nil, 0, fmt.Errorf("пара %d: токен %d вне словаря модели (%d) — корпус другого словаря", n+1, id, vocab)
			}
		}
		if context > 0 && len(p.IDs) > context {
			skipped++
			continue
		}
		out = append(out, example{p.IDs, p.Start})
	}
	return out, skipped, nil
}

// trainBatches — пары близкой длины вместе; в батче не больше budget
// токенов с выравниванием по самой длинной. Состав по длинам от rng не
// зависит — только порядок батчей и пар равной длины.
func trainBatches(data []example, budget int, rng *rand.Rand) [][]int {
	order, keys := make([]int, len(data)), make([]float64, len(data))
	for i := range order {
		order[i], keys[i] = i, rng.Float64()
	}
	sort.Slice(order, func(a, b int) bool {
		la, lb := len(data[order[a]].ids), len(data[order[b]].ids)
		if la != lb {
			return la < lb
		}
		return keys[order[a]] < keys[order[b]]
	})
	var out [][]int
	var batch []int
	longest := 0
	for _, k := range order {
		n := len(data[k].ids)
		if len(batch) > 0 && max(longest, n)*(len(batch)+1) > budget {
			out = append(out, batch)
			batch, longest = nil, 0
		}
		batch = append(batch, k)
		longest = max(longest, n)
	}
	if len(batch) > 0 {
		out = append(out, batch)
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// lrFactor — доля скорости на шаге s из steps: разгон, затем косинусный
// спад до десятой доли.
func lrFactor(s, steps int) float64 {
	warmup := max(1, min(500, steps/20))
	return math.Min(1, float64(s+1)/float64(warmup)) *
		(0.1 + 0.9*0.5*(1+math.Cos(math.Pi*math.Min(1, float64(s)/float64(steps)))))
}

// trainStep — батч: градиент среднего по всем целям батча и шаг AdamW.
// Возвращает средние потери батча и число целей.
func (t *Trainer) trainStep(ctx context.Context, data []example, batch []int, lr float64) (float64, int, error) {
	targets := 0
	for _, k := range batch {
		targets += len(data[k].ids) - 1
	}
	var total float64
	for _, k := range batch {
		if err := ctx.Err(); err != nil {
			t.zeroGrad()
			return 0, 0, err
		}
		e := data[k]
		all, _, _ := t.pairLoss(e.ids, e.start, true, 1/float64(targets))
		t.backward(e.ids[:len(e.ids)-1])
		total += all
	}
	t.apply(lr)
	return total / float64(targets), targets, nil
}

// evaluate — средние потери на парах: по всей паре и по ответу. Среднее —
// по батчам с весом числа пар, как в train_own.py.
func (t *Trainer) evaluate(ctx context.Context, data []example, budget int) ([2]float64, error) {
	var pair, answer float64
	n := 0
	for _, batch := range trainBatches(data, budget, rand.New(rand.NewSource(0))) {
		var all, ans float64
		targets, answers := 0, 0
		for _, k := range batch {
			if err := ctx.Err(); err != nil {
				return [2]float64{}, err
			}
			a, b, c := t.pairLoss(data[k].ids, data[k].start, false, 0)
			all, ans, targets, answers = all+a, ans+b, targets+len(data[k].ids)-1, answers+c
		}
		pair += all / float64(targets) * float64(len(batch))
		answer += ans / float64(max(answers, 1)) * float64(len(batch))
		n += len(batch)
	}
	return [2]float64{pair / float64(max(n, 1)), answer / float64(max(n, 1))}, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// hasWeights — в каталоге уже лежат веса: полные или квантованные.
func hasWeights(dir string) bool {
	return fileExists(filepath.Join(dir, weightsFile)) || fileExists(filepath.Join(dir, weightsFile+".index.json")) ||
		IsQuantized(dir)
}

func sameDir(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(ia, ib)
	}
	x, _ := filepath.Abs(a)
	y, _ := filepath.Abs(b)
	return filepath.Clean(x) == filepath.Clean(y)
}

// save — веса и состояние обучения: каталог — рабочая модель, Resume
// продолжит с этого шага. Файлы пишутся рядом и переименовываются: обрыв
// посреди записи прежних не портит.
func (t *Trainer) save(dir string, st trainState) error {
	replace := func(name string, write func(path string) error) error {
		tmp := filepath.Join(dir, name+".tmp")
		if err := write(tmp); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(dir, name))
	}
	if err := replace(weightsFile, func(path string) error { return WriteSafetensors(path, t.tensors()) }); err != nil {
		return err
	}
	if st.Step >= st.Steps {
		// Обучение кончено: моменты AdamW больше не нужны.
		if err := os.Remove(filepath.Join(dir, trainMomentsFile)); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		moments := map[string]*Tensor{}
		for _, p := range t.weights {
			moments["m."+p.name] = &Tensor{Shape: []int{len(p.m)}, Data: p.m, DType: "F32"}
			moments["v."+p.name] = &Tensor{Shape: []int{len(p.v)}, Data: p.v, DType: "F32"}
		}
		if err := replace(trainMomentsFile, func(path string) error { return WriteSafetensors(path, moments) }); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return replace(trainStateFile, func(path string) error { return os.WriteFile(path, append(data, '\n'), 0o644) })
}

// restore — моменты AdamW и счёт шагов сохранённого обучения.
func (t *Trainer) restore(dir string, st trainState) error {
	moments, err := ReadSafetensors(filepath.Join(dir, trainMomentsFile))
	if err != nil {
		return fmt.Errorf("состояние обучения: %w", err)
	}
	for _, p := range t.weights {
		m, v := moments["m."+p.name], moments["v."+p.name]
		if m == nil || v == nil || len(m.Data) != len(p.m) || len(v.Data) != len(p.v) {
			return fmt.Errorf("состояние обучения: моменты %s не от этой сети", p.name)
		}
		copy(p.m, m.Data)
		copy(p.v, v.Data)
	}
	t.steps = st.Step
	return nil
}

// Train учит веса сети по парам корпуса и пишет их в каталог модели.
func Train(ctx context.Context, o TrainOptions) (TrainResult, error) {
	var res TrainResult
	say := func(format string, args ...any) {
		if o.Log != nil {
			o.Log(fmt.Sprintf(format, args...))
		}
	}
	switch {
	case o.Dir == "":
		return res, errors.New("каталог модели не задан")
	case !(o.Epochs > 0):
		return res, errors.New("--epochs: больше нуля")
	case o.BatchTokens < 2:
		return res, errors.New("--batch-tokens: не меньше двух токенов")
	case !(o.LR > 0):
		return res, errors.New("--lr: больше нуля")
	}
	if o.SaveEvery <= 0 {
		o.SaveEvery = 500
	}
	out := o.Dir
	var saved trainState
	resume := false
	if o.Resume {
		if data, err := os.ReadFile(filepath.Join(out, trainStateFile)); err == nil {
			if err := json.Unmarshal(data, &saved); err != nil {
				return res, fmt.Errorf("%s: %w", trainStateFile, err)
			}
			resume = true
		}
	}
	if o.Init != "" {
		if sameDir(o.Init, out) {
			return res, errors.New("--model должен быть новым каталогом: прежние веса остаются для отката")
		}
		if !IsModel(o.Init) {
			return res, fmt.Errorf("%s — не своя модель ℒ: нет %s", o.Init, TokenizerFile)
		}
		if !resume {
			if hasWeights(out) {
				return res, fmt.Errorf("%s уже с весами: новые веса — в новый каталог, прежние остаются для отката", out)
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				return res, err
			}
			for _, name := range []string{ConfigFile, TokenizerFile, ParamsFile} {
				if err := copyFile(filepath.Join(o.Init, name), filepath.Join(out, name)); err != nil && !os.IsNotExist(err) {
					return res, err
				}
			}
			if err := copyDir(filepath.Join(o.Init, VersionsDir), filepath.Join(out, VersionsDir)); err != nil {
				return res, err
			}
		}
	}
	if !fileExists(filepath.Join(out, TokenizerFile)) || !fileExists(filepath.Join(out, ConfigFile)) {
		return res, fmt.Errorf("%s: нет словаря и конфигурации — ochan llm init --out %s", out, out)
	}
	cfg, err := LoadConfig(out)
	if err != nil {
		return res, err
	}
	tok, err := LoadTokenizer(out)
	if err != nil {
		return res, err
	}
	if tok.Len() != cfg.VocabSize {
		return res, fmt.Errorf("словарь %d токенов, а выход сети — %d: config.json от другого словаря", tok.Len(), cfg.VocabSize)
	}
	train, skipped, err := trainExamples(o.Pairs, "train", cfg.MaxPositions, cfg.VocabSize)
	if err != nil {
		return res, err
	}
	test, _, err := trainExamples(o.Pairs, "test", cfg.MaxPositions, cfg.VocabSize)
	if err != nil {
		return res, err
	}
	tokens := 0
	for _, e := range train {
		tokens += len(e.ids)
	}
	say("Пар: обучение %d, отложено %d, длиннее контекста %d; токенов в паре ~%d, словарь %d", len(train), len(test),
		skipped, tokens/max(1, len(train)), cfg.VocabSize)
	if len(train) == 0 {
		return res, errors.New("пар для обучения нет")
	}
	measured := test
	if o.EvalPairs > 0 && len(test) > o.EvalPairs {
		measured = make([]example, o.EvalPairs)
		for i := range measured {
			measured[i] = test[i*len(test)/o.EvalPairs]
		}
	}

	var net *Net
	switch {
	case resume:
		if IsQuantized(out) {
			return res, fmt.Errorf("%s: веса квантованы — продолжать нечего", out)
		}
		net, err = LoadNet(out)
	case o.Init != "":
		// Квантованные веса распаковываются: дообучение идёт с float32.
		net, err = LoadNet(o.Init)
	case IsQuantized(out):
		return res, fmt.Errorf("%s: веса квантованы — дообучение в новый каталог: --init %s --model <новый каталог>", out, out)
	case hasWeights(out):
		net, err = LoadNet(out)
	default:
		net, err = newNet(cfg, randomWeights(cfg, o.Seed))
	}
	if err != nil {
		return res, err
	}
	t := NewTrainer(net)
	// Рабочая память — сразу под самую длинную пару.
	longest := 0
	for _, e := range append(append([]example(nil), train...), measured...) {
		longest = max(longest, len(e.ids)-1)
	}
	t.tapeFor(longest)
	say("Модель: %.1f млн параметров; считает процессор, ядер %d", float64(net.Params())/1e6, runtime.GOMAXPROCS(0))

	perEpoch := len(trainBatches(train, o.BatchTokens, rand.New(rand.NewSource(0))))
	steps := max(1, int(math.Ceil(float64(perEpoch)*o.Epochs)))
	state := trainState{Steps: steps, Pairs: len(train), BatchTokens: o.BatchTokens, Epochs: o.Epochs, LR: o.LR, Seed: o.Seed}
	res.Steps = steps
	first := 0
	if resume {
		same := saved
		same.Step = 0
		if same != state {
			return res, fmt.Errorf("%s: сохранено другое обучение (пар %d, шагов %d, --batch-tokens %d, --epochs %g, --lr %g, --seed %d) — "+
				"--resume продолжает с теми же параметрами и корпусом", out, saved.Pairs, saved.Steps, saved.BatchTokens, saved.Epochs,
				saved.LR, saved.Seed)
		}
		first = saved.Step
		if first < steps {
			if err := t.restore(out, saved); err != nil {
				return res, err
			}
		}
		say("Продолжение с шага %d/%d", first, steps)
	}
	evaluate := func() ([2]float64, error) {
		if len(measured) == 0 {
			return [2]float64{math.NaN(), math.NaN()}, nil
		}
		return t.evaluate(ctx, measured, o.BatchTokens)
	}
	of := ""
	if len(measured) < len(test) {
		of = fmt.Sprintf(" (по %d парам из %d)", len(measured), len(test))
	}
	res.Measured = len(measured) > 0
	if !res.Measured {
		say("Отложенных пар нет: потери на них не меряются")
	} else if !resume {
		if res.Before, err = evaluate(); err != nil {
			return res, err
		}
		say("Потери на отложенных до обучения%s: пара %.3f, ответ %.3f", of, res.Before[0], res.Before[1])
	}

	interrupted := func(step int) (TrainResult, error) {
		state.Step = step
		if err := t.save(out, state); err != nil {
			return res, err
		}
		res.Done, res.Interrupted = step, true
		say("Прервано на шаге %d/%d: веса и состояние сохранены — --resume продолжит", step, steps)
		return res, nil
	}
	started, lastLog := time.Now(), time.Now()
	var running []float64
	var queue [][]int
	epoch, seen := -1, 0
	for step := first; step < steps; step++ {
		if e := step / perEpoch; e != epoch {
			epoch = e
			queue = trainBatches(train, o.BatchTokens, rand.New(rand.NewSource(o.Seed+int64(e+1)*1_000_003)))
		}
		loss, n, err := t.trainStep(ctx, train, queue[step%perEpoch], o.LR*lrFactor(step, steps))
		if err != nil {
			return interrupted(step)
		}
		running, seen = append(running, loss), seen+n
		if (step+1)%100 == 0 || step+1 == steps || time.Since(lastLog) >= 30*time.Second {
			var mean float64
			for _, l := range running {
				mean += l
			}
			elapsed := time.Since(started)
			line := fmt.Sprintf("шаг %d/%d (эпоха %.1f): потери %.3f, %.0f с, %.0f токенов/с", step+1, steps,
				float64(step+1)/float64(perEpoch), mean/float64(len(running)), elapsed.Seconds(), float64(seen)/elapsed.Seconds())
			if left := steps - step - 1; left > 0 {
				eta := time.Duration(float64(elapsed) / float64(step+1-first) * float64(left))
				line += ", осталось ~" + eta.Round(time.Second).String()
			}
			say("%s", line)
			running, lastLog = nil, time.Now()
		}
		if (step+1)%o.SaveEvery == 0 && step+1 < steps {
			state.Step = step + 1
			if err := t.save(out, state); err != nil {
				return res, err
			}
			if res.Measured {
				mid, err := evaluate()
				if err != nil {
					return interrupted(step + 1)
				}
				say("  отложенные%s: пара %.3f, ответ %.3f", of, mid[0], mid[1])
			}
		}
	}
	state.Step = steps
	if err := t.save(out, state); err != nil {
		return res, err
	}
	res.Done = steps
	if res.Measured {
		if res.After, err = evaluate(); err != nil {
			return res, err
		}
		say("Потери на отложенных после обучения%s: пара %.3f, ответ %.3f", of, res.After[0], res.After[1])
	}
	say("Сохранено: %s — модель для ochan: llm.model: %s", out, out)
	return res, nil
}

// copyDir — файлы каталога from в to; нет from — нечего копировать.
func copyDir(from, to string) error {
	entries, err := os.ReadDir(from)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if err := copyFile(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
