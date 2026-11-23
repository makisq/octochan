package fp

import (
	"sort"
	"strings"
)

// Shape — структура формального промпта без частных имён: факты, по
// которым похожи записи (инциденты), — цель, классы, отношения между
// классами, приоритеты, жёсткие правила, обобщения, частные случаи по их
// обобщению и правки «Цель.атрибут» с видом значения. Имена частных
// случаев — стрелок, ячеек, значений — в факты не входят: две записи одной
// структуры на разных хостах дают одни и те же факты. isWord — слово
// языка: такое значение со смыслом и остаётся в факте; nil — все
// значения-имена неразличимы.
func Shape(prog *Program, isWord func(string) bool) []string {
	facts := map[string]bool{}
	classOf := map[string]string{}
	for _, c := range prog.Classes {
		facts["class "+c.Name] = true
		for _, w := range c.Words {
			classOf[w.Text] = c.Name
		}
	}
	if prog.Goal != nil {
		facts["goal "+classOf[prog.Goal.Word]] = true
	}
	for _, r := range prog.Relations {
		facts["relation "+r.From+" "+r.Type+" "+r.To] = true
	}
	for _, p := range prog.Priorities {
		facts["priority "+p.Higher+" < "+p.Lower] = true
	}
	for _, r := range prog.Rules {
		facts["rule "+string(r.Kind)+" "+r.Action] = true
	}
	for _, g := range prog.Generalizations {
		facts["generalization "+g.Result+" = "+g.Op.Text] = true
	}
	particular := map[string]bool{}
	for _, i := range prog.Instances {
		particular[i.Name] = true
		facts["instance of "+i.Of.Text] = true
	}
	for _, cell := range prog.Cells {
		for _, set := range cell.Sets {
			root, attr, _ := strings.Cut(set.Target, ".")
			if particular[root] {
				root = "_"
			}
			facts["set "+root+"."+attr+" "+valueKind(set.Value, isWord)] = true
		}
	}
	out := make([]string, 0, len(facts))
	for f := range facts {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// valueKind — вид значения правки: число с единицей, неточное, слово
// языка или частное имя.
func valueKind(v Value, isWord func(string) bool) string {
	switch {
	case v.Range != nil:
		return ".." + v.Range.Unit
	case len(v.Variants) > 0:
		return "{…}"
	}
	if _, unit, ok := ParseNumber(v.Text); ok {
		return "#" + unit
	}
	if isWord != nil && isWord(v.Text) {
		return v.Text
	}
	return "_"
}

// Similarity — похожесть двух структур: 1 − δ, δ — расстояние Жаккара по
// фактам с весом 1 (А.5 манифеста). Пустые — одинаковы.
func Similarity(a, b []string) float64 {
	in := map[string]int{}
	for _, f := range a {
		in[f] |= 1
	}
	for _, f := range b {
		in[f] |= 2
	}
	if len(in) == 0 {
		return 1
	}
	both := 0
	for _, mark := range in {
		if mark == 3 {
			both++
		}
	}
	return float64(both) / float64(len(in))
}
