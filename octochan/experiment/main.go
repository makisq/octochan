// Стенд эксперимента 9.2 манифеста ФПм: задачи, прогоны в четырёх условиях
// и метрики.
//
// Условия (раздел 9.2):
//
//	natural        — 1: естественный язык;
//	formal         — 2: формальный промпт с разбором, арбитрами и отчётом, без колец;
//	protocol       — 3: то же с кольцами 1–3 в режиме протокола;
//	architectural  — 4: своя модель ℒ, у которой логика и кольца в её коде.
//
// В условиях 1–3 модель одна и та же — внешняя (протокольный подход); её ещё
// не выбрали, и для них здесь только подготовка промптов: прогон отказывает
// с причиной. Условие 4 запускается: ochan llm generate своей моделью в
// процессе ochan.
//
// Прогон — задача × запись × повтор. План ответа — пары «вопрос → ответ»
// X(I) из отчёта (А.5): по ним metrics считает согласованность и
// правильность. Логика — та же, что у ochan и у модели (fp): задуманный
// план и веса вопросов выводит fp.PlanAnswer.
//
//	go run ./experiment prepare --from "experiment/sources/*.fp"
//	go run ./experiment run --condition architectural --model own-small-2 --weights G:/fpm-llm/models/own-small-2 \
//	    --ochan ochan.exe --home G:/fpm-llm/runs/exp-home --out experiment/runs/architectural-own-small-2.jsonl
//	go run ./experiment metrics experiment/runs/*.jsonl --out experiment/results/report.md
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"ochan/fp"

	"gopkg.in/yaml.v3"
)

var conditions = []string{"natural", "formal", "protocol", "architectural"}

// here — каталог стенда: рядом с исходником, если запуск из корня модуля.
func here() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		if _, err := os.Stat(filepath.Join(filepath.Dir(file), "plan.yaml")); err == nil {
			return filepath.Dir(file)
		}
	}
	return "experiment"
}

// Task — задача эксперимента (tasks/*.yaml).
type Task struct {
	ID       string            `yaml:"id"`
	Domain   string            `yaml:"domain"`
	Status   string            `yaml:"status"`
	Intended map[string]string `yaml:"intended"`
	Open     []string          `yaml:"open"`
	Weights  map[string]int    `yaml:"weights"`
	Natural  []string          `yaml:"natural"`
	Formal   []string          `yaml:"formal"`
	Lacking  []string          `yaml:"lacking"`
}

// Plan — параметры эксперимента (plan.yaml).
type Plan struct {
	Version        int    `yaml:"version"`
	Fixed          string `yaml:"fixed"`
	RunsPerWriting int    `yaml:"runs_per_writing"`
}

// Row — прогон.
type Row struct {
	Task      string            `json:"task"`
	Kind      string            `json:"kind"`
	Writing   int               `json:"writing"`
	Run       int               `json:"run"`
	Condition string            `json:"condition"`
	Model     string            `json:"model"`
	OK        bool              `json:"ok"`
	Refused   bool              `json:"refused"`
	Plan      map[string]string `json:"plan"`
	Decisions map[string]string `json:"decisions"`
	Seconds   float64           `json:"seconds"`
	Forced    int               `json:"forced"`
	Chosen    int               `json:"chosen"`
	Reason    string            `json:"reason"`
}

func loadPlan() (Plan, error) {
	var p Plan
	data, err := os.ReadFile(filepath.Join(here(), "plan.yaml"))
	if err != nil {
		return p, err
	}
	return p, yaml.Unmarshal(data, &p)
}

