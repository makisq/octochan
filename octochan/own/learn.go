package own

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"ochan/fp"
	"ochan/rings"

	"gopkg.in/yaml.v3"
)

// Обучение своей модели на журнале решений и повторяющихся инцидентах.
//
// Журнал (FitJournal): надёжность правил, порог well_True и условный счёт
// автора берутся как есть; веса правила выбора — логистической регрессией
// по исходам выборов модели, калибровка оценок — монотонной поправкой
// «оценка → наблюдаемая доля». Пока исходов мало (меньше 20 выборов с
// обоими исходами), веса и калибровка остаются прежними.
//
// Инциденты (LearnClusters): одиночный инцидент — шум; созревший кластер —
// закономерность, и модель учится ей так же, как у неё устроено всё
// остальное — логико-структурно, со статистикой только там, где она есть
// по смыслу:
//
//   - обобщение — структура кластера без частных имён: факты fp.Shape,
//     которые есть больше чем у половины его инцидентов;
//   - правила на структуре — кандидаты: для инцидента с исходом логика
//     модели строит свой план (те же правила, что при ответе), и каждое
//     правило плана получает исход инцидента — решила модель сама (успех)
//     или отдала инженерам: сама или по сроку (неудача). Решённое человеком
//     и открытое исхода модели не говорит и не считается. Вес исхода падает
//     с давностью, как в кластере;
//   - задачу той же структуры модель узнаёт сама (Params.ClusterOf): её
//     кольцо 1 берёт надёжность правил на этой структуре, а структура,
//     которую модель решает плохо, — вопрос оператору.

// Journal — журнал решений для пересчёта колец (ochan fp journal export).
type Journal struct {
	Prior    [2]float64                    `json:"prior"`
	WellTrue float64                       `json:"well_true"`
	Rules    map[string][2]float64         `json:"rules"`
	Choices  []JournalChoice               `json:"choices"`
	History  map[string]map[string]float64 `json:"history"`
}

// JournalChoice — выбор модели с исходом: признаки правила выбора.
type JournalChoice struct {
	LogProb float64 `json:"logprob"`
	L       float64 `json:"l"`
	U       float64 `json:"u"`
	Correct bool    `json:"correct"`
}

// FitResult — что пересчитано.
type FitResult struct {
	Choices int
	Weights bool // веса и калибровка пересчитаны
}

// FitJournal пересчитывает параметры колец по журналу решений. Кластеры
// инцидентов журнал не трогает.
func (p *Params) FitJournal(j Journal) FitResult {
	p.Prior, p.WellTrue = j.Prior, j.WellTrue
	p.Rules = map[string]Counts{}
	for rule, c := range j.Rules {
		p.Rules[rule] = Counts(c)
	}
	p.History = j.History
	if p.History == nil {
		p.History = map[string]map[string]float64{}
	}
	var features []rings.Features
	var correct []bool
	for _, c := range j.Choices {
		features = append(features, rings.Features{LogProb: c.LogProb, Lower: c.L, Width: c.U - c.L, Binding: 1})
		correct = append(correct, c.Correct)
	}
	res := FitResult{Choices: len(j.Choices)}
	if w, ok := rings.FitWeights(features, correct); ok {
		p.SetChoiceWeights(w)
		var raw []float64
		for _, f := range features {
			raw = append(raw, w.Probability(f, nil))
		}
		p.SetCalibration(rings.Isotonic(raw, correct, 10))
		res.Weights = true
	}
	return res
}

// LearnIncident — инцидент созревшего кластера (ochan incident learn --export).
type LearnIncident struct {
	ID      string  `json:"id"`
	Weight  float64 `json:"weight"`
	Outcome string  `json:"outcome"`
	Summary string  `json:"summary"`
}

// LearnRow — созревший кластер выгрузки.
type LearnRow struct {
	Similarity float64         `json:"similarity"`
	Incidents  []LearnIncident `json:"incidents"`
}

// learnOutcomes — исход инцидента для модели: решила сама — успех; отдала
// инженерам сама или по сроку — неудача. Остальное исхода не говорит.
var learnOutcomes = map[string]float64{"model": 1, "gave_up": 0, "timeout": 0}

