package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ochan/own"

	"github.com/spf13/cobra"
)

// Своя модель целиком в ochan: словарь (llm init), корпус (llm corpus),
// параметры колец по журналу и инцидентам (llm fit, llm learn),
// квантование (llm quantize). Обучение весов сети — llama/train_own.py:
// единственное, что остаётся вне ochan, потому что считается на видеокарте.

// describeModel — модель одной строкой: каталог, размер, словарь, веса,
// версия параметров колец.
func describeModel(m *own.Model) string {
	weights := "float32"
	if bits := own.QuantBits(m.Dir); bits > 0 {
		weights = fmt.Sprintf("int%d", bits)
	}
	line := fmt.Sprintf("%s: %.1f млн параметров, словарь %d, контекст %d, веса %s %.1f МБ", m.Dir,
		float64(m.Net.Params())/1e6, m.Tok.Len(), m.Net.MaxPositions, weights, float64(own.WeightsSize(m.Dir))/(1<<20))
	if v, err := own.Versions(m.Dir); err == nil && len(v) > 0 {
		line += fmt.Sprintf("; параметры колец — версия %d", v[len(v)-1].Version)
	}
	return line
}

// modelDir — каталог модели: --model или llm.model.
func modelDir(flag string) (string, error) {
	dir := expandHome(strings.TrimSpace(flag))
	if dir == "" {
		dir = llmModelDir()
	}
	if dir == "" {
		return "", errors.New("каталог своей модели: --model или llm.model в config.yaml")
	}
	if !own.IsModel(dir) {
		return "", fmt.Errorf("%s — не своя модель ℒ: нет %s (ochan llm init)", dir, own.TokenizerFile)
	}
	return dir, nil
}

var (
	llmModelFlag   string
	llmInitSize    string
	llmInitContext int
	llmInitOut     string
	llmInitRandom  bool
	llmInitTrained bool
	llmInitFull    bool
)

