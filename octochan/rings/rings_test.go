package rings

import (
	"math"
	"strings"
	"testing"
)

// round2 — до сотых, как числа в манифесте.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

func TestReliabilityMatchesManifesto(t *testing.T) {
	// Числа приложения А.7.
	cases := []struct {
		name            string
		successes, uses int
		prior           Prior
		wantL, wantU    float64
	}{
		{"270 успехов из 300", 270, 300, Prior{}, 0.86, 0.93},
		{"3 из 3", 3, 3, Prior{}, 0.40, 0.99},
		{"условные «8 из 8» без журнала", 0, 0, Prior{Successes: 8, Uses: 8}, 0.66, 1.00},
	}
	for _, c := range cases {
		got := Reliability(c.successes, c.uses, c.prior)
		if round2(got.L) != c.wantL || round2(got.U) != c.wantU {
			t.Errorf("%s: [%.4f; %.4f], в манифесте [%.2f; %.2f]", c.name, got.L, got.U, c.wantL, c.wantU)
		}
	}
}

func TestBetaQuantileClosedForm(t *testing.T) {
	// У Beta(a, 1) функция распределения xᵃ: квантиль считается точно.
	for _, a := range []float64{1, 4, 9} {
		for _, p := range []float64{0.025, 0.5, 0.975} {
			want := math.Pow(p, 1/a)
			if got := BetaQuantile(p, a, 1); math.Abs(got-want) > 1e-9 {
				t.Errorf("Beta(%v, 1), p = %v: %.12f, точно %.12f", a, p, got, want)
			}
		}
	}
	// Симметричное распределение: медиана — ровно середина.
	if got := BetaQuantile(0.5, 7, 7); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("медиана Beta(7, 7) = %v", got)
	}
}

func TestNarrowerWithMoreEvidence(t *testing.T) {
	// Чем чаще правило проверено, тем уже интервал (раздел 6.2).
	few := Reliability(9, 10, Prior{})
	many := Reliability(900, 1000, Prior{})
	if many.Width() >= few.Width() {
		t.Errorf("1000 проверок дали интервал не уже 10: %.3f против %.3f", many.Width(), few.Width())
	}
}

func TestDeriveAndPaths(t *testing.T) {
	// Поддержка вывода — надёжность правила, умноженная на посылки.
	got := Derive(Interval{0.9, 0.95}, Interval{0.8, 0.9}, Certain)
	if math.Abs(got.L-0.72) > 1e-12 || math.Abs(got.U-0.855) > 1e-12 {
		t.Errorf("вывод: %+v", got)
	}

	// Пути 0,7 и 0,6 независимы: 1 − 0,3·0,4 = 0,88.
	ind := Independent(Interval{0.7, 0.7}, Interval{0.6, 0.6})
	if math.Abs(ind.L-0.88) > 1e-12 {
		t.Errorf("независимые пути: %+v, в манифесте 0,88", ind)
	}
	// С общими посылками нижней границей служит только максимум, 0,7.
	shared := Shared(Interval{0.7, 0.7}, Interval{0.6, 0.6})
	if shared.L != 0.7 || shared.U != 1 {
		t.Errorf("пути с общими посылками: %+v, ожидалось [0,7; 1]", shared)
	}
}

func TestChainLosesConfidence(t *testing.T) {
	// Двадцать шагов по 0,97 дают около 0,54 (раздел 6.2).
	steps := make([]Interval, 20)
	for i := range steps {
		steps[i] = Interval{0.97, 0.97}
	}
	if got := round2(Chain(steps...).L); got != 0.54 {
		t.Errorf("план из 20 шагов по 0,97: %.2f, в манифесте 0,54", got)
	}
}

func TestRing0Bound(t *testing.T) {
	// τ пропускает не больше ⌊1/τ⌋ токенов (утверждение 6).
	if MaxPass(0.8) != 1 || MaxPass(0.1) != 10 || MaxPass(0.25) != 4 {
		t.Errorf("⌊1/τ⌋: %d %d %d", MaxPass(0.8), MaxPass(0.1), MaxPass(0.25))
	}
	dist := []float64{0.05, 0.3, 0.3, 0.2, 0.15}
	if got := Ring0(dist, 0.2); len(got) != 3 || len(got) > MaxPass(0.2) {
		t.Errorf("кольцо 0 при τ = 0,2: %v", got)
	}
	if got := Ring0(dist, 0.8); len(got) != 0 {
		t.Errorf("порог 0,8 никто не прошёл, а кольцо пропустило %v", got)
	}
}