// generalization — обобщение кластера: факты, которые есть больше чем у
// половины его структур.
func generalization(shapes [][]string) []string {
	count := map[string]int{}
	for _, s := range shapes {
		seen := map[string]bool{}
		for _, f := range s {
			if !seen[f] {
				seen[f] = true
				count[f]++
			}
		}
	}
	out := []string{}
	for f, n := range count {
		if 2*n > len(shapes) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// ClusterKey — ключ кластера по его обобщению.
func ClusterKey(shape []string) string {
	sum := sha256.Sum256([]byte(strings.Join(shape, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// PlanRules — правила, которыми модель решает задачу: вывод её логики и
// сужения по истории. Задача без предпосылки — правил нет.
func (m *Model) PlanRules(task string) []string {
	r := fp.CheckText("task", task, m.Options)
	if r.Program == nil || len(r.Syntax()) > 0 || undef(r) > 0 {
		return nil
	}
	plan := fp.PlanAnswer(task, r.Program)
	set := map[string]bool{}
	for _, d := range plan.Decisions {
		for _, rule := range d.Rules {
			set[rule] = true
		}
	}
	for _, n := range narrowingsOf(m.Params, plan, inexactOrder(r.Program)) {
		for _, rule := range n.d.Rules {
			set[rule] = true
		}
	}
	out := make([]string, 0, len(set))
	for rule := range set {
		out = append(out, rule)
	}
	sort.Strings(out)
	return out
}

// LearnClusters — кластеры параметров модели из выгрузки: ключ → кластер.
// Кластеры пересчитываются из выгрузки целиком: не созревший больше
// кластер уходит из параметров.
func (m *Model) LearnClusters(rows []LearnRow) map[string]*Cluster {
	out := map[string]*Cluster{}
	for _, row := range rows {
		var shapes [][]string
		for _, inc := range row.Incidents {
			prog, _ := fp.Parse(inc.ID, inc.Summary)
			shapes = append(shapes, fp.Shape(prog, m.Tok.Closed))
		}
		shape := generalization(shapes)
		c := &Cluster{Shape: shape, Similarity: row.Similarity, Instances: Instances{IDs: []string{}}, Rules: map[string]Counts{}}
		for _, inc := range row.Incidents {
			c.Instances.IDs = append(c.Instances.IDs, inc.ID)
			ok, known := learnOutcomes[inc.Outcome]
			if !known {
				continue
			}
			w := inc.Weight
			if w == 0 {
				w = 1
			}
			c.Outcomes[0] += ok * w
			c.Outcomes[1] += w
			for _, rule := range m.PlanRules(inc.Summary) {
				counts := c.Rules[rule]
				counts[0] += ok * w
				counts[1] += w
				c.Rules[rule] = counts
			}
		}
		out[ClusterKey(shape)] = c
	}
	return out
}

// LearnPairs — пары для дообучения сети: задачи инцидентов, которые модель
// решила сама, с ответом логики; примесь базового корпуса replay — доля
// share среди пар обучения, и до 500 отложенных базового — смотреть, не
// забыла ли сеть остальное. Возвращает пары и сколько из инцидентов.
func (m *Model) LearnPairs(rows []LearnRow, replay []Pair, share float64, seed int64) ([]Pair, int) {
	rng := rand.New(rand.NewSource(seed))
	var mine []Pair
	for _, row := range rows {
		for _, inc := range row.Incidents {
			if inc.Outcome != "model" {
				continue
			}
			answer, ok := Solve(inc.Summary, m.Options)
			r := fp.CheckText("task", inc.Summary, m.Options)
			if !ok || undef(r) > 0 {
				continue
			}
			mine = append(mine, Pair{Task: inc.Summary, Answer: answer, Split: "train"})
		}
	}
	if len(mine) == 0 {
		return nil, 0
	}
	var train, test []Pair
	for _, p := range replay {
		if p.Split == "test" {
			test = append(test, p)
		} else {
			train = append(train, p)
		}
	}
	need := len(train)
	if share < 1 {
		need = int(float64(len(mine))*share/(1-share) + 0.5)
	}
	pick := func(from []Pair, n int) []Pair {
		cp := append([]Pair(nil), from...)
		rng.Shuffle(len(cp), func(i, j int) { cp[i], cp[j] = cp[j], cp[i] })
		return cp[:min(n, len(cp))]
	}
	out := append(append(append([]Pair(nil), mine...), pick(train, need)...), pick(test, 500)...)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, len(mine)
}

var (
	knowledgePackage = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]*$`)
	knowledgeVersion = regexp.MustCompile(`^v?(\d+(?:\.\d+){0,2})$`)
)

// ExportKnowledge — пакет знаний из кластеров модели: package.yaml,
// knowledge.yaml и README.md в dir; имя пакета — имя каталога. Номера
// инцидентов не выгружаются — только их число.
func ExportKnowledge(clusters map[string]*Cluster, dir, version, description string) (string, int, error) {
	name := filepath.Base(filepath.Clean(dir))
	if !knowledgePackage.MatchString(name) {
		return "", 0, fmt.Errorf("пакет %q: имя — имя каталога, буквы, цифры и _, первой — буква", name)
	}
	m := knowledgeVersion.FindStringSubmatch(version)
	if m == nil {
		return "", 0, fmt.Errorf("версия %q: вида 1, 1.2 или 1.2.3", version)
	}
	version = m[1]
	if strings.ContainsAny(description, "\n:") {
		return "", 0, fmt.Errorf("описание — одна строка без «:»")
	}
	if len(clusters) == 0 {
		return "", 0, fmt.Errorf("кластеров в параметрах модели нет — выгружать нечего (ochan llm learn --clusters)")
	}
	keys := make([]string, 0, len(clusters))
	for k := range clusters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var gens []*fp.KnownGeneralization
	for _, k := range keys {
		c := clusters[k]
		g := &fp.KnownGeneralization{Key: k, Shape: c.Shape, Similarity: c.Similarity, Instances: c.Instances.N(),
			Outcomes: []float64{c.Outcomes[0], c.Outcomes[1]}, Rules: map[string][]float64{}}
		for r, n := range c.Rules {
			g.Rules[r] = []float64{n[0], n[1]}
		}
		gens = append(gens, g)
	}
	body, err := yaml.Marshal(fp.Knowledge{Protocol: fp.KnowledgeProtocol, Generalizations: gens})
	if err != nil {
		return "", 0, err
	}
	text := "# Знания своей модели ℒ: обобщения задач, которые повторялись, и надёжность\n" +
		"# правил вывода на их структуре (ochan llm learn --package). Частных случаев нет.\n" + string(body)
	if _, err := fp.ParseKnowledge(fp.KnowledgeName, []byte(text)); err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(filepath.Join(dir, fp.KnowledgeName), []byte(text), 0o644); err != nil {
		return "", 0, err
	}
	manifest := "version: " + version + "\n"
	if description != "" {
		manifest += "description: " + description + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), []byte(manifest), 0o644); err != nil {
		return "", 0, err
	}
	major, _, _ := strings.Cut(version, ".")
	lines := []string{"# @" + name + " v" + version, "",
		"Знания своей модели ℒ — пакет реестра ochan. Модель берёт их для задачи с",
		"`import @" + name + " v" + major + "`, которая повторяет обобщение.", ""}
	for _, g := range gens {
		lines = append(lines, "## "+g.Key, "", fmt.Sprintf("Частных случаев %d, исходов %.1f, модель решила сама %.1f.",
			g.Instances, g.Outcomes[1], g.Outcomes[0]), "")
		for _, f := range g.Shape {
			lines = append(lines, "- `"+f+"`")
		}
		lines = append(lines, "")
		var rules []string
		for r := range g.Rules {
			rules = append(rules, r)
		}
		sort.Strings(rules)
		for _, r := range rules {
			lines = append(lines, fmt.Sprintf("Правило `%s`: верно %.1f из %.1f.", r, g.Rules[r][0], g.Rules[r][1]))
		}
		lines = append(lines, "")
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return "", 0, err
	}
	return name, len(gens), nil
}