var llmInitCmd = &cobra.Command{
	Use: "init --out каталог --pretrained [--full] | " +
		"init --out каталог [--size tiny|small|base] [--context 1024] [--random-weights]",
	Short: "Своя модель: стартовая с обученными весами или новая под обучение",
	Long: "--pretrained — стартовая модель: обученные веса и словарь, которые\n" +
		"доставляются вместе с ochan (они встроены в бинарник). Учить язык с нуля\n" +
		"на каждой машине незачем: модель отвечает сразу — без Python, видеокарты\n" +
		"и сети. Каталог — llm.model. Своё у каждой установки — параметры колец:\n" +
		"надёжность правил по её журналу и инцидентам (llm fit, llm learn).\n" +
		"Веса — int8; --full распаковывает их в float32 — с таких идёт дообучение\n" +
		"сети (llama/train_own.py --init).\n\n" +
		"Без --pretrained — новая модель под обучение с нуля. Её словарь — алфавит\n" +
		"ℒ (ochan fp alphabet: слова словаря, их смыслы, ключевые и служебные\n" +
		"слова, единицы, знаки, операции скиллов) с цифрами, разметкой строки и\n" +
		"переменными для имён частных случаев. Конфигурация — архитектура Llama\n" +
		"размера --size. Веса сети обучает llama/train_own.py по корпусу ochan llm\n" +
		"corpus; каталог с весами — llm.model.\n\n" +
		"--random-weights — случайные веса: модель отвечает сразу, потому что ответ\n" +
		"пишет её логика, а сеть только выбирает порядок среди допустимых. Это\n" +
		"проверка контура до обучения, а не обученная модель.",
	Args: cobra.NoArgs,
	Example: "ochan llm init --pretrained --out ~/models/own\n" +
		"ochan llm init --out ~/models/new --size small\n" +
		"ochan llm corpus --model ~/models/new --count 60000 --out corpus.jsonl\n" +
		"python llama/train_own.py --model ~/models/new --corpus corpus.jsonl",
	RunE: func(cmd *cobra.Command, _ []string) error {
		out := expandHome(strings.TrimSpace(llmInitOut))
		if out == "" {
			return errors.New("--out: каталог новой модели")
		}
		if own.IsModel(out) {
			return fmt.Errorf("%s — уже модель: новая модель — в новый каталог, прежняя остаётся", out)
		}
		if llmInitFull && !llmInitTrained {
			return errors.New("--full — для стартовой модели: вместе с --pretrained")
		}
		if llmInitTrained {
			if llmInitRandom || cmd.Flags().Changed("size") || cmd.Flags().Changed("context") {
				return errors.New("--pretrained — готовая модель: её размер, контекст и веса заданы; " +
					"--size, --context и --random-weights — для новой")
			}
			if err := own.WritePretrained(out, llmInitFull); err != nil {
				return err
			}
			m, err := own.Load(out, modulesDir())
			if err != nil {
				return err
			}
			cmd.Printf("Стартовая модель (%s): %s\n", own.PretrainedName, describeModel(m))
			cmd.Printf("Включить: в config.yaml — llm.mode: embedded и llm.model: %s; проверить — ochan llm\n", out)
			if llmInitFull {
				cmd.Printf("Веса полные: дообучение сети — python llama/train_own.py --init %s --model <новый каталог> --corpus пары.jsonl\n", out)
			}
			return nil
		}
		opts, err := fpOptions(cmd, false)
		if err != nil {
			return err
		}
		alphabet := alphabetEntries(opts)
		tok, err := own.BuildTokenizer(alphabet, true)
		if err != nil {
			return err
		}
		cfg, err := own.NewConfig(tok, llmInitSize, llmInitContext)
		if err != nil {
			return err
		}
		if err := tok.Save(out); err != nil {
			return err
		}
		if err := cfg.Save(out); err != nil {
			return err
		}
		if llmInitRandom {
			if err := own.WriteRandomWeights(out, cfg, 1); err != nil {
				return err
			}
		}
		h, inter := cfg.HiddenSize, cfg.Intermediate
		params := cfg.VocabSize*h + cfg.Layers*(4*h*h+3*h*inter+2*h) + h
		cmd.Printf("Модель %s: словарь %d токенов (алфавит ℒ — %d записей), размер %s — %.1f млн параметров, контекст %d\n",
			out, tok.Len(), len(alphabet), llmInitSize, float64(params)/1e6, cfg.MaxPositions)
		if llmInitRandom {
			cmd.Println("Веса случайные: модель отвечает сразу — ответ пишет логика, сеть только выбирает порядок; это проверка контура, не обученная модель")
		}
		cmd.Printf("Обучение: ochan llm corpus --model %s --out corpus.jsonl, затем python llama/train_own.py --model %s --corpus corpus.jsonl\n", out, out)
		return nil
	},
}

var (
	llmCorpusCount      int
	llmCorpusSeed       int64
	llmCorpusTest       float64
	llmCorpusH3         float64
	llmCorpusOut        string
	llmCorpusTasks      string
	llmCorpusTasksCount int
)

