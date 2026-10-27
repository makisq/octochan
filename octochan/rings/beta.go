package rings

import "math"

// Interval — интервал поддержки [L, U] на отрезке [0, 1].
type Interval struct {
	L, U float64
}

// Width — ширина интервала: чем чаще правило проверено, тем она меньше.
func (i Interval) Width() float64 { return i.U - i.L }

// Certain — интервал установленного факта: принятый пакет становится
// предпосылкой следующих с единицей (раздел 6.2).
var Certain = Interval{L: 1, U: 1}

// Prior — условный счёт автора скилла: «как будто правило применялось Uses
// раз и Successes из них прошли» (раздел 7.5). Пустой — без условного
// счёта, то есть a = b = 1.
type Prior struct {
	Successes int
	Uses      int
}

// Reliability — надёжность правила: 95-процентный интервал
// бета-распределения Beta(k + a, n − k + b), где правило применялось
// uses раз и successes из этих планов прошли арбитра без правок оператора,
// а a = 1 + prior.Successes и b = 1 + (prior.Uses − prior.Successes) —
// условный счёт автора (приложение А.7).
//
// Для 270 успехов из 300 это [0,86; 0,93], для 3 из 3 — [0,40; 0,99], для
// условных «8 из 8» без журнала — [0,66; 1,00].
func Reliability(successes, uses int, prior Prior) Interval {
	a := float64(1 + successes + prior.Successes)
	b := float64(1 + (uses - successes) + (prior.Uses - prior.Successes))
	return Interval{L: BetaQuantile(0.025, a, b), U: BetaQuantile(0.975, a, b)}
}

// BetaQuantile — квантиль уровня p распределения Beta(a, b): обращение
// регуляризованной неполной бета-функции делением отрезка пополам.
// Сотни итераций с запасом дают точность до машинной.
func BetaQuantile(p, a, b float64) float64 {
	switch {
	case p <= 0:
		return 0
	case p >= 1:
		return 1
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if betaRegularized(mid, a, b) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// betaRegularized — регуляризованная неполная бета-функция I_x(a, b):
// функция распределения Beta(a, b) в точке x.
func betaRegularized(x, a, b float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(a*math.Log(x) + b*math.Log(1-x) + lab - la - lb)
	// Цепная дробь сходится быстро по одну сторону от среднего; по другую
	// считается дополнение через симметрию I_x(a, b) = 1 − I_{1−x}(b, a).
	if x < (a+1)/(a+b+2) {
		return front * betaFraction(x, a, b) / a
	}
	return 1 - front*betaFraction(1-x, b, a)/b
}

// betaFraction — цепная дробь для неполной бета-функции (метод Ленца).
func betaFraction(x, a, b float64) float64 {
	const (
		maxIter = 500
		eps     = 1e-15
		tiny    = 1e-300
	)
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		m2 := 2 * fm

		num := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + num*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + num/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c

		num = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + num*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + num/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		step := d * c
		h *= step
		if math.Abs(step-1) < eps {
			break
		}
	}
	return h
}
