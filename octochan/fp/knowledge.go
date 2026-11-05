package fp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// KnowledgeName — знания пакета для своей модели: обобщения задач, которые
// повторялись, и как на них работали правила вывода. Лежит рядом с
// package.yaml; пишет его ochan llm learn --package.
const KnowledgeName = "knowledge.yaml"

// KnowledgeProtocol — версия формата знаний, которую понимает эта сборка.
const KnowledgeProtocol = 1

// Knowledge — знания пакета. Обобщение — структура задачи без частных имён
// (Shape): частные случаи, из которых оно выведено, в пакет не входят, и
// хосты, числа и номера инцидентов другой машины не уходят с ним. Своя
// модель берёт знания для задачи, которая подключает пакет
// (import @пакет v…) и повторяет обобщение: надёжность правил на этой
// структуре — по исходам из пакета, а не в среднем по своему журналу.
type Knowledge struct {
	Protocol        int                    `yaml:"protocol"`
	Generalizations []*KnownGeneralization `yaml:"generalizations"`
}

// KnownGeneralization — обобщение в пакете знаний. Outcomes и Rules —
// [успехов, исходов]: сколько задач этой структуры модель решила сама и
// сколько раз правило на ней было верно.
type KnownGeneralization struct {
	Key        string               `yaml:"key"`
	Shape      []string             `yaml:"shape"`
	Similarity float64              `yaml:"similarity"`
	Instances  int                  `yaml:"instances"`
	Outcomes   []float64            `yaml:"outcomes"`
	Rules      map[string][]float64 `yaml:"rules"`
}

var (
	knowledgeKey  = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	knowledgeRule = regexp.MustCompile(`^fp\.[a-z_]+$`)
)

// ParseKnowledge разбирает и проверяет знания пакета. Неизвестное поле —
// ошибка: опечатка иначе тихо оставила бы модель без знаний.
func ParseKnowledge(source string, data []byte) (*Knowledge, error) {
	var k Knowledge
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&k); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: пуст", source)
		}
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if k.Protocol == 0 {
		k.Protocol = 1
	}
	if k.Protocol > KnowledgeProtocol {
		return nil, fmt.Errorf("%s: знания протокола v%d, эта сборка понимает v%d — обновите ochan", source, k.Protocol, KnowledgeProtocol)
	}
	if len(k.Generalizations) == 0 {
		return nil, fmt.Errorf("%s: нет обобщений", source)
	}
	seen := map[string]bool{}
	for i, g := range k.Generalizations {
		where := fmt.Sprintf("%s: обобщение %d", source, i+1)
		switch {
		case g == nil:
			return nil, fmt.Errorf("%s пусто", where)
		case !knowledgeKey.MatchString(g.Key):
			return nil, fmt.Errorf("%s: ключ %q — буквы, цифры и _, до 64", where, g.Key)
		case seen[g.Key]:
			return nil, fmt.Errorf("%s: ключ %s повторяется", where, g.Key)
		case len(g.Shape) == 0:
			return nil, fmt.Errorf("%s (%s): структура пуста", where, g.Key)
		case !(g.Similarity > 0 && g.Similarity <= 1):
			return nil, fmt.Errorf("%s (%s): similarity %v — доля, больше 0 и не больше 1", where, g.Key, g.Similarity)
		case g.Instances < 0:
			return nil, fmt.Errorf("%s (%s): instances %d — не меньше 0", where, g.Key, g.Instances)
		}
		seen[g.Key] = true
		for _, fact := range g.Shape {
			if strings.TrimSpace(fact) == "" || strings.ContainsAny(fact, "\r\n") {
				return nil, fmt.Errorf("%s (%s): факт структуры %q — одна непустая строка", where, g.Key, fact)
			}
		}
		if err := checkCounts(g.Outcomes); err != nil {
			return nil, fmt.Errorf("%s (%s): outcomes: %w", where, g.Key, err)
		}
		for rule, counts := range g.Rules {
			if !knowledgeRule.MatchString(rule) {
				return nil, fmt.Errorf("%s (%s): правило %q — правило вывода fp.<тип>", where, g.Key, rule)
			}
			if err := checkCounts(counts); err != nil {
				return nil, fmt.Errorf("%s (%s): %s: %w", where, g.Key, rule, err)
			}
		}
	}
	return &k, nil
}

// checkCounts — [успехов, исходов]: два конечных числа, 0 ≤ успехов ≤ исходов.
func checkCounts(c []float64) error {
	if len(c) != 2 {
		return fmt.Errorf("ожидалось [успехов, исходов], получено %d чисел", len(c))
	}
	for _, x := range c {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("число не конечно")
		}
	}
	if c[0] < 0 || c[0] > c[1] {
		return fmt.Errorf("[%g, %g]: успехов от 0 до числа исходов", c[0], c[1])
	}
	return nil
}

// LoadKnowledge читает знания пакета из файла.
func LoadKnowledge(path string) (*Knowledge, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKnowledge(path, data)
}