var llmCorpusCmd = &cobra.Command{
	Use:   "corpus --out корпус.jsonl [--model каталог] [--count 5000]",
	Short: "Корпус для обучения своей модели: пары задача → ответ логики ochan",
	Long: "Пара — задача на ℒ (цель, классы, отношения, приоритеты, жёсткие\n" +
		"правила, обобщения со стрелками, неточные предпосылки, скиллы) и ответ —\n" +
		"та же программа, закрытая там, где задача оставила открытым. Ответ выводит\n" +
		"логика ochan — та же, что у модели при ответе. Задача без предпосылки\n" +
		"(⊥undef) — близнец решаемой, её ответ — без решений (H3). Каждую пару\n" +
		"принимает разбор ochan; непрошедшие отбрасываются.\n\n" +
		"С --model у пары — токены словарём модели (ids, start): по ним учит\n" +
		"llama/train_own.py. Строка JSON на пару; split — train или test,\n" +
		"задача и её близнец — в одной части.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if llmCorpusOut == "" {
			return errors.New("--out: файл корпуса")
		}
		var tok *own.Tokenizer
		if llmModelFlag != "" || llmModelDir() != "" {
			dir, err := modelDir(llmModelFlag)
			if err != nil {
				return err
			}
			if tok, err = own.LoadTokenizer(dir); err != nil {
				return err
			}
		}
		opts, err := fpOptions(cmd, true)
		if err != nil {
			return err
		}
		pairs, rep := own.Corpus(own.CorpusOptions{Count: llmCorpusCount, Seed: llmCorpusSeed, Test: llmCorpusTest,
			H3: llmCorpusH3, Lexicon: opts.Lexicon})
		if tok != nil {
			for i := range pairs {
				if err := pairs[i].Tokenize(tok); err != nil {
					return fmt.Errorf("пара %d: %w — словарь модели не из этого алфавита (ochan llm init)", i, err)
				}
			}
		}
		if err := writePairs(llmCorpusOut, pairs); err != nil {
			return err
		}
		cmd.Printf("Пар: %d из %d прошли проверку (обучение %d, отложено %d); %s\n", len(pairs), rep.Generated,
			rep.Train, len(pairs)-rep.Train, countsLine(rep.Kinds))
		cmd.Printf("Знание в задачах: %s\n", countsLine(rep.Knowledge))
		for _, kv := range sortedCounts(rep.Failures) {
			cmd.Printf("  отброшено %d: %s\n", kv.n, kv.key)
		}
		if tok != nil {
			cmd.Printf("Токены — словарём модели (%d токенов): ids и start у каждой пары\n", tok.Len())
		}
		if llmCorpusTasks != "" {
			if err := os.MkdirAll(llmCorpusTasks, 0o755); err != nil {
				return err
			}
			n := 0
			for _, p := range pairs {
				if p.Split != "test" || n >= llmCorpusTasksCount {
					continue
				}
				n++
				if err := os.WriteFile(filepath.Join(llmCorpusTasks, fmt.Sprintf("test%02d.fp", n)), []byte(p.Task), 0o644); err != nil {
					return err
				}
			}
			cmd.Printf("Задачи для замера: %s (%d)\n", llmCorpusTasks, n)
		}
		cmd.Printf("Корпус: %s\n", llmCorpusOut)
		return nil
	},
}

type countKV struct {
	key string
	n   int
}

func sortedCounts(m map[string]int) []countKV {
	var out []countKV
	for k, n := range m {
		out = append(out, countKV{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].key < out[j].key
	})
	return out
}

func countsLine(m map[string]int) string {
	var parts []string
	for _, kv := range sortedCounts(m) {
		parts = append(parts, fmt.Sprintf("%s %d", kv.key, kv.n))
	}
	return strings.Join(parts, ", ")
}