func loadTasks(pattern string) ([]Task, error) {
	if pattern == "" {
		pattern = filepath.Join(here(), "tasks", "*.yaml")
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var tasks []Task
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var t Task
		if err := yaml.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

var setLine = regexp.MustCompile(`^\s*set\s+(\w+)\.(\w+)\s*=\s*(.+?)\s*$`)
var cellHead = regexp.MustCompile(`^cell\s+(\w+)`)

// planOf — X(I): вопрос отчёта → выбор; и решения — правки новых ячеек
// ответа (цель.атрибут → значение), не ячеек задачи: по ним ищутся ошибки
// вне отчёта.
func planOf(answer, task string) (map[string]string, map[string]string) {
	given := map[string]bool{}
	if prog, _ := fp.Parse("task", task); prog != nil {
		for _, c := range prog.Cells {
			given[c.Name] = true
		}
	}
	report, decisions := map[string]string{}, map[string]string{}
	section, q := "", ""
	for _, line := range strings.Split(answer, "\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			section = ""
			if strings.HasPrefix(line, "report:") {
				section = "report"
			} else if m := cellHead.FindStringSubmatch(line); m != nil && !given[m[1]] {
				section = "cell"
			}
			continue
		}
		body := strings.TrimSpace(line)
		switch section {
		case "report":
			key, value, _ := strings.Cut(body, ":")
			switch key {
			case "q":
				q = strings.TrimSpace(value)
			case "choice":
				if q != "" {
					report[q] = strings.TrimSpace(value)
				}
			}
		case "cell":
			if m := setLine.FindStringSubmatch(line); m != nil {
				decisions[m[1]+"."+m[2]] = m[3]
			}
		}
	}
	return report, decisions
}

// weightsOf — w(q) = 1 + n(q): n — число отношений задачи, опирающихся на
// класс цели решения по вопросу q (А.5). Фиксируются до замера.
func weightsOf(prog *fp.Program) map[string]int {
	out := map[string]int{}
	for _, d := range fp.Derive(prog).Decisions {
		n := 0
		for _, r := range prog.Relations {
			if r.From == d.Target || r.To == d.Target {
				n++
			}
		}
		if w := 1 + n; w > out[d.Attr] {
			out[d.Attr] = w
		}
		if out[d.Attr] == 0 {
			out[d.Attr] = 1
		}
	}
	return out
}

var (
	goalLine  = regexp.MustCompile(`(?m)^goal:\s*(\w+)`)
	classLine = regexp.MustCompile(`^(\s+)(\w+)(\s*:\s*)"(\w+)"\s*$`)
)

// rewrite — другая формальная запись той же задачи: отношения и правки в
// другом порядке, слова классов — синонимы того же смысла.
func rewrite(text string, lex *fp.Lexicon, seed string) string {
	h := fnv.New64a()
	h.Write([]byte(seed))
	rng := rand.New(rand.NewSource(int64(h.Sum64())))
	same := map[string][]string{}
	for _, w := range lex.Words() {
		if e, ok := lex.Lookup(w); ok && len(e.Senses) == 1 {
			same[e.Senses[0]] = append(same[e.Senses[0]], w)
		}
	}
	goal := ""
	if m := goalLine.FindStringSubmatch(text); m != nil {
		goal = m[1]
	}
	var out, block []string
	flush := func() {
		rng.Shuffle(len(block), func(i, j int) { block[i], block[j] = block[j], block[i] })
		out = append(out, block...)
		block = nil
	}
	section := ""
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			flush()
			section = strings.Fields(strings.SplitN(line, ":", 2)[0])[0]
			out = append(out, line)
			continue
		}
		m := classLine.FindStringSubmatch(line)
		switch {
		case section == "classes" && m != nil && m[4] != goal:
			var options []string
			for _, w := range same[m[2]] {
				if w != m[4] {
					options = append(options, w)
				}
			}
			word := m[4]
			if len(options) > 0 && rng.Float64() < 0.7 {
				word = options[rng.Intn(len(options))]
			}
			out = append(out, m[1]+m[2]+m[3]+`"`+word+`"`)
		case (section == "relations" || section == "cell") && strings.TrimSpace(line) != "":
			block = append(block, line)
		default:
			flush()
			out = append(out, line)
		}
	}
	flush()
	return strings.Join(out, "\n") + "\n"
}

func prepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	from := fs.String("from", filepath.Join(here(), "sources", "*.fp"), "формальные промпты задач (шаблон)")
	force := fs.Bool("force", false, "перезаписать готовые задачи")
	fs.Parse(args)
	var natural map[string]struct {
		Domain  string   `yaml:"domain"`
		Natural []string `yaml:"natural"`
	}
	if data, err := os.ReadFile(filepath.Join(here(), "natural.yaml")); err == nil {
		if err := yaml.Unmarshal(data, &natural); err != nil {
			return fmt.Errorf("natural.yaml: %w", err)
		}
	}
	paths, err := filepath.Glob(*from)
	if err != nil {
		return err
	}
	sort.Strings(paths)
	lex := fp.BaseLexicon()
	if err := os.MkdirAll(filepath.Join(here(), "tasks"), 0o755); err != nil {
		return err
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		target := filepath.Join(here(), "tasks", name+".yaml")
		if _, err := os.Stat(target); err == nil && !*force {
			fmt.Printf("%s: уже есть (--force — перезаписать)\n", target)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		prog, diags := fp.Parse(name, text)
		if len(diags) > 0 {
			return fmt.Errorf("%s: %v", path, diags[0])
		}
		plan := fp.PlanAnswer(text, prog)
		intended := map[string]string{}
		for _, d := range plan.Decisions {
			intended[d.Attr] = d.Value
		}
		open := map[string]bool{}
		for _, q := range plan.Open {
			open[q.Attr] = true
		}
		for key := range plan.Inexact {
			_, attr, _ := strings.Cut(key, ".")
			if _, decided := intended[attr]; !decided {
				open[attr] = true
			}
		}
		about := natural[name]
		t := Task{ID: name, Domain: about.Domain, Status: "черновик: эталон и пересказы утверждает владелец",
			Intended: intended, Open: sortedSet(open), Weights: weightsOf(prog), Natural: about.Natural,
			Formal:  []string{text, rewrite(text, lex, name+"/1"), rewrite(text, lex, name+"/2")},
			Lacking: []string{strings.TrimRight(text, "\n") + "\n\nrequires: incident_history\n"}}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(t); err != nil {
			return err
		}
		if err := os.WriteFile(target, buf.Bytes(), 0o644); err != nil {
			return err
		}
		fmt.Printf("%s: записей %d, вопросов %d\n", target, len(t.Formal), len(t.Intended))
	}
	return nil
}