func TestThresholdIncoherentWithNegation(t *testing.T) {
	// Утверждение 8: при x = 1/2 порог либо пропускает и x, и 1 − x, либо
	// ни одного. Поэтому отвергнутый пакет не идёт в кольцо 3 отрицанием.
	half := Interval{L: 0.5, U: 0.5}
	for _, tau := range []float64{0.3, 0.5, 0.7} {
		if Accept(half, tau) != Accept(Interval{L: 1 - 0.5, U: 1 - 0.5}, tau) {
			t.Errorf("τ = %v: решения по x и 1 − x противоположны — с отрицанием порог согласован?", tau)
		}
	}
	// И с конъюнкцией: два пакета по 0,9 проходят 0,85, их конъюнкция — нет.
	a, b := Interval{0.9, 0.9}, Interval{0.9, 0.9}
	if !Accept(a, 0.85) || !Accept(b, 0.85) || Accept(Chain(a, b), 0.85) {
		t.Error("порог и конъюнкция повели себя не так, как в приложении А.7")
	}
}

func TestRunSplitsWellTrueAndWellFalse(t *testing.T) {
	packages := []*Package{
		{Statement: "перечитать конфигурацию на web-3", Support: Interval{0.86, 0.93}},
		{Statement: "кандидат до подтверждения", Support: Interval{0.40, 0.99}},
		{Statement: "перезапустить nginx на web-3", Support: Interval{0.88, 0.95}},
	}
	// Кольцо 3: перезапуск противоречит правилу «никогда не разрывать
	// соединения» и отношению скилла «перезапуск разрывает соединения».
	ring3 := func(p *Package, accepted []*Package) string {
		if strings.Contains(p.Statement, "перезапустить") {
			return "противоречит c3: перезапуск разрывает соединения"
		}
		return ""
	}

	accepted, rejected := Run(packages, 0.6, ring3)
	if len(accepted) != 1 || accepted[0] != packages[0] {
		t.Fatalf("принято: %v", accepted)
	}
	if packages[1].Verdict != WellFalse {
		t.Errorf("мало проверенное правило прошло кольцо 2: %s", packages[1].Verdict)
	}
	if packages[2].Verdict != Contradiction || packages[2].Reason == "" {
		t.Errorf("противоречие не поймано кольцом 3: %s %q", packages[2].Verdict, packages[2].Reason)
	}
	if len(rejected) != 2 {
		t.Errorf("отвергнуто %d, ожидалось 2", len(rejected))
	}
}

func TestTrustedBypassesRing2ButNotRing3(t *testing.T) {
	// Оператор принял мало проверенный пакет: кольцо 2 его пропускает.
	weak := &Package{Statement: "store prod_conf", Support: Interval{0.40, 0.99}, Trusted: true}
	// Но жёсткое правило оператор своим доверием не отменяет.
	forbidden := &Package{Statement: "перезапустить", Support: Interval{0.40, 0.99}, Trusted: true}
	ring3 := func(p *Package, _ []*Package) string {
		if p.Statement == "перезапустить" {
			return "нарушает правило «никогда»"
		}
		return ""
	}
	accepted, rejected := Run([]*Package{weak, forbidden}, 0.6, ring3)
	if len(accepted) != 1 || accepted[0] != weak {
		t.Errorf("пакет, принятый оператором, не прошёл кольцо 2: %v", accepted)
	}
	if len(rejected) != 1 || rejected[0].Verdict != Contradiction {
		t.Errorf("доверие оператора отменило кольцо 3: %v", rejected)
	}
}

func TestRing3SeesAcceptedPackages(t *testing.T) {
	// Принятый пакет — предпосылка следующих: кольцо 3 ловит противоречие
	// внутри плана («перезапустить» после «соединения сохраняем»).
	packages := []*Package{
		{Statement: "соединения сохраняем", Support: Interval{0.9, 0.95}},
		{Statement: "перезапустить", Support: Interval{0.9, 0.95}},
	}
	ring3 := func(p *Package, accepted []*Package) string {
		for _, a := range accepted {
			if a.Statement == "соединения сохраняем" && p.Statement == "перезапустить" {
				return "противоречит принятому: " + a.Statement
			}
		}
		return ""
	}
	if _, rejected := Run(packages, 0.6, ring3); len(rejected) != 1 || rejected[0] != packages[1] {
		t.Errorf("противоречие внутри плана не поймано: %v", rejected)
	}
}