func writePairs(path string, pairs []own.Pair) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, p := range pairs {
		if err := enc.Encode(p); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readPairs(path string) ([]own.Pair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []own.Pair
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\uFEFF"))
		if line == "" {
			continue
		}
		var p own.Pair
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

var llmFitJournal string

var llmFitCmd = &cobra.Command{
	Use:   "fit [--model каталог] [--journal журнал.json]",
	Short: "Параметры колец своей модели по журналу решений",
	Long: "Надёжность правил, порог well_True и условный счёт автора — из журнала\n" +
		"решений как есть; веса правила выбора — логистической регрессией по\n" +
		"исходам выборов модели, калибровка оценок — монотонной поправкой «оценка →\n" +
		"наблюдаемая доля». Пока исходов мало (меньше 20 выборов с обоими\n" +
		"исходами), веса и калибровка прежние. Кластеры инцидентов пересчёт по\n" +
		"журналу не трогает.\n\n" +
		"Журнал — этой машины; --journal — выгрузка ochan fp journal export\n" +
		"другой. Результат — rings.json рядом с весами, новая версия в\n" +
		"rings.versions/ (откат — ochan llm learn --rollback).",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := modelDir(llmModelFlag)
		if err != nil {
			return err
		}
		var j own.Journal
		source := "журнал этой машины"
		if llmFitJournal != "" {
			data, err := os.ReadFile(llmFitJournal)
			if err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(string(data), "\uFEFF")), &j); err != nil {
				return fmt.Errorf("%s: %w", llmFitJournal, err)
			}
			source = filepath.Base(llmFitJournal)
		} else {
			exp, err := journalExportData()
			if err != nil {
				return err
			}
			data, _ := json.Marshal(exp)
			if err := json.Unmarshal(data, &j); err != nil {
				return err
			}
		}
		params, err := own.LoadParams(dir)
		if err != nil {
			return err
		}
		res := params.FitJournal(j)
		w := params.ChoiceWeights()
		if math.IsNaN(w.Bias + w.LogProb + w.Lower + w.Width + w.Binding) {
			return errors.New("веса правила выбора не сошлись — параметры не записаны")
		}
		version, err := params.Save(dir, "журнал решений: "+source)
		if err != nil {
			return err
		}
		cmd.Printf("Версия параметров %d (откат — ochan llm learn --rollback)\n", version)
		cmd.Printf("Правил %d, выборов с исходом %d, порог well_True %g, условный счёт %g/%g\n",
			len(params.Rules), res.Choices, params.WellTrue, params.Prior[0], params.Prior[1])
		for _, rule := range sortedRuleNames(params.Rules) {
			c, r := params.Rules[rule], params.Rule(rule, nil)
			cmd.Printf("  %-20s верных %6.2f из %6.2f  надёжность [%.2f; %.2f]\n", rule, c[0], c[1], r.L, r.U)
		}
		if res.Weights {
			cmd.Printf("Веса правила выбора: β0 %.3f, β1 %.3f, β2 %.3f, β3 %.3f, β4 %.3f\n", w.Bias, w.LogProb, w.Lower, w.Width, w.Binding)
			var pts []string
			for _, p := range params.Calibration {
				pts = append(pts, fmt.Sprintf("%.2f → %.2f", p[0], p[1]))
			}
			cmd.Printf("Калибровка: %s\n", strings.Join(pts, ", "))
		} else {
			cmd.Println("Веса и калибровка прежние: исходов выбора мало или все одинаковые")
		}
		return nil
	},
}

func sortedRuleNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	llmLearnClusters    string
	llmLearnPairs       string
	llmLearnReplay      string
	llmLearnReplayShare float64
	llmLearnVersions    bool
	llmLearnRollback    string
	llmLearnPackage     string
	llmLearnVersion     string
	llmLearnDescription string
)

