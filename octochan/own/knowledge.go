package own

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ochan/fp"
)

// Пакеты знаний своей модели — через реестр ochan, с подписью.
//
// Знания — то, чему модель научилась на одной машине и что годится другой:
// обобщения задач, которые повторялись (ochan llm learn), и как на их
// структуре работали правила вывода. Частных случаев в пакете нет:
// обобщение — факты fp.Shape без частных имён, вместо номеров инцидентов —
// их число. Пакет знаний — обычный пакет реестра: package.yaml с версией и
// knowledge.yaml (fp.ParseKnowledge).
//
// Модель берёт знания пакета только для задачи, которая его подключает —
// import @пакет v1, тот же import, что у скилла, — и повторяет его
// обобщение. Версия — часть знания: задача просит v1, а в каталоге 2.0 —
// знаний этой версии у модели нет, и это недостающая предпосылка.

const (
	manifestFile = "package.yaml"
	maxKnowledge = 64 << 20
)

var manifestVersion = regexp.MustCompile(`^v?(\d+(?:\.\d+){0,2})$`)

type knownPackage struct {
	version string
	gens    []*fp.KnownGeneralization
}

// Library — пакеты знаний в каталоге модулей ochan. Каталог перечитывается
// не чаще раза в секунду и только когда в нём что-то поменялось: пакет
// поставили, обновили или убрали, а модель работает дальше.
type Library struct {
	Modules string

	mu         sync.Mutex
	packages   map[string]knownPackage
	errors     map[string]string
	generation int
	stamp      string
	checked    time.Time
}

// NewLibrary — пакеты знаний каталога модулей; пусто — знаний нет.
func NewLibrary(modules string) *Library {
	return &Library{Modules: modules, packages: map[string]knownPackage{}, errors: map[string]string{}}
}

func (l *Library) refresh() {
	if l.Modules == "" || time.Since(l.checked) < time.Second {
		return
	}
	l.checked = time.Now()
	var stamp []string
	entries, _ := os.ReadDir(l.Modules)
	for _, e := range entries {
		file := filepath.Join(l.Modules, e.Name(), fp.KnowledgeName)
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		manifest := int64(0)
		if m, err := os.Stat(filepath.Join(l.Modules, e.Name(), manifestFile)); err == nil {
			manifest = m.ModTime().UnixNano()
		}
		stamp = append(stamp, fmt.Sprintf("%s|%d|%d|%d", e.Name(), info.ModTime().UnixNano(), info.Size(), manifest))
	}
	sort.Strings(stamp)
	joined := strings.Join(stamp, "\n")
	if joined == l.stamp && l.generation > 0 {
		return
	}
	packages, errs := map[string]knownPackage{}, map[string]string{}
	for _, s := range stamp {
		name, _, _ := strings.Cut(s, "|")
		dir := filepath.Join(l.Modules, name)
		file := filepath.Join(dir, fp.KnowledgeName)
		if info, err := os.Stat(file); err == nil && info.Size() > maxKnowledge {
			errs[name] = fmt.Sprintf("%s больше %d МиБ", file, maxKnowledge>>20)
			continue
		}
		k, err := fp.LoadKnowledge(file)
		if err != nil {
			errs[name] = err.Error()
			continue
		}
		version := packageVersion(dir)
		if version == "" {
			errs[name] = fmt.Sprintf("%s: у пакета знаний нет версии (%s)", dir, manifestFile)
			continue
		}
		packages[name] = knownPackage{version, k.Generalizations}
	}
	l.packages, l.errors, l.stamp = packages, errs, joined
	l.generation++
}

// packageVersion — версия пакета из package.yaml (строка «version: …»).
func packageVersion(dir string) string {
	f, err := os.Open(filepath.Join(dir, manifestFile))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimPrefix(sc.Text(), "\uFEFF"), ":")
		if ok && strings.TrimSpace(key) == "version" {
			if m := manifestVersion.FindStringSubmatch(strings.TrimSpace(value)); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// Generation — номер прочтения каталога: меняется, когда меняются пакеты.
func (l *Library) Generation() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh()
	return l.generation
}

// Listed — пакеты знаний, которые видит модель: «имя версия».
func (l *Library) Listed() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh()
	var out []string
	for name, p := range l.packages {
		out = append(out, name+" "+p.version)
	}
	sort.Strings(out)
	return out
}

// ForTask — обобщения пакетов, которые задача подключает, — как кластеры,
// и недостающее. Пакет без знаний — скилл: он не модели.
func (l *Library) ForTask(prog *fp.Program) (map[string]*Cluster, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh()
	clusters := map[string]*Cluster{}
	var missing []string
	for _, imp := range prog.Imports {
		if msg, bad := l.errors[imp.Skill]; bad {
			missing = append(missing, fmt.Sprintf("@%s: знания пакета негодны — %s", imp.Skill, msg))
			continue
		}
		p, ok := l.packages[imp.Skill]
		if !ok {
			continue
		}
		if !fp.VersionMatches(imp.Version, p.version) {
			missing = append(missing, fmt.Sprintf("@%s %s: знаний этой версии у модели нет — в каталоге %s", imp.Skill, imp.Version, p.version))
			continue
		}
		for _, g := range p.gens {
			c := &Cluster{Shape: append([]string(nil), g.Shape...), Similarity: g.Similarity,
				Instances: Instances{Count: g.Instances}, Rules: map[string]Counts{}}
			sort.Strings(c.Shape)
			if len(g.Outcomes) == 2 {
				c.Outcomes = Counts{g.Outcomes[0], g.Outcomes[1]}
			}
			for r, counts := range g.Rules {
				if len(counts) == 2 {
					c.Rules[r] = Counts{counts[0], counts[1]}
				}
			}
			clusters["@"+imp.Skill+"/"+g.Key] = c
		}
	}
	return clusters, missing
}