func TestChooseIsDeterministic(t *testing.T) {
	strong := &Package{Statement: "a", Support: Interval{0.86, 0.93}}
	weak := &Package{Statement: "b", Support: Interval{0.40, 0.99}}
	best, gap, ok := Choose(DefaultWeights, []Candidate{{Package: weak}, {Package: strong}})
	if !ok || best.Package != strong || gap <= 0 {
		t.Errorf("выбран %v, разрыв %v", best.Package, gap)
	}
	// Равные оценки — по порядку предложения, а не случайно.
	x := &Package{Statement: "x", Support: Interval{0.5, 0.6}}
	y := &Package{Statement: "y", Support: Interval{0.5, 0.6}}
	if best, gap, _ := Choose(DefaultWeights, []Candidate{{Package: x}, {Package: y}}); best.Package != x || gap != 0 {
		t.Errorf("при равенстве выбран %v, разрыв %v", best.Package, gap)
	}
}

func TestConceptWeights(t *testing.T) {
	// В задаче с nginx больше всех весят «узел» и «группа web»: на них
	// опираются почти все утверждения.
	premises := [][]string{
		{"узел", "группа_web"},
		{"узел", "группа_web", "конфигурация"},
		{"узел", "проверка"},
		{"узел", "группа_web", "окно"},
		// Источник без общих понятий с задачей.
		{"печенье", "рецепт"},
	}
	w := ConceptWeights(premises)
	if !(w["узел"] > w["конфигурация"] && w["группа_web"] > w["проверка"]) {
		t.Errorf("веса понятий: %v", w)
	}
	if w["печенье"] != 0 || w["рецепт"] != 0 {
		t.Errorf("оторванная часть графа получила вес: печенье %v, рецепт %v", w["печенье"], w["рецепт"])
	}
	sum := 0.0
	for _, v := range w {
		sum += v
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("сумма весов %v, должна быть 1", sum)
	}

	d := Degrees(premises)
	if g := Binding([]string{"узел", "группа_web"}, w, d, nil); g <= Binding([]string{"окно"}, w, d, nil) {
		t.Errorf("пакет о центральных понятиях привязан слабее периферийного: %v", g)
	}
	if g := Binding([]string{"печенье"}, w, d, nil); g != 0 {
		t.Errorf("пакет из оторванного источника привязан: %v", g)
	}
}

func TestCalibrate(t *testing.T) {
	scores := []float64{0.72, 0.75, 0.78, 0.71, 0.15}
	correct := []bool{true, true, true, false, false}
	bins := Calibrate(scores, correct, 10)
	if bins[7].Count != 4 || bins[7].Correct != 3 || bins[7].Observed() != 0.75 {
		t.Errorf("интервал 0,7–0,8: %+v", bins[7])
	}
	if bins[1].Count != 1 || bins[1].Observed() != 0 {
		t.Errorf("интервал 0,1–0,2: %+v", bins[1])
	}
}

// Поддержка плана — по различным правилам: правило, вызванное
// пятьдесят раз, — один множитель, а не пятьдесят.
func TestPlanSupport(t *testing.T) {
	strong := Interval{L: 0.97, U: 0.99}
	weak := Interval{L: 0.6, U: 0.9}
	var packages []*Package
	for i := 0; i < 50; i++ {
		packages = append(packages, &Package{Rules: []string{"pg.tune"}, Support: strong})
	}
	packages = append(packages, &Package{Rules: []string{"write"}, Support: weak},
		&Package{Rules: []string{"pg.verify"}, Support: Interval{L: 0.8, U: 0.95}},
		&Package{Rules: []string{"pg.restart"}, Support: Interval{L: 0.1, U: 0.5}, Trusted: true})

	got := PlanSupport(packages)
	if math.Abs(got.L-0.97*0.6*0.8) > 1e-9 || math.Abs(got.U-0.99*0.9*0.95) > 1e-9 {
		t.Fatalf("поддержка плана: %+v", got)
	}
	if w := Weakest(packages, 2); len(w) != 2 || w[0] != "write" || w[1] != "pg.verify" {
		t.Fatalf("слабейшие: %v", w)
	}
}
