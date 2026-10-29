// Package rings — кольца проверок внутри генерации и поверх готового плана
// (раздел 6.2 манифеста ФПм v0.4, приложение А.7).
//
// Модель выдаёт вероятности, а формальные проверки отвечают «да» или «нет».
// Кольца — цепочка фильтров между тем и другим; каждое следующее пройти
// труднее, и то, что не прошло, дальше не участвует:
//
//	кольцо 0 — порог τ на вероятность токена;
//	кольцо 1 — интервал поддержки пакета: надёжность правил по журналу,
//	           умноженная на поддержку посылок;
//	кольцо 2 — бинаризация: нижняя граница поддержки сравнивается с порогом
//	           well_True; не прошедшее попадает в well_False и отбрасывается
//	           целиком — ни само, ни в виде отрицания дальше не участвует;
//	кольцо 3 — булева проверка принятого пакета на противоречие с точными
//	           предпосылками, правилами «никогда» и уже принятыми пакетами.
//
// Арбитр — внешнее кольцо, он видит только готовый план и живёт в пакете
// arbiter.
//
// Пакет — чистая математика: он не знает ни про модели, ни про журнал на
// диске, ни про octochan. Числа ему передают снаружи.
package rings

import (
	"math"
	"sort"
	"strings"
)

// Mode — режим, в котором работали кольца. Гарантии в режимах разные, и
// отчёт обязан это сказать (раздел 6.4).
type Mode string

const (
	// Embedded — встроенный режим: система управляет моделью, видит
	// распределение и придерживает токен. Отвергнутое утверждение не
	// появляется в ответе.
	Embedded Mode = "embedded"
	// Protocol — режим протокола: модель доступна только готовым ответом.
	// Кольца 0 нет, кольца 1–3 применяются к утверждениям готового ответа:
	// отвергнутое в ответе есть, но дальше не проходит.
	Protocol Mode = "protocol"
)

// Derive — поддержка вывода по правилу с надёжностью rule из независимых
// посылок (утверждение 7): [cL·∏Lᵢ, cU·∏Uᵢ]. Без посылок — сама
// надёжность правила.
func Derive(rule Interval, premises ...Interval) Interval {
	out := rule
	for _, p := range premises {
		out.L *= p.L
		out.U *= p.U
	}
	return out
}

// Independent — поддержка пакета, выводимого несколькими независимыми
// путями: 1 − ∏(1 − Pₛ), концы интервала подставляются. Пути 0,7 и 0,6
// дают 1 − 0,3·0,4 = 0,88.
func Independent(paths ...Interval) Interval {
	if len(paths) == 0 {
		return Interval{}
	}
	missL, missU := 1.0, 1.0
	for _, p := range paths {
		missL *= 1 - p.L
		missU *= 1 - p.U
	}
	return Interval{L: 1 - missL, U: 1 - missU}
}

// Shared — пути с общими посылками: без независимости объединение
// ограничено снизу максимумом, сверху суммой: [max Lₛ, min(1, ∑Uₛ)].
func Shared(paths ...Interval) Interval {
	if len(paths) == 0 {
		return Interval{}
	}
	out := Interval{}
	for _, p := range paths {
		out.L = math.Max(out.L, p.L)
		out.U += p.U
	}
	out.U = math.Min(1, out.U)
	return out
}

// Chain — поддержка плана: произведение поддержек шагов (утверждение 9).
// Двадцать шагов по 0,97 дают около 0,54 — уверенность теряется по цепочке,
// даже когда каждый шаг прошёл.
func Chain(steps ...Interval) Interval {
	out := Certain
	for _, s := range steps {
		out.L *= s.L
		out.U *= s.U
	}
	return out
}

// PlanSupport — поддержка плана целиком (вопрос 1 манифеста): произведение
// поддержек по различным правилам, а не по вызовам. Правило, применённое
// к пятидесяти хостам, либо верно для этой задачи, либо нет: его отказы
// между вызовами не независимы, и умножать его поддержку на себя
// пятьдесят раз — считать, что план держится в 0,97⁵⁰ раз слабее, чем
// на деле. Правило, которое оператор принял сам (Trusted), в произведение
// не входит: за него отвечает оператор.
func PlanSupport(packages []*Package) Interval {
	seen := map[string]bool{}
	out := Certain
	for _, p := range packages {
		key := strings.Join(p.Rules, "+")
		if seen[key] || p.Trusted {
			continue
		}
		seen[key] = true
		out.L *= p.Support.L
		out.U *= p.Support.U
	}
	return out
}

