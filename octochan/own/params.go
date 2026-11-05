package own

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ochan/fp"
	"ochan/rings"
)

// Параметры колец своей модели — rings.json рядом с весами (раздел 6.2
// манифеста, приложение А.7). Числа — параметры модели: их пересчитывают
// журнал решений (ochan llm fit) и созревшие кластеры инцидентов (ochan llm
// learn), как обучение пересчитывает веса. Каждая запись — новая версия в
// rings.versions/: прежние остаются, к любой можно откатиться.
//
// Математика — пакет rings, тот же, что судит ответ внешней модели снаружи.

const (
	// ParamsFile — параметры колец модели рядом с весами.
	ParamsFile = "rings.json"
	// VersionsDir — версии параметров.
	VersionsDir = "rings.versions"
	versionsLog = "log.json"
)

// Counts — исходы правила: [успехов, применений], с весом давности.
type Counts [2]float64

// Instances — частные случаи кластера: номера инцидентов своего кластера
// или их число у обобщения из пакета знаний.
type Instances struct {
	IDs   []string
	Count int
}

// N — сколько частных случаев.
func (i Instances) N() int {
	if i.IDs != nil {
		return len(i.IDs)
	}
	return i.Count
}

func (i Instances) MarshalJSON() ([]byte, error) {
	if i.IDs != nil {
		return json.Marshal(i.IDs)
	}
	return json.Marshal(i.Count)
}

func (i *Instances) UnmarshalJSON(data []byte) error {
	if strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
		i.IDs = []string{}
		return json.Unmarshal(data, &i.IDs)
	}
	var f float64
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	i.Count = int(f)
	return nil
}

// Cluster — кластер повторяющихся инцидентов: обобщение (структура без
// частных имён), порог похожести, частные случаи, исходы модели на нём —
// [решила сама, исходов всего] — и исходы правил на этой структуре.
type Cluster struct {
	Shape      []string          `json:"shape"`
	Similarity float64           `json:"similarity"`
	Instances  Instances         `json:"instances"`
	Outcomes   Counts            `json:"outcomes"`
	Rules      map[string]Counts `json:"rules"`
}

// Params — параметры колец модели: из rings.json, иначе по умолчанию.
type Params struct {
	WellTrue float64           `json:"well_true"`
	Prior    [2]float64        `json:"prior"`
	Rules    map[string]Counts `json:"rules"`
	// Weights — β₀..β₄ правила выбора.
	Weights [5]float64 `json:"weights"`
	// Calibration — точки (оценка, наблюдаемая доля), монотонные.
	Calibration [][2]float64 `json:"calibration"`
	// History — история прочтений (раздел 5.2): атрибут → {значение: сколько
	// раз оператор задал его точно или одобрил такое сужение}.
	History  map[string]map[string]float64 `json:"history"`
	Clusters map[string]*Cluster           `json:"clusters"`
}

// DefaultParams — параметры до журнала: порог 0,5, условный счёт автора
// 8 из 8, веса правила выбора по умолчанию.
func DefaultParams() *Params {
	w := rings.DefaultWeights
	return &Params{WellTrue: 0.5, Prior: [2]float64{8, 8}, Rules: map[string]Counts{},
		Weights:     [5]float64{w.Bias, w.LogProb, w.Lower, w.Width, w.Binding},
		Calibration: [][2]float64{}, History: map[string]map[string]float64{}, Clusters: map[string]*Cluster{}}
}

// LoadParams — параметры модели каталога dir.
func LoadParams(dir string) (*Params, error) {
	data, err := os.ReadFile(filepath.Join(dir, ParamsFile))
	if errors.Is(err, os.ErrNotExist) {
		return DefaultParams(), nil
	}
	if err != nil {
		return nil, err
	}
	return parseParams(data)
}

func parseParams(data []byte) (*Params, error) {
	p := DefaultParams()
	if err := json.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("%s: %w", ParamsFile, err)
	}
	for _, m := range []*map[string]Counts{&p.Rules} {
		if *m == nil {
			*m = map[string]Counts{}
		}
	}
	if p.History == nil {
		p.History = map[string]map[string]float64{}
	}
	if p.Clusters == nil {
		p.Clusters = map[string]*Cluster{}
	}
	if p.Calibration == nil {
		p.Calibration = [][2]float64{}
	}
	return p, nil
}

// Prior — условный счёт автора.
func (p *Params) prior() rings.Prior { return rings.Prior{Successes: p.Prior[0], Uses: p.Prior[1]} }

// Rule — надёжность правила: на структуре кластера — по исходам его
// инцидентов, если правило там применялось, иначе по журналу.
func (p *Params) Rule(name string, c *Cluster) rings.Interval {
	counts, ok := Counts{}, false
	if c != nil {
		counts, ok = c.Rules[name]
	}
	if !ok {
		counts = p.Rules[name]
	}
	return rings.Reliability(counts[0], counts[1], p.prior())
}