var logicLine = regexp.MustCompile(`Логика:\s+утверждений (\d+) — .*?среди допустимых: (\d+)`)

func runArchitectural(ochan, home string, env []string, task Task, kind string, k int, text string) Row {
	path := filepath.Join(home, "tasks", fmt.Sprintf("%s-%s%d.fp", task.ID, kind, k))
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(text), 0o644)
	started := time.Now()
	c := exec.Command(ochan, "llm", "generate", "--prompt", path)
	c.Env = env
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	row := Row{OK: err == nil, Refused: strings.Contains(stderr.String(), "не хватает предпосылок"),
		Seconds: math.Round(time.Since(started).Seconds()*100) / 100, Plan: map[string]string{}, Decisions: map[string]string{}}
	if err == nil {
		row.Plan, row.Decisions = planOf(stdout.String(), text)
	} else {
		reason := strings.TrimSpace(stderr.String())
		row.Reason = reason[max(0, len(reason)-300):]
	}
	if m := logicLine.FindStringSubmatch(stderr.String()); m != nil {
		fmt.Sscan(m[1], &row.Forced)
		fmt.Sscan(m[2], &row.Chosen)
	}
	return row
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	condition := fs.String("condition", "", "условие: "+strings.Join(conditions, ", "))
	model := fs.String("model", "", "метка модели: по ней H4 сравнивает версии")
	weights := fs.String("weights", "", "каталог своей модели (llm.model)")
	ochan := fs.String("ochan", "ochan", "ochan")
	home := fs.String("home", "", "дом ochan для прогонов: свой журнал")
	tasks := fs.String("tasks", "", "шаблон задач (по умолчанию tasks/*.yaml)")
	out := fs.String("out", "", "файл прогонов (JSON Lines)")
	fs.Parse(args)
	if *condition != "architectural" {
		return fmt.Errorf("условие %s: модель для условий 1–3 не выбрана (experiment/README.md, «Что ещё не запускается»); "+
			"промпты готовы в задачах — natural и formal", *condition)
	}
	if *model == "" || *weights == "" || *home == "" || *out == "" {
		return errors.New("нужны --model, --weights, --home и --out")
	}
	plan, err := loadPlan()
	if err != nil {
		return err
	}
	list, err := loadTasks(*tasks)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(*home, ".octochan"), 0o755); err != nil {
		return err
	}
	config := filepath.Join(*home, "config.yaml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("llm:\n  mode: embedded\n  model: %q\n", *weights)), 0o644); err != nil {
		return err
	}
	env := append(os.Environ(), "HOME="+*home, "USERPROFILE="+*home, "OCTOCHAN_CONFIG="+config)
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, t := range list {
		for _, kind := range []string{"formal", "lacking"} {
			texts := t.Formal
			if kind == "lacking" {
				texts = t.Lacking
			}
			for k, text := range texts {
				for r := 0; r < plan.RunsPerWriting; r++ {
					row := runArchitectural(*ochan, *home, env, t, kind, k, text)
					row.Task, row.Kind, row.Writing, row.Run, row.Condition, row.Model = t.ID, kind, k, r, *condition, *model
					if err := enc.Encode(row); err != nil {
						return err
					}
				}
			}
		}
		fmt.Printf("%s: готово\n", t.ID)
	}
	return nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "experiment prepare | run | metrics")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "prepare":
		err = prepare(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "metrics":
		err = metrics(os.Args[2:])
	default:
		err = fmt.Errorf("команда %q: prepare, run или metrics", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}