// Weakest — до n различных правил плана, от самого слабого по нижней
// границе поддержки. Порядок, а не числа: его можно показать оператору,
// не нарушая слепоты калибровки (раздел 7.3).
func Weakest(packages []*Package, n int) []string {
	type rule struct {
		name string
		l    float64
	}
	seen := map[string]bool{}
	var rules []rule
	for _, p := range packages {
		key := strings.Join(p.Rules, "+")
		if seen[key] || p.Trusted {
			continue
		}
		seen[key] = true
		rules = append(rules, rule{key, p.Support.L})
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].l < rules[j].l })
	var out []string
	for i := 0; i < len(rules) && i < n; i++ {
		out = append(out, rules[i].name)
	}
	return out
}

// Ring0 — кольцо 0: индексы токенов с вероятностью не ниже порога tau.
// Пустой ответ значит, что позицию генерировать заново бесполезно: при том
// же тексте модель даст то же распределение, и генерация откатывается к
// началу утверждения (раздел 6.2).
func Ring0(dist []float64, tau float64) []int {
	var out []int
	for i, p := range dist {
		if p >= tau {
			out = append(out, i)
		}
	}
	return out
}

// MaxPass — сколько токенов в одной позиции может пройти порог τ: ⌊1/τ⌋
// (утверждение 6). Поэтому высоким порог быть не может: при τ = 0,8 проходит
// один кандидат, при τ = 0,1 — десять.
func MaxPass(tau float64) int {
	if tau <= 0 {
		return math.MaxInt
	}
	return int(math.Floor(1/tau + 1e-12))
}

// Verdict — что кольца решили о пакете.
type Verdict string

const (
	// WellTrue — пакет прошёл кольцо 2 и кольцо 3: принят.
	WellTrue Verdict = "well_true"
	// WellFalse — нижняя граница поддержки ниже порога well_True. Для колец
	// «ложь» и «не принят» — одно и то же: пакет отбрасывается целиком.
	WellFalse Verdict = "well_false"
	// Contradiction — пакет прошёл кольцо 2, но противоречит точным
	// предпосылкам: кольцо 3.
	Contradiction Verdict = "contradiction"
)

// Package — пакет: законченное утверждение-кандидат на формальном языке.
type Package struct {
	// Statement — утверждение, как его видит оператор.
	Statement string
	// Rules — правила, по которым пакет выведен.
	Rules []string
	// Support — интервал поддержки из кольца 1. Сырой счёт хранится рядом
	// с бинарным решением кольца 2: по нему разбирают инциденты.
	Support Interval
	// Trusted — пакет принял оператор, хотя поддержки для порога мало.
	// Кольцо 2 такой пакет пропускает: для языка пользователь всегда прав
	// (П8), и без этого правило, однажды подведшее, не набрало бы новых
	// успехов никогда. Кольцо 3 — нет: жёсткие правила абсолютны.
	Trusted bool
	// Verdict и Reason заполняет Run.
	Verdict Verdict
	Reason  string
}

// Checker — кольцо 3: противоречие пакета с точными предпосылками,
// правилами «никогда» и уже принятыми пакетами. Пустая строка — нет
// противоречия. Догадки, даже уверенные, в булеву проверку не идут.
type Checker func(p *Package, accepted []*Package) string

// Accept — решение кольца 2: принят, если нижняя граница поддержки не ниже
// порога well_True. Решение по нижней границе делает «кандидата до
// подтверждения» само собой: мало проверенное правило даёт широкий
// интервал с низкой нижней границей и порог не проходит.
func Accept(support Interval, wellTrue float64) bool {
	return support.L >= wellTrue
}

// Run прогоняет пакеты через кольца 2 и 3 по порядку и делит их на два
// множества: well_True (принятые) и отвергнутые — well_False кольца 2 и
// противоречия кольца 3.
//
// Отвергнутый кольцом 2 пакет в кольцо 3 не идёт и в виде отрицания:
// порог не согласован с отрицанием ни при каком значении (утверждение 8),
// и кольцо 3 выводило бы то, чего кольцо 1 не поддерживает. Принятый пакет
// становится предпосылкой следующих с единицей, поэтому кольцо 3 сразу
// ловит противоречие внутри плана.
func Run(packages []*Package, wellTrue float64, ring3 Checker) (accepted, rejected []*Package) {
	for _, p := range packages {
		if !p.Trusted && !Accept(p.Support, wellTrue) {
			p.Verdict = WellFalse
			rejected = append(rejected, p)
			continue
		}
		if ring3 != nil {
			if reason := ring3(p, accepted); reason != "" {
				p.Verdict, p.Reason = Contradiction, reason
				rejected = append(rejected, p)
				continue
			}
		}
		p.Verdict = WellTrue
		accepted = append(accepted, p)
	}
	return accepted, rejected
}