var llmLearnCmd = &cobra.Command{
	Use:   "learn [--model каталог] [--clusters файл] | --versions | --rollback [N] | --package каталог --version V",
	Short: "Своя модель учится на повторяющихся инцидентах; версии параметров и откат",
	Long: "Созревшие кластеры инцидентов (ochan incident learn) становятся знанием\n" +
		"модели: обобщение — факты структуры, которые есть больше чем у половины\n" +
		"инцидентов кластера; правила плана, которым модель решает задачу\n" +
		"инцидента, получают его исход — решила сама или отдала инженерам. Задачу\n" +
		"той же структуры модель узнаёт сама: надёжность правил на ней — по этим\n" +
		"исходам, а плохо решённая структура — вопрос оператору. Кластеры\n" +
		"пересчитываются целиком; каждая запись — новая версия rings.json.\n\n" +
		"Инциденты — этой машины; --clusters — выгрузка ochan incident learn\n" +
		"--export другой. --pairs — пары для дообучения сети (train_own.py --init):\n" +
		"задачи, которые модель решила сама, с примесью базового корпуса --replay.\n" +
		"--package — знания модели пакетом реестра для других машин: обобщения и\n" +
		"надёжность правил на них, без номеров инцидентов.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := modelDir(llmModelFlag)
		if err != nil {
			return err
		}
		switch {
		case llmLearnVersions:
			log, err := own.Versions(dir)
			if err != nil {
				return err
			}
			if len(log) == 0 {
				cmd.Println("Версий нет: параметры ещё не пересчитывались (ochan llm fit, ochan llm learn)")
			}
			for _, v := range log {
				when := "—"
				if v.Time != nil {
					when = *v.Time
				}
				cmd.Printf("%4d  %-25s  %s\n", v.Version, when, v.Note)
			}
			return nil
		case cmd.Flags().Changed("rollback"):
			to := -1
			if llmLearnRollback != "" && llmLearnRollback != "-1" {
				if to, err = strconv.Atoi(llmLearnRollback); err != nil {
					return fmt.Errorf("--rollback %q: номер версии", llmLearnRollback)
				}
			}
			from, version, err := own.Rollback(dir, to)
			if err != nil {
				return err
			}
			cmd.Printf("Параметры откачены к версии %d: это версия %d\n", from, version)
			return nil
		case llmLearnPackage != "":
			if llmLearnVersion == "" {
				return errors.New("--package: нужна --version — версия часть знания, её просит import")
			}
			params, err := own.LoadParams(dir)
			if err != nil {
				return err
			}
			name, n, err := own.ExportKnowledge(params.Clusters, llmLearnPackage, llmLearnVersion, llmLearnDescription)
			if err != nil {
				return err
			}
			cmd.Printf("Пакет знаний @%s v%s: обобщений %d — %s\n", name, strings.TrimPrefix(llmLearnVersion, "v"), n, llmLearnPackage)
			cmd.Printf("Публикация с подписью: ochan '!!registry' publish %s --registry <каталог> --key <ключ>\n", llmLearnPackage)
			return nil
		}
		if llmLearnPairs != "" && !(llmLearnReplayShare >= 0 && llmLearnReplayShare < 1) {
			return errors.New("--replay-share — доля от 0 до 1, не включая 1")
		}
		var rows []own.LearnRow
		source := "инциденты этой машины"
		if llmLearnClusters != "" {
			if rows, err = readLearnRows(llmLearnClusters); err != nil {
				return err
			}
			source = filepath.Base(llmLearnClusters)
		} else {
			opts, err := fpOptions(cmd, false)
			if err != nil {
				return err
			}
			sum, err := incidentClusters(opts)
			if err != nil {
				return err
			}
			data, _ := json.Marshal(sum.ripe)
			if err := json.Unmarshal(data, &rows); err != nil {
				return err
			}
		}
		m, err := loadOwnModel(dir)
		if err != nil {
			return err
		}
		before := m.Params.Clusters
		clusters := m.LearnClusters(rows)
		fresh, gone := 0, 0
		for k := range clusters {
			if before[k] == nil {
				fresh++
			}
		}
		for k := range before {
			if clusters[k] == nil {
				gone++
			}
		}
		cmd.Printf("Созревших кластеров %d: новых %d, прежних %d, ушло %d\n", len(clusters), fresh, len(clusters)-fresh, gone)
		for _, key := range sortedRuleNames(clusters) {
			c := clusters[key]
			r := m.Params.Outcomes(c)
			cmd.Printf("  %s  инцидентов %d; исходов %.1f, модель решила сама %.1f — надёжность [%.2f; %.2f]\n",
				key, c.Instances.N(), c.Outcomes[1], c.Outcomes[0], r.L, r.U)
			cmd.Printf("    обобщение: %s\n", strings.Join(c.Shape, "; "))
			for _, rule := range sortedRuleNames(c.Rules) {
				n, rr := c.Rules[rule], m.Params.Rule(rule, c)
				cmd.Printf("    %-12s верно %.1f из %.1f на этой структуре — надёжность [%.2f; %.2f]\n", rule, n[0], n[1], rr.L, rr.U)
			}
		}
		m.Params.Clusters = clusters
		version, err := m.Params.Save(dir, "кластеры инцидентов: "+source)
		if err != nil {
			return err
		}
		cmd.Printf("Версия параметров %d (откат — ochan llm learn --rollback)\n", version)
		if llmLearnPairs == "" {
			return nil
		}
		var replay []own.Pair
		if llmLearnReplay != "" {
			if replay, err = readPairs(llmLearnReplay); err != nil {
				return err
			}
		}
		pairs, mine := m.LearnPairs(rows, replay, llmLearnReplayShare, 7)
		for i := range pairs {
			if err := pairs[i].Tokenize(m.Tok); err != nil {
				return fmt.Errorf("пара %d: %w", i, err)
			}
		}
		if err := writePairs(llmLearnPairs, pairs); err != nil {
			return err
		}
		cmd.Printf("Пар для дообучения в %s: %d — из инцидентов %d, остальное — базовый корпус\n", llmLearnPairs, len(pairs), mine)
		return nil
	},
}