// Outcomes — надёжность модели на структуре кластера: сколько его
// инцидентов она решила сама — тот же интервал, что у правила.
func (p *Params) Outcomes(c *Cluster) rings.Interval {
	return rings.Reliability(c.Outcomes[0], c.Outcomes[1], p.prior())
}

// ChoiceWeights — веса правила выбора.
func (p *Params) ChoiceWeights() rings.Weights {
	w := p.Weights
	return rings.Weights{Bias: w[0], LogProb: w[1], Lower: w[2], Width: w[3], Binding: w[4]}
}

// SetChoiceWeights — веса правила выбора после пересчёта.
func (p *Params) SetChoiceWeights(w rings.Weights) {
	p.Weights = [5]float64{w.Bias, w.LogProb, w.Lower, w.Width, w.Binding}
}

// CalibrationPoints — калибровка как поправка rings.
func (p *Params) CalibrationPoints() rings.Calibration {
	out := make(rings.Calibration, len(p.Calibration))
	for i, pt := range p.Calibration {
		out[i] = rings.Point{Score: pt[0], Observed: pt[1]}
	}
	return out
}

// SetCalibration — калибровка после пересчёта.
func (p *Params) SetCalibration(c rings.Calibration) {
	p.Calibration = make([][2]float64, len(c))
	for i, pt := range c {
		p.Calibration[i] = [2]float64{pt.Score, pt.Observed}
	}
}

// Score — итоговая оценка пакета: σ(z) правила выбора, затем калибровка.
func (p *Params) Score(f rings.Features) float64 {
	return p.ChoiceWeights().Probability(f, p.CalibrationPoints())
}

// ClusterOf — кластер, обобщение которого повторяет структура задачи:
// самый похожий из тех, на которые она похожа не меньше их порога. extra —
// обобщения пакетов знаний, которые задача подключает; при равной
// похожести свой кластер — раньше: свои исходы ближе к этой машине.
func (p *Params) ClusterOf(shape []string, extra map[string]*Cluster) (string, *Cluster) {
	best, key := -1.0, ""
	var found *Cluster
	for _, group := range []map[string]*Cluster{p.Clusters, extra} {
		keys := make([]string, 0, len(group))
		for k := range group {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c := group[k]
			if s := fp.Similarity(shape, c.Shape); s >= c.Similarity && s > best {
				best, key, found = s, k, c
			}
		}
	}
	return key, found
}

// Version — запись о версии параметров.
type Version struct {
	Version int     `json:"version"`
	Time    *string `json:"time"`
	Note    string  `json:"note"`
}

// Versions — версии параметров модели, от старых.
func Versions(dir string) ([]Version, error) {
	data, err := os.ReadFile(filepath.Join(dir, VersionsDir, versionsLog))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var log []Version
	if err := json.Unmarshal(data, &log); err != nil {
		return nil, fmt.Errorf("%s: %w", versionsLog, err)
	}
	return log, nil
}

// Save пишет rings.json и его версию с заметкой, откуда она: прежние
// версии остаются. rings.json без версий — версия 0.
func (p *Params) Save(dir, note string) (int, error) {
	folder := filepath.Join(dir, VersionsDir)
	log, err := Versions(dir)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return 0, err
	}
	current := filepath.Join(dir, ParamsFile)
	if _, err := os.Stat(current); len(log) == 0 && err == nil {
		if err := copyFile(current, filepath.Join(folder, "0.json")); err != nil {
			return 0, err
		}
		log = append(log, Version{Version: 0, Note: "до версий"})
	}
	n := 1
	if len(log) > 0 {
		n = log[len(log)-1].Version + 1
	}
	data, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return 0, err
	}
	for _, f := range []string{filepath.Join(folder, strconv.Itoa(n)+".json"), current} {
		if err := os.WriteFile(f, data, 0o644); err != nil {
			return 0, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	log = append(log, Version{Version: n, Time: &now, Note: note})
	logData, _ := json.MarshalIndent(log, "", " ")
	return n, os.WriteFile(filepath.Join(folder, versionsLog), logData, 0o644)
}

// Rollback — откат к версии (по умолчанию, version < 0, — к предыдущей):
// она записывается новой версией, так что откат тоже откатывается.
// Возвращает, к какой версии откатились, и номер новой.
func Rollback(dir string, version int) (int, int, error) {
	log, err := Versions(dir)
	if err != nil {
		return 0, 0, err
	}
	if version < 0 {
		if len(log) < 2 {
			return 0, 0, errors.New("откатываться не к чему: версия одна")
		}
		version = log[len(log)-2].Version
	}
	data, err := os.ReadFile(filepath.Join(dir, VersionsDir, strconv.Itoa(version)+".json"))
	if err != nil {
		var have []string
		for _, v := range log {
			have = append(have, strconv.Itoa(v.Version))
		}
		if len(have) == 0 {
			have = []string{"версий нет"}
		}
		return 0, 0, fmt.Errorf("версии %d нет: %s", version, strings.Join(have, ", "))
	}
	p, err := parseParams(data)
	if err != nil {
		return 0, 0, err
	}
	n, err := p.Save(dir, fmt.Sprintf("откат к версии %d", version))
	return version, n, err
}
