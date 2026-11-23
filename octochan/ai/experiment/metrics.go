package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Метрики эксперимента 9.2 по прогонам (разделы 9.1, 9.2, А.5).
//
//   - ĉ — согласованность: доля пар прогонов с одинаковым планом; на одной
//     записи (повторы) и между записями одной задачи (перефразировки, H1);
//   - p̂ — правильность: доля прогонов с задуманным планом I* задачи;
//   - градуированные ĉ и p̂ — 1 − δ, δ — взвешенное расстояние Жаккара по
//     парам «вопрос → ответ» с весами задачи (А.5);
//   - H3 — на задаче без предпосылки: ошибка (отказ), а не догадка;
//   - ошибки вне отчёта — решения ячеек ответа, которых нет в отчёте (H2);
//   - H4 — доля задач, план которых меняется при смене версии модели;
//   - H6 — калибровка по журналу: ochan fp journal export (--journal);
//   - H7 — связь согласованности и правильности по записям;
//   - цена — время, утверждения логики и выборы сети.
//
// Разметка слепая (9.2): --blind пишет планы для разметчика без условия и
// модели, в случайном порядке, с ключом отдельно.

// delta — взвешенное расстояние Жаккара между планами (А.5); δ(∅, ∅) = 0.
func delta(x, y map[string]string, w map[string]int) float64 {
	weight := func(q string) float64 {
		if v, ok := w[q]; ok {
			return float64(v)
		}
		return 1
	}
	var union, diff float64
	seen := map[string]bool{}
	for q, a := range x {
		key := q + "\x00" + a
		seen[key] = true
		union += weight(q)
		if y[q] != a || !hasKey(y, q) {
			diff += weight(q)
		}
	}
	for q, b := range y {
		if seen[q+"\x00"+b] {
			continue
		}
		union += weight(q)
		diff += weight(q)
	}
	if union == 0 {
		return 0
	}
	return diff / union
}

func hasKey(m map[string]string, k string) bool { _, ok := m[k]; return ok }

func planKey(r Row) string {
	if r.Refused {
		return "⊥undef"
	}
	keys := make([]string, 0, len(r.Plan))
	for k := range r.Plan {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + r.Plan[k] + "\n")
	}
	return b.String()
}

