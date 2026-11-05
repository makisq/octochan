package own

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ochan/embedded"
	"ochan/fp"
)

// testModel — каталог своей модели со словарём базового алфавита и
// случайными весами крошечной сети: логика модели пишет копию и выводимое
// сама, сеть выбирает только порядок, поэтому ответ от весов почти не
// зависит.
func testModel(t *testing.T, params *Params) string {
	t.Helper()
	dir := t.TempDir()
	tok, err := BuildTokenizer(testAlphabet(t), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.Save(dir); err != nil {
		t.Fatal(err)
	}
	cfg := Config{VocabSize: tok.Len(), HiddenSize: 16, Layers: 1, Heads: 2, KVHeads: 2, Intermediate: 32, HeadDim: 8,
		MaxPositions: 1024, RMSNormEps: 1e-6, RopeTheta: 10000, TieWordEmbedding: true, ModelType: "llama", HiddenAct: "silu",
		Bos: tok.Bos, Eos: tok.Eos, PadToken: tok.Pad}
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := WriteRandomWeights(dir, cfg, 1); err != nil {
		t.Fatal(err)
	}
	if params != nil {
		if _, err := params.Save(dir, "тест"); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Задача с тремя решениями: перенос по defines и modifies, цепочка в два
// шага и стрелка обобщения.
const engineTask = `goal: probe

classes:
  File     : "file"
  Approval : "approve"
  Probe    : "probe"

relations:
  File     --defines-->    Approval (database_quota)
  Approval --modifies-->   Probe    (low_nightly)
  Probe    --strives_to--> File

cell c1:
  set File.limit = 200
  set File.owner = root
`

func prompt(task string) string { return "protocol" + TaskMarker + task }

func generate(t *testing.T, m *Model, task string) embedded.Answer {
	t.Helper()
	a, err := m.Generate(context.Background(), prompt(task))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Ответ пишет логика: копия задачи, решения, отчёт. Цепочка из двух
// правил без журнала (условный счёт 8 из 8) — нижняя граница около 0,44:
// кольцо 2 её не пропускает, и она уходит вопросом оператору. Выбор между
// двумя решениями одного шага — сеть, правилом выбора.
func TestGenerateIsLogic(t *testing.T) {
	m, err := Load(testModel(t, nil), "")
	if err != nil {
		t.Fatal(err)
	}
	a := generate(t, m, engineTask)
	prog, _ := fp.Parse("task", engineTask)
	plan := fp.PlanAnswer(engineTask, prog)
	if a.Refused || !strings.HasPrefix(a.Text, plan.Copy) {
		t.Fatalf("ответ:\n%s", a.Text)
	}
	var omitted []string
	for _, p := range a.Packages {
		if p.Verdict == "well_false" {
			omitted = append(omitted, p.Statement)
		}
	}
	if err := plan.Verify(a.Text, omitted, nil); err != nil {
		t.Fatalf("логика ochan не принимает ответ своей модели: %v\n%s", err, a.Text)
	}
	if len(omitted) != 2 || a.Chosen != 1 || len(a.Questions) != 2 || !strings.Contains(a.Questions[0].Why, "ниже порога 0.50") {
		t.Fatalf("пропущено %v, выборов %d, вопросы %+v", omitted, a.Chosen, a.Questions)
	}
	var chosen int
	for _, p := range a.Packages {
		if p.Chosen {
			chosen++
			if p.Options != 2 || p.LogProb >= 0 || p.Score <= 0 || p.Score >= 1 {
				t.Fatalf("выбор: %+v", p)
			}
		}
	}
	if chosen != 1 {
		t.Fatalf("выбрано %d", chosen)
	}
	// Тот же вход — тот же ответ.
	if again := generate(t, m, engineTask); again.Text != a.Text {
		t.Fatalf("ответ не повторяется:\n%s\n%s", a.Text, again.Text)
	}
}

// Без предпосылки модель не отвечает: отказ — её ответ.
func TestRefusesWithoutPremise(t *testing.T) {
	m, _ := Load(testModel(t, nil), "")
	a := generate(t, m, engineTask+"\nrequires: store\n")
	if !a.Refused || len(a.Missing) != 1 || !strings.HasPrefix(a.Missing[0], "store") || a.Text != "" {
		t.Fatalf("отказ: %+v", a)
	}
}

// Журнал подтвердил правила — цепочка проходит кольцо 2. Неточную
// предпосылку модель сужает по истории прочтений. Задачу той же структуры,
// что кластер инцидентов, модель узнаёт сама; плохо решённая структура —
// вопрос оператору.
func TestRingsFromJournal(t *testing.T) {
	p := DefaultParams()
	p.Rules = map[string]Counts{"fp.defines": {40, 40}, "fp.modifies": {40, 40}}
	p.History = map[string]map[string]float64{"window": {"night": 9}}
	task := strings.Replace(engineTask, "  set File.owner = root\n", "  set File.owner = root\n  set Probe.window = {night, weekend}\n", 1)
	prog, _ := fp.Parse("task", task)
	tok, _ := BuildTokenizer(testAlphabet(t), true)
	p.Clusters = map[string]*Cluster{"aa11": {Shape: fp.Shape(prog, tok.Closed), Similarity: 0.9,
		Instances: Instances{IDs: []string{"i1", "i2"}}, Outcomes: Counts{1, 6}, Rules: map[string]Counts{"fp.defines": {6, 6}}}}
	m, _ := Load(testModel(t, p), "")
	a := generate(t, m, task)
	if !strings.Contains(a.Text, "set Probe.limit = 200") || !strings.Contains(a.Text, "basis: history(window)") {
		t.Fatalf("ответ:\n%s", a.Text)
	}
	if a.Cluster == nil || a.Cluster.Key != "aa11" || a.Cluster.Instances != 2 {
		t.Fatalf("кластер: %+v", a.Cluster)
	}
	last := a.Questions[len(a.Questions)-1]
	if last.Cluster != "aa11" || !strings.Contains(last.Why, "модель решила сама 1.0 из 6.0") {
		t.Fatalf("вопросы: %+v", a.Questions)
	}
	for _, pkg := range a.Packages {
		if strings.Contains(pkg.Statement, "Approval.limit") && pkg.Cluster != "aa11" {
			t.Fatalf("пакет по кластеру: %+v", pkg)
		}
	}
}

// Знания пакета, который задача подключает, — как кластер; версии, которую
// задача просит, нет — недостающая предпосылка.
func TestKnowledgePackages(t *testing.T) {
	modules := t.TempDir()
	write := func(name, version, shape string) {
		dir := filepath.Join(modules, name)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "package.yaml"), []byte("version: "+version+"\n"), 0o644)
		os.WriteFile(filepath.Join(dir, fp.KnowledgeName), []byte("protocol: 1\ngeneralizations:\n  - key: k1\n    shape: ["+shape+
			"]\n    similarity: 0.5\n    instances: 3\n    outcomes: [3, 3]\n    rules:\n      fp.defines: [3, 3]\n"), 0o644)
	}
	task := engineTask + "\nimport @pg_incidents v1\n  provides: File\n"
	prog, _ := fp.Parse("task", task)
	tok, _ := BuildTokenizer(testAlphabet(t), true)
	quoted, _ := json.Marshal(fp.Shape(prog, tok.Closed))
	write("pg_incidents", "1.0", strings.Trim(string(quoted), "[]"))
	write("old", "2.0", `"class X"`)
	m, _ := Load(testModel(t, nil), modules)
	if got := m.Info().Knowledge; strings.Join(got, ",") != "old 2.0,pg_incidents 1.0" {
		t.Fatalf("знания: %v", got)
	}
	a := generate(t, m, task)
	if a.Refused || a.Cluster == nil || a.Cluster.Key != "@pg_incidents/k1" || a.Cluster.Instances != 3 {
		t.Fatalf("знания пакета: %+v %+v", a.Refused, a.Cluster)
	}
	a = generate(t, m, engineTask+"\nimport @old v1\n  provides: File\n")
	if !a.Refused || !strings.Contains(strings.Join(a.Missing, ";"), "@old v1: знаний этой версии у модели нет — в каталоге 2.0") {
		t.Fatalf("другая версия: %+v", a)
	}
}

// Протокол движка v1: /v1/info говорит «generate», /v1/generate — ответ,
// /v1/next — распределение, переопределённое логикой: копию задачи модель
// пишет с вероятностью единица.
func TestProtocolServer(t *testing.T) {
	m, _ := Load(testModel(t, nil), "")
	srv := httptest.NewServer(embedded.Handler(m.Info(), m))
	defer srv.Close()
	client := embedded.NewHTTP(srv.URL, 0)
	ctx := context.Background()
	info, err := client.Info(ctx)
	if err != nil || !info.Generate {
		t.Fatalf("info %+v: %v", info, err)
	}
	a, err := client.Generate(ctx, prompt(engineTask))
	if err != nil || a.Refused || !strings.HasPrefix(a.Text, "goal: probe\n") {
		t.Fatalf("generate: %v %+v", err, a)
	}
	step, err := client.Next(ctx, prompt(engineTask), "", 2)
	if err != nil || len(step.Candidates) != 1 || step.Candidates[0].P != 1 || step.Candidates[0].Text != "goal" {
		t.Fatalf("next: %v %+v", err, step)
	}
	out, _ := json.Marshal(step)
	if strings.Contains(string(out), "NaN") {
		t.Fatal(string(out))
	}
	scores, err := client.Score(ctx, prompt(engineTask), "", []string{"goal", "classes"})
	if err != nil || len(scores) != 2 || scores[0].Tokens != 1 || scores[0].LogP >= 0 {
		t.Fatalf("score: %v %+v", err, scores)
	}
	raw, err := m.Raw(prompt(engineTask), 5, "")
	if err != nil || strings.Contains(raw, "<") {
		t.Fatalf("сырой вывод %q: %v", raw, err)
	}
}
