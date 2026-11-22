package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"ochan/own"

	"github.com/spf13/cobra"
)

// Обучение весов сети — в ochan, на процессоре: тот же счёт, что у torch в
// llama/train_own.py (он остаётся для видеокарты), сверенный с ним на
// градиентах и шагах AdamW.

var (
	llmTrainCorpus string
	llmTrainInit   string
	llmTrainEpochs float64
	llmTrainBatch  int
	llmTrainLR     float64
	llmTrainSeed   int64
	llmTrainSave   int
	llmTrainEval   int
	llmTrainResume bool
)

var llmTrainCmd = &cobra.Command{
	Use:   "train --corpus корпус.jsonl [--model каталог] [--init каталог] [--epochs 8] [--lr 1e-3] [--resume]",
	Short: "Обучить веса сети своей модели: градиентный спуск на процессоре",
	Long: "Градиентный спуск по токенам корпуса (ochan llm corpus --model): потери —\n" +
		"по всей паре <bos> задача <answer> ответ <eos>, батч — по числу токенов\n" +
		"(--batch-tokens, пары близкой длины вместе), AdamW, обрезка нормы\n" +
		"градиента, разгон и косинусный спад скорости. Считает процессор, всеми\n" +
		"ядрами: Python и видеокарта не нужны. Веса пишутся в каталог модели\n" +
		"(model.safetensors) — это и есть llm.model.\n\n" +
		"--init — дообучение: веса и словарь готовой модели, полной или\n" +
		"квантованной (стартовая — ochan llm init --pretrained), например на парах\n" +
		"кластеров инцидентов с примесью базового корпуса (ochan llm learn\n" +
		"--pairs). Новые веса пишутся в --model — новый каталог; прежний остаётся\n" +
		"для отката, параметры колец и их версии переходят с весами.\n\n" +
		"Каждые --save-every шагов и при прерывании (Ctrl+C) каталог — рабочая\n" +
		"модель и состояние обучения: --resume продолжает с него с теми же\n" +
		"параметрами и корпусом, и результат тот же, что без прерывания.\n\n" +
		"Дообучение на сотнях пар — минуты. Обучение с нуля на десятках тысяч пар\n" +
		"на процессоре — дни: тот же счёт на видеокарте делает\n" +
		"llama/train_own.py, каталог модели и корпус у них общие.",
	Args: cobra.NoArgs,
	Example: "ochan llm train --init ~/models/own --model ~/models/own-2 --corpus pairs.jsonl --epochs 1 --lr 1e-4\n" +
		"ochan llm train --model ~/models/new --corpus corpus.jsonl\n" +
		"ochan llm train --model ~/models/new --corpus corpus.jsonl --resume",
	RunE: func(cmd *cobra.Command, _ []string) error {
		if llmTrainCorpus == "" {
			return errors.New("--corpus: корпус с токенами (ochan llm corpus --model <каталог модели> --out корпус.jsonl)")
		}
		dir := expandHome(strings.TrimSpace(llmModelFlag))
		init := expandHome(strings.TrimSpace(llmTrainInit))
		switch {
		case init != "" && dir == "":
			return errors.New("--init — дообучение в новый каталог: задайте его в --model")
		case dir == "":
			dir = llmModelDir()
		}
		if dir == "" {
			return errors.New("каталог своей модели: --model или llm.model в config.yaml")
		}
		pairs, err := readPairs(expandHome(llmTrainCorpus))
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		res, err := own.Train(ctx, own.TrainOptions{Dir: dir, Init: init, Pairs: pairs, Epochs: llmTrainEpochs,
			BatchTokens: llmTrainBatch, LR: llmTrainLR, Seed: llmTrainSeed, SaveEvery: llmTrainSave, EvalPairs: llmTrainEval,
			Resume: llmTrainResume, Log: func(line string) { cmd.Println(line) }})
		if err != nil {
			return err
		}
		// Модель в памяти процесса — прежняя: следующая команда загрузит новые веса.
		loadedMu.Lock()
		delete(loaded, dir)
		loadedMu.Unlock()
		if res.Interrupted {
			return fmt.Errorf("обучение прервано на шаге %d из %d: ochan llm train с теми же параметрами и --resume продолжит",
				res.Done, res.Steps)
		}
		return nil
	},
}

func init() {
	llmTrainCmd.Flags().StringVar(&llmModelFlag, "model", "", "каталог модели, куда пишутся веса; без флага — llm.model")
	llmTrainCmd.Flags().StringVar(&llmTrainInit, "init", "", "дообучение: начать с весов этой модели; --model — новый каталог")
	llmTrainCmd.Flags().StringVar(&llmTrainCorpus, "corpus", "", "корпус с токенами: ochan llm corpus --model или llm learn --pairs")
	llmTrainCmd.Flags().Float64Var(&llmTrainEpochs, "epochs", 8, "проходов по корпусу")
	llmTrainCmd.Flags().IntVar(&llmTrainBatch, "batch-tokens", 32768, "токенов в батче с выравниванием по самой длинной паре")
	llmTrainCmd.Flags().Float64Var(&llmTrainLR, "lr", 1e-3, "скорость обучения; для дообучения — меньше (1e-4)")
	llmTrainCmd.Flags().Int64Var(&llmTrainSeed, "seed", 7, "начало случайной последовательности: тот же seed — те же веса")
	llmTrainCmd.Flags().IntVar(&llmTrainSave, "save-every", 500, "шагов между сохранениями")
	llmTrainCmd.Flags().IntVar(&llmTrainEval, "eval-pairs", 200, "сколько отложенных пар мерить; 0 — все")
	llmTrainCmd.Flags().BoolVar(&llmTrainResume, "resume", false, "продолжить с сохранения в --model")
	llmCmd.AddCommand(llmTrainCmd)
}