func samePlan(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// pearson — корреляция Пирсона; NaN, если у ряда нет разброса.
func pearson(x, y []float64) float64 {
	mx, my := mean(x), mean(y)
	var sxy, sxx, syy float64
	for i := range x {
		sxy += (x[i] - mx) * (y[i] - my)
		sxx += (x[i] - mx) * (x[i] - mx)
		syy += (y[i] - my) * (y[i] - my)
	}
	if sxx == 0 || syy == 0 {
		return math.NaN()
	}
	return sxy / math.Sqrt(sxx*syy)
}

// metric — точно, 1 − δ (если есть) и n.
type metric struct {
	name   string
	value  float64
	graded *float64
	n      int
}

func summarize(rows []Row, tasks map[string]Task) []metric {
	var formal, lacking []Row
	for _, r := range rows {
		switch r.Kind {
		case "formal":
			formal = append(formal, r)
		case "lacking":
			lacking = append(lacking, r)
		}
	}
	type writing struct {
		task string
		k    int
	}
	groups := map[writing][]Row{}
	var order []writing
	for _, r := range formal {
		w := writing{r.Task, r.Writing}
		if _, ok := groups[w]; !ok {
			order = append(order, w)
		}
		groups[w] = append(groups[w], r)
	}
	var same, sameG, para, paraG []float64
	for _, w := range order {
		runs, weights := groups[w], tasks[w.task].Weights
		for i := 0; i < len(runs); i++ {
			for j := i + 1; j < len(runs); j++ {
				same = append(same, b2f(planKey(runs[i]) == planKey(runs[j])))
				sameG = append(sameG, 1-delta(runs[i].Plan, runs[j].Plan, weights))
			}
		}
	}
	byTask := map[string][]Row{}
	for _, r := range formal {
		if r.Run == 0 {
			byTask[r.Task] = append(byTask[r.Task], r)
		}
	}
	for task, first := range byTask {
		weights := tasks[task].Weights
		for i := 0; i < len(first); i++ {
			for j := i + 1; j < len(first); j++ {
				para = append(para, b2f(planKey(first[i]) == planKey(first[j])))
				paraG = append(paraG, 1-delta(first[i].Plan, first[j].Plan, weights))
			}
		}
	}
	g := func(v float64) *float64 { return &v }
	var right, graded, outside, h3 []float64
	for _, r := range formal {
		t := tasks[r.Task]
		right = append(right, b2f(samePlan(r.Plan, t.Intended) && !r.Refused))
		graded = append(graded, 1-delta(r.Plan, t.Intended, t.Weights))
		if r.OK {
			bad := false
			for target := range r.Decisions {
				attr := target[strings.LastIndex(target, ".")+1:]
				if !hasKey(r.Plan, attr) {
					bad = true
				}
			}
			outside = append(outside, b2f(bad))
		}
	}
	for _, r := range lacking {
		h3 = append(h3, b2f(r.Refused || len(r.Plan) == 0))
	}
	var cs, ps []float64
	for _, w := range order {
		runs := groups[w]
		c := 1.0
		if len(runs) > 1 {
			var eq []float64
			for i := 0; i < len(runs); i++ {
				for j := i + 1; j < len(runs); j++ {
					eq = append(eq, b2f(planKey(runs[i]) == planKey(runs[j])))
				}
			}
			c = mean(eq)
		}
		var ok []float64
		for _, r := range runs {
			ok = append(ok, b2f(samePlan(r.Plan, tasks[w.task].Intended)))
		}
		cs, ps = append(cs, c), append(ps, mean(ok))
	}
	h7 := math.NaN()
	if len(cs) > 2 {
		h7 = pearson(cs, ps)
	}
	var seconds, forced, chosen []float64
	for _, r := range formal {
		seconds, forced, chosen = append(seconds, r.Seconds), append(forced, float64(r.Forced)), append(chosen, float64(r.Chosen))
	}
	return []metric{
		{"ĉ на одной записи", mean(same), g(mean(sameG)), len(same)},
		{"ĉ между записями (H1)", mean(para), g(mean(paraG)), len(para)},
		{"p̂ по эталону", mean(right), g(mean(graded)), len(right)},
		{"H3: без предпосылки — отказ", mean(h3), nil, len(h3)},
		{"ошибки вне отчёта (H2)", mean(outside), nil, len(outside)},
		{"H7: корреляция ĉ и p̂ по записям", h7, nil, len(cs)},
		{"цена: время на прогон, с", mean(seconds), nil, len(formal)},
		{"цена: утверждений логики / выборов сети", mean(forced), g(mean(chosen)), len(formal)},
	}
}

// versionsChanged — H4: доля задач, план которых (первый прогон первой
// записи) меняется между версиями модели.
func versionsChanged(byModel map[string][]Row) (float64, int) {
	plans := map[string]map[string]string{}
	for model, rows := range byModel {
		for _, r := range rows {
			if r.Kind == "formal" && r.Writing == 0 && r.Run == 0 {
				if plans[r.Task] == nil {
					plans[r.Task] = map[string]string{}
				}
				plans[r.Task][model] = planKey(r)
			}
		}
	}
	changed, n := 0, 0
	for _, v := range plans {
		if len(v) < 2 {
			continue
		}
		n++
		distinct := map[string]bool{}
		for _, k := range v {
			distinct[k] = true
		}
		if len(distinct) > 1 {
			changed++
		}
	}
	if n == 0 {
		return math.NaN(), 0
	}
	return float64(changed) / float64(n), n
}

func fmtValue(v float64) string {
	if math.IsNaN(v) {
		return "nan"
	}
	return fmt.Sprintf("%.3f", v)
}

func metrics(args []string) error {
	fs := flag.NewFlagSet("metrics", flag.ExitOnError)
	out := fs.String("out", "", "отчёт (Markdown)")
	journal := fs.String("journal", "", "ochan fp journal export — для H6")
	blindPath := fs.String("blind", "", "файл планов для слепой разметки")
	files := parseInterleaved(fs, args)
	if *out == "" || len(files) == 0 {
		return errors.New("metrics прогоны.jsonl… --out отчёт.md")
	}
	list, err := loadTasks("")
	if err != nil {
		return err
	}
	tasks := map[string]Task{}
	for _, t := range list {
		tasks[t.ID] = t
	}
	var rows []Row
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "" {
				continue
			}
			var r Row
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				f.Close()
				return fmt.Errorf("%s: %w", path, err)
			}
			rows = append(rows, r)
		}
		f.Close()
	}
	plan, err := loadPlan()
	if err != nil {
		return err
	}
	type cond struct{ condition, model string }
	byCond, byModel := map[cond][]Row{}, map[string][]Row{}
	for _, r := range rows {
		c := cond{r.Condition, r.Model}
		byCond[c] = append(byCond[c], r)
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	var conds []cond
	for c := range byCond {
		conds = append(conds, c)
	}
	sort.Slice(conds, func(i, j int) bool {
		if conds[i].condition != conds[j].condition {
			return conds[i].condition < conds[j].condition
		}
		return conds[i].model < conds[j].model
	})
	lines := []string{"# Эксперимент 9.2: результаты", "",
		fmt.Sprintf("План версии %d от %s; задач %d, прогонов %d.", plan.Version, plan.Fixed, len(list), len(rows)), ""}
	for _, c := range conds {
		lines = append(lines, fmt.Sprintf("## %s, модель %s", c.condition, c.model), "", "| метрика | точно | 1 − δ | n |", "|---|---|---|---|")
		for _, m := range summarize(byCond[c], tasks) {
			graded := "—"
			if m.graded != nil {
				graded = fmtValue(*m.graded)
			}
			lines = append(lines, fmt.Sprintf("| %s | %s | %s | %d |", m.name, fmtValue(m.value), graded, m.n))
		}
		lines = append(lines, "")
	}
	if len(byModel) > 1 {
		var models []string
		for m := range byModel {
			models = append(models, m)
		}
		sort.Strings(models)
		share, n := versionsChanged(byModel)
		lines = append(lines, "## H4: смена версии модели", "",
			fmt.Sprintf("Доля задач, план которых меняется между версиями (%s): %s из %d задач.", strings.Join(models, ", "), fmtValue(share), n), "")
	}
	if *journal != "" {
		data, err := os.ReadFile(*journal)
		if err != nil {
			return err
		}
		var j struct {
			Choices []struct {
				Score   float64 `json:"score"`
				Correct bool    `json:"correct"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(string(data), "\uFEFF")), &j); err != nil {
			return err
		}
		bins := map[int][2]int{}
		for _, c := range j.Choices {
			k := min(int(c.Score*10), 9)
			b := bins[k]
			if c.Correct {
				b[0]++
			}
			b[1]++
			bins[k] = b
		}
		lines = append(lines, "## H6: калибровка выбора по журналу", "", "| оценка | доля верных | выборов |", "|---|---|---|")
		var ks []int
		for k := range bins {
			ks = append(ks, k)
		}
		sort.Ints(ks)
		for _, k := range ks {
			b := bins[k]
			lines = append(lines, fmt.Sprintf("| %.1f–%.1f | %.2f | %d |", float64(k)/10, float64(k+1)/10, float64(b[0])/float64(b[1]), b[1]))
		}
		lines = append(lines, "")
	}
	report := strings.Join(lines, "\n")
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
		return err
	}
	if *blindPath != "" {
		if err := blind(rows, *blindPath, 7); err != nil {
			return err
		}
	}
	fmt.Println(report)
	return nil
}

// blind — планы для слепой разметки: без условия и модели, в случайном
// порядке; ключ — отдельным файлом.
func blind(rows []Row, path string, seed int64) error {
	type item struct {
		Item    int               `json:"item"`
		Task    string            `json:"task"`
		Plan    map[string]string `json:"plan"`
		Refused bool              `json:"refused"`
	}
	type key struct {
		Item      int    `json:"item"`
		Condition string `json:"condition"`
		Model     string `json:"model"`
		Writing   int    `json:"writing"`
		Run       int    `json:"run"`
	}
	items := make([]item, len(rows))
	keys := make([]key, len(rows))
	for i, r := range rows {
		items[i] = item{i, r.Task, r.Plan, r.Refused}
		keys[i] = key{i, r.Condition, r.Model, r.Writing, r.Run}
	}
	rand.New(rand.NewSource(seed)).Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
	write := func(path string, v func(enc *json.Encoder) error) error {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		enc.SetEscapeHTML(false)
		if err := v(enc); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	if err := write(path, func(enc *json.Encoder) error {
		for _, it := range items {
			if err := enc.Encode(it); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return write(path+".key", func(enc *json.Encoder) error {
		for _, k := range keys {
			if err := enc.Encode(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// parseInterleaved — флаги и позиционные аргументы вперемешку, как у
// argparse: файлы прогонов можно перечислить до --out.
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return positional
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