func readLearnRows(path string) ([]own.LearnRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []own.LearnRow
	for n, line := range strings.Split(strings.TrimPrefix(string(data), "\uFEFF"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r own.LearnRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n+1, err)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

var (
	llmQuantBits  int
	llmQuantGroup int
	llmQuantOut   string
	llmQuantCheck string
	llmQuantPairs int
)

var llmQuantizeCmd = &cobra.Command{
	Use:   "quantize --bits 8|4 --out каталог [--model каталог] [--group N] [--check корпус.jsonl]",
	Short: "Квантовать свою модель: int8 или int4 по группам строки",
	Long: "Веса матриц — симметрично по группам строки: int8 (группа — вся строка)\n" +
		"или int4 (группа --group, по умолчанию 32, по два значения в байте);\n" +
		"масштаб группы — fp16, нормировки остаются fp32. Каталог --out — снова\n" +
		"модель: словарь, конфигурация и параметры колец переходят с весами.\n\n" +
		"--check — сверка с исходной на отложенных парах корпуса (с токенами,\n" +
		"ochan llm corpus --model): доля позиций ответа с тем же самым вероятным\n" +
		"токеном и потери на ответе у обеих.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := modelDir(llmModelFlag)
		if err != nil {
			return err
		}
		if llmQuantOut == "" {
			return errors.New("--out: каталог квантованной модели")
		}
		res, err := own.Quantize(dir, expandHome(llmQuantOut), llmQuantBits, llmQuantGroup)
		if err != nil {
			return err
		}
		group := "строка"
		if res.Group > 0 {
			group = strconv.Itoa(res.Group)
		}
		cmd.Printf("int%d, группа %s: веса %.1f → %.1f МБ (%.1f×) — %s\n", res.Bits, group, float64(res.Before)/(1<<20),
			float64(res.After)/(1<<20), float64(res.Before)/float64(max(res.After, 1)), llmQuantOut)
		if llmQuantCheck == "" {
			return nil
		}
		pairs, err := readPairs(llmQuantCheck)
		if err != nil {
			return err
		}
		var test []own.Pair
		for _, p := range pairs {
			if p.Split == "test" && len(p.IDs) > 0 && len(test) < llmQuantPairs {
				test = append(test, p)
			}
		}
		if len(test) == 0 {
			return fmt.Errorf("%s: нет отложенных пар с токенами (ochan llm corpus --model)", llmQuantCheck)
		}
		base, err := own.LoadNet(dir)
		if err != nil {
			return err
		}
		other, err := own.LoadNet(expandHome(llmQuantOut))
		if err != nil {
			return err
		}
		agree, lossBase, lossQ, err := own.Compare(base, other, test)
		if err != nil {
			return err
		}
		cmd.Printf("Сверка на %d отложенных парах: тот же токен в %.2f%% позиций ответа; потери на ответе %.4f → %.4f\n",
			len(test), agree*100, lossBase, lossQ)
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{llmCorpusCmd, llmFitCmd, llmLearnCmd, llmQuantizeCmd} {
		c.Flags().StringVar(&llmModelFlag, "model", "", "каталог своей модели; без флага — llm.model")
	}
	llmInitCmd.Flags().StringVar(&llmInitOut, "out", "", "каталог новой модели")
	llmInitCmd.Flags().StringVar(&llmInitSize, "size", "small", "размер сети: tiny, small или base")
	llmInitCmd.Flags().IntVar(&llmInitContext, "context", 1024, "контекст: сколько токенов видит модель")
	llmInitCmd.Flags().BoolVar(&llmInitRandom, "random-weights", false, "случайные веса: проверить контур до обучения")
	llmInitCmd.Flags().BoolVar(&llmInitTrained, "pretrained", false, "стартовая модель: обученные веса и словарь, встроенные в ochan")
	llmInitCmd.Flags().BoolVar(&llmInitFull, "full", false, "с --pretrained: распаковать веса в float32 — для дообучения сети")

	llmCorpusCmd.Flags().IntVar(&llmCorpusCount, "count", 5000, "сколько пар")
	llmCorpusCmd.Flags().Int64Var(&llmCorpusSeed, "seed", 7, "начало случайной последовательности: тот же seed — тот же корпус")
	llmCorpusCmd.Flags().Float64Var(&llmCorpusTest, "test", 0.1, "доля отложенных пар")
	llmCorpusCmd.Flags().Float64Var(&llmCorpusH3, "h3", 0.5, "доля задач с решениями, у которых есть близнец без предпосылки")
	llmCorpusCmd.Flags().StringVar(&llmCorpusOut, "out", "", "файл корпуса (JSON Lines)")
	llmCorpusCmd.Flags().StringVar(&llmCorpusTasks, "tasks", "", "каталог: отложенные задачи файлами .fp — для замера")
	llmCorpusCmd.Flags().IntVar(&llmCorpusTasksCount, "tasks-count", 40, "сколько отложенных задач в --tasks")

	llmFitCmd.Flags().StringVar(&llmFitJournal, "journal", "", "выгрузка ochan fp journal export другой машины; без флага — журнал этой")

	llmLearnCmd.Flags().StringVar(&llmLearnClusters, "clusters", "", "выгрузка ochan incident learn --export; без флага — инциденты этой машины")
	llmLearnCmd.Flags().StringVar(&llmLearnPairs, "pairs", "", "записать пары для дообучения сети (train_own.py --init)")
	llmLearnCmd.Flags().StringVar(&llmLearnReplay, "replay", "", "базовый корпус для примеси к парам")
	llmLearnCmd.Flags().Float64Var(&llmLearnReplayShare, "replay-share", 0.9, "доля базового корпуса среди пар обучения")
	llmLearnCmd.Flags().BoolVar(&llmLearnVersions, "versions", false, "версии параметров модели")
	llmLearnCmd.Flags().StringVar(&llmLearnRollback, "rollback", "", "откатить параметры к версии N; без N — к предыдущей")
	llmLearnCmd.Flags().Lookup("rollback").NoOptDefVal = "-1"
	llmLearnCmd.Flags().StringVar(&llmLearnPackage, "package", "", "пакет знаний из кластеров модели; имя пакета — имя каталога")
	llmLearnCmd.Flags().StringVar(&llmLearnVersion, "version", "", "версия пакета знаний")
	llmLearnCmd.Flags().StringVar(&llmLearnDescription, "description", "", "описание пакета знаний одной строкой")

	llmQuantizeCmd.Flags().IntVar(&llmQuantBits, "bits", 8, "4 или 8")
	llmQuantizeCmd.Flags().IntVar(&llmQuantGroup, "group", 0, "размер группы; 0 — вся строка (у int4 — 32)")
	llmQuantizeCmd.Flags().StringVar(&llmQuantOut, "out", "", "каталог квантованной модели")
	llmQuantizeCmd.Flags().StringVar(&llmQuantCheck, "check", "", "корпус с токенами: сверка с исходной на отложенных парах")
	llmQuantizeCmd.Flags().IntVar(&llmQuantPairs, "pairs", 100, "сколько отложенных пар сверять")

	llmCmd.AddCommand(llmInitCmd, llmCorpusCmd, llmFitCmd, llmLearnCmd, llmQuantizeCmd)
}
