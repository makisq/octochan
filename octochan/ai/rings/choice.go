package rings

import (
	"math"
	"sort"
)

// Weights — веса правила выбора между пакетами, прошедшими кольца
// (приложение А.7):
//
//	z(a) = β₀ + β₁·log p₀(a) + β₂·L(a) + β₃·(U(a) − L(a)) + β₄·G(a),
//	P(a) = 1 / (1 + e^(−z(a))).
//
// Веса определяет калибровка по журналу, а пока журнал мал, их задаёт автор
// скилла. Заранее ни одному кольцу приоритет не даётся.
type Weights struct {
	Bias    float64 // β₀
	LogProb float64 // β₁: средний логарифм вероятности токенов пакета (кольцо 0)
	Lower   float64 // β₂: нижняя граница поддержки (кольцо 1)
	Width   float64 // β₃: ширина интервала поддержки — чем уже, тем лучше
	Binding float64 // β₄: привязка понятий пакета к предпосылкам
}

// DefaultWeights — веса до калибровки: единичные по модулю, ширина
// интервала со знаком минус.
var DefaultWeights = Weights{LogProb: 1, Lower: 1, Width: -1, Binding: 1}

// Candidate — пакет, прошедший кольца, с величинами для правила выбора.
type Candidate struct {
	Package *Package
	// MeanLogProb — средний логарифм вероятности токенов пакета. В режиме
	// протокола его нет — 0.
	MeanLogProb float64
	// Binding — привязка понятий пакета к предпосылкам, G(a).
	Binding float64
}

// Score — z(a) и итоговая оценка P(a) = σ(z).
func Score(w Weights, c Candidate) (z, p float64) {
	s := c.Package.Support
	z = w.Bias + w.LogProb*c.MeanLogProb + w.Lower*s.L + w.Width*s.Width() + w.Binding*c.Binding
	return z, 1 / (1 + math.Exp(-z))
}

// Choose выбирает пакет правилом, без участия модели, и возвращает разрыв
// между лучшим и вторым: если они почти равны, разрыв пишется в журнал.
// При равных оценках выбор детерминирован — по порядку предложения.
func Choose(w Weights, candidates []Candidate) (best Candidate, gap float64, ok bool) {
	if len(candidates) == 0 {
		return Candidate{}, 0, false
	}
	type scored struct {
		c Candidate
		p float64
		i int
	}
	all := make([]scored, len(candidates))
	for i, c := range candidates {
		_, p := Score(w, c)
		all[i] = scored{c, p, i}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].p > all[j].p })
	if len(all) > 1 {
		gap = all[0].p - all[1].p
	}
	return all[0].c, gap, true
}

// ConceptWeights — веса понятий: компоненты главного собственного вектора
// матрицы совместной встречаемости M, нормированного на сумму 1. Понятия —
// вершины, два понятия связаны, если встречаются в одной предпосылке;
// M_uv — число таких предпосылок, M_uu = 0.
//
// Если граф распадается на части, главный вектор сосредоточен на одной из
// них, а понятия остальных получают нулевой вес: это источник без общих
// понятий с задачей (раздел 5.2).
func ConceptWeights(premises [][]string) map[string]float64 {
	index := map[string]int{}
	var names []string
	for _, premise := range premises {
		for _, u := range premise {
			if _, ok := index[u]; !ok {
				index[u] = len(names)
				names = append(names, u)
			}
		}
	}
	n := len(names)
	if n == 0 {
		return map[string]float64{}
	}
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
	}
	for _, premise := range premises {
		seen := map[string]bool{}
		var uniq []string
		for _, u := range premise {
			if !seen[u] {
				seen[u] = true
				uniq = append(uniq, u)
			}
		}
		for i := range uniq {
			for j := range uniq {
				if i != j {
					m[index[uniq[i]]][index[uniq[j]]]++
				}
			}
		}
	}

	// Степенной метод для M + I: сдвиг не меняет собственных векторов, но
	// убирает колебания на двудольных графах, где у M есть −λ рядом с λ.
	v := make([]float64, n)
	for i := range v {
		v[i] = 1 / float64(n)
	}
	for iter := 0; iter < 10000; iter++ {
		next := make([]float64, n)
		sum := 0.0
		for i := 0; i < n; i++ {
			acc := v[i]
			for j := 0; j < n; j++ {
				acc += m[i][j] * v[j]
			}
			next[i] = acc
			sum += acc
		}
		diff := 0.0
		for i := range next {
			next[i] /= sum
			diff += math.Abs(next[i] - v[i])
		}
		v = next
		if diff < 1e-13 {
			break
		}
	}

	out := make(map[string]float64, n)
	for i, name := range names {
		// Остаток от сдвига на отпавших частях графа убывает
		// геометрически; всё, что ниже шума, — ноль.
		if v[i] < 1e-9 {
			v[i] = 0
		}
		out[name] = v[i]
	}
	return out
}

// Degrees — доля предпосылок, в которых встречается понятие, от наибольшей.
func Degrees(premises [][]string) map[string]float64 {
	count := map[string]float64{}
	top := 0.0
	for _, premise := range premises {
		seen := map[string]bool{}
		for _, u := range premise {
			if !seen[u] {
				seen[u] = true
				count[u]++
				top = math.Max(top, count[u])
			}
		}
	}
	for u := range count {
		count[u] /= top
	}
	return count
}

// Binding — привязка понятий пакета к предпосылкам:
//
//	G(a) = ∑ w(u)·max_v σ(u, v)·d(v) / ∑ w(u),  u ∈ a.
//
// similarity — таблица близости понятий («перечитать» и reload); nil —
// понятие близко только самому себе.
func Binding(concepts []string, weights, degree map[string]float64, similarity func(u, v string) float64) float64 {
	if similarity == nil {
		similarity = func(u, v string) float64 {
			if u == v {
				return 1
			}
			return 0
		}
	}
	num, den := 0.0, 0.0
	for _, u := range concepts {
		best := 0.0
		for v, d := range degree {
			best = math.Max(best, similarity(u, v)*d)
		}
		num += weights[u] * best
		den += weights[u]
	}
	if den == 0 {
		return 0
	}
	return num / den
}

// Bin — интервал оценок и доля верных решений в нём.
type Bin struct {
	From, To float64
	Count    int
	Correct  int
}

// Observed — наблюдаемая доля верных решений в интервале.
func (b Bin) Observed() float64 {
	if b.Count == 0 {
		return 0
	}
	return float64(b.Correct) / float64(b.Count)
}

// Calibrate раскладывает оценки по интервалам равной ширины и считает в
// каждом наблюдаемую долю верных решений (раздел 7.2): если для оценок
// 0,7–0,8 доля около 75 %, шкала откалибрована, если нет — поправка
// «оценка → наблюдаемая доля» её исправляет.
func Calibrate(scores []float64, correct []bool, bins int) []Bin {
	if bins <= 0 {
		bins = 10
	}
	out := make([]Bin, bins)
	for i := range out {
		out[i].From = float64(i) / float64(bins)
		out[i].To = float64(i+1) / float64(bins)
	}
	for i, s := range scores {
		k := int(s * float64(bins))
		k = min(max(k, 0), bins-1)
		out[k].Count++
		if i < len(correct) && correct[i] {
			out[k].Correct++
		}
	}
	return out
}
