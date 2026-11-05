package rings

import (
	"math"
	"sort"
)

// Share — доля прочтения: k из n, 95-процентный интервал Beta(k + 1,
// n − k + 1) — без условного счёта автора.
func Share(k, n float64) Interval { return Reliability(k, n, Prior{}) }

// Point — точка калибровки: оценка и наблюдаемая доля верных при ней.
type Point struct{ Score, Observed float64 }

// Calibration — монотонная поправка «оценка → наблюдаемая доля»: кусочно
// линейная по точкам. Пустая — без поправки.
type Calibration []Point

// Apply — оценка после поправки. Левее первой точки и правее последней —
// их доли.
func (c Calibration) Apply(score float64) float64 {
	if len(c) == 0 {
		return score
	}
	points := append(Calibration(nil), c...)
	sort.SliceStable(points, func(i, j int) bool {
		if points[i].Score != points[j].Score {
			return points[i].Score < points[j].Score
		}
		return points[i].Observed < points[j].Observed
	})
	if score <= points[0].Score {
		return points[0].Observed
	}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if score <= b.Score {
			if b.Score > a.Score {
				return a.Observed + (b.Observed-a.Observed)*(score-a.Score)/(b.Score-a.Score)
			}
			return b.Observed
		}
	}
	return points[len(points)-1].Observed
}

// Isotonic — калибровка по журналу: доля верных по интервалам оценок
// (Calibrate), сглаженная до монотонной объединением соседних нарушителей.
// Точка — средняя оценка блока и его наблюдаемая доля.
func Isotonic(scores []float64, correct []bool, bins int) Calibration {
	if bins <= 0 {
		bins = 10
	}
	type block struct{ ok, n, sum float64 }
	groups := make([]block, bins)
	for i, s := range scores {
		k := min(max(int(s*float64(bins)), 0), bins-1)
		if i < len(correct) && correct[i] {
			groups[k].ok++
		}
		groups[k].n++
		groups[k].sum += s
	}
	var merged []block
	for _, g := range groups {
		if g.n == 0 {
			continue
		}
		merged = append(merged, g)
		for len(merged) > 1 {
			a, b := merged[len(merged)-2], merged[len(merged)-1]
			if a.ok/a.n <= b.ok/b.n {
				break
			}
			merged = append(merged[:len(merged)-2], block{a.ok + b.ok, a.n + b.n, a.sum + b.sum})
		}
	}
	out := make(Calibration, len(merged))
	for i, b := range merged {
		out[i] = Point{b.sum / b.n, b.ok / b.n}
	}
	return out
}

// Features — признаки выбора для правила выбора: средний логарифм
// вероятности, нижняя граница поддержки, её ширина, привязка G.
type Features struct{ LogProb, Lower, Width, Binding float64 }

// Z — z(a) правила выбора.
func (w Weights) Z(f Features) float64 {
	return w.Bias + w.LogProb*f.LogProb + w.Lower*f.Lower + w.Width*f.Width + w.Binding*f.Binding
}

// sigmoid — σ(z); z ограничен ±60, чтобы exp не уходил в бесконечность.
func sigmoid(z float64) float64 { return 1 / (1 + math.Exp(-math.Max(-60, math.Min(60, z)))) }

// Probability — оценка P(a) = σ(z) с поправкой калибровки.
func (w Weights) Probability(f Features, c Calibration) float64 { return c.Apply(sigmoid(w.Z(f))) }

// FitWeights — веса правила выбора по исходам журнала: логистическая
// регрессия градиентным спуском (2000 шагов, шаг 0,1, L2 10⁻³) от весов
// по умолчанию. Мало исходов — меньше 20 или все одинаковые — ok ложно:
// остаются веса автора.
func FitWeights(features []Features, correct []bool) (w Weights, ok bool) {
	if len(features) < 20 || len(features) != len(correct) {
		return DefaultWeights, false
	}
	var pos int
	for _, c := range correct {
		if c {
			pos++
		}
	}
	if pos == 0 || pos == len(correct) {
		return DefaultWeights, false
	}
	const steps, lr, l2 = 2000, 0.1, 1e-3
	b := [5]float64{0, 1, 1, -1, 1}
	n := float64(len(features))
	for s := 0; s < steps; s++ {
		var grad [5]float64
		for i, f := range features {
			x := [5]float64{1, f.LogProb, f.Lower, f.Width, f.Binding}
			z := b[0] + b[1]*x[1] + b[2]*x[2] + b[3]*x[3] + b[4]*x[4]
			y := 0.0
			if correct[i] {
				y = 1
			}
			p := sigmoid(z)
			for k := range grad {
				grad[k] += (p - y) * x[k]
			}
		}
		for k := range b {
			b[k] -= lr * (grad[k]/n + l2*b[k])
		}
	}
	return Weights{Bias: b[0], LogProb: b[1], Lower: b[2], Width: b[3], Binding: b[4]}, true
}
