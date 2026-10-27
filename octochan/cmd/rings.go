package cmd

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"octochan/arbiter"
	"octochan/fp"
	"octochan/lang"
	"octochan/rbac"
	"octochan/rings"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Кольца в режиме протокола (разделы 6.2 и 6.4 манифеста ФПм).
//
// Модель доступна только готовым ответом — сценарием, написанным по
// формальному промпту. Кольца 1–3 проверяют утверждения его плана, арбитр —
// план целиком. Утверждение — то, что написала модель: вызов операции
// скилла или запись встроенной операцией из самого сценария. Что скилл
// делает внутри, — реализация из библиотеки, а не утверждение модели.
//
// План неделим: сценарий не выполняется по частям. Отвергнутое утверждение
// в ответе есть, но план с ним дальше не проходит — ни в заявку, ни к
// выполнению.

// ringsPurpose — зачем строится план: от этого зависит порог well_True.
type ringsPurpose string

const (
	purposePlan    ringsPurpose = "plan"
	purposeRequest ringsPurpose = "request"
)

// ringsConfig — настройки колец, fp.rings в config.yaml.
type ringsConfig struct {
	enabled    bool
	wellTrue   float64
	prior      lang.Prior
	showScores bool
	budget     int
}

func loadRingsConfig(purpose ringsPurpose) (ringsConfig, error) {
	c := ringsConfig{
		enabled:    viper.GetBool("fp.rings.enabled"),
		showScores: viper.GetBool("fp.rings.show_scores"),
		budget:     viper.GetInt("fp.rings.budget.plan"),
	}
	key := "fp.rings.well_true.plan"
	if purpose == purposeRequest {
		key = "fp.rings.well_true.apply"
	}
	threshold, err := fraction(viper.Get(key))
	if err != nil {
		return c, fmt.Errorf("%s: %w", key, err)
	}
	c.wellTrue = threshold
	if c.budget < 0 {
		return c, fmt.Errorf("fp.rings.budget.plan: %d, бюджет — число отказов подряд, 0 — без предела", c.budget)
	}
	if c.prior, err = lang.ParsePrior(viper.GetString("fp.rings.prior")); err != nil {
		return c, fmt.Errorf("fp.rings.prior: %w", err)
	}
	return c, nil
}

// fraction — число от 0 до 1 из конфига. Опечатка не должна стать нулём:
// порог 0 пропустил бы всё.
func fraction(v any) (float64, error) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int:
		f = float64(x)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, fmt.Errorf("%q — не число", x)
		}
		f = parsed
	default:
		return 0, fmt.Errorf("%v — не число", v)
	}
	if f < 0 || f > 1 {
		return 0, fmt.Errorf("%v, порог — число от 0 до 1", f)
	}
	return f, nil
}

// statement — утверждение модели и его часть трассы плана: по ней кольцо 3
// проверяет правила «никогда».
type statement struct {
	text    string
	rule    string
	effects []lang.Effect
	calls   []lang.Invocation
}

// statements делит трассу плана на утверждения модели: вызовы операций
// скиллов и записи встроенными операциями вне скиллов. Алиас самого
// сценария утверждением не считается — утверждения то, что внутри него.
// Чтение живых источников — посылки, а не утверждения.
func statements(effects []lang.Effect, calls []lang.Invocation) []*statement {
	byID := make(map[int]lang.Invocation, len(calls))
	for _, c := range calls {
		byID[c.ID] = c
	}
	var out []*statement
	owner := map[int]*statement{}
	for _, c := range calls {
		if c.Module == "" || slices.ContainsFunc(c.Outer, func(id int) bool { return byID[id].Module != "" }) {
			continue
		}
		st := &statement{text: fmt.Sprintf("%s(%s)", c.Qualified(), strings.Join(c.Args, ", ")), rule: c.Qualified()}
		owner[c.ID] = st
		out = append(out, st)
	}
	// Вызов скилла — первый скилл в цепочке вызовов, от внешнего: всё,
	// что случилось внутри, принадлежит ему.
	find := func(chain []int) *statement {
		for _, id := range chain {
			if st, ok := owner[id]; ok {
				return st
			}
		}
		return nil
	}
	for _, c := range calls {
		if st := find(append(append([]int(nil), c.Outer...), c.ID)); st != nil {
			st.calls = append(st.calls, c)
		}
	}
	for _, e := range effects {
		switch st := find(e.Calls); {
		case st != nil:
			st.effects = append(st.effects, e)
		case e.Kind != lang.EffectLive:
			out = append(out, &statement{text: fmt.Sprintf("%s %s", e.Kind, e.Path), rule: string(e.Kind),
				effects: []lang.Effect{e}})
		}
	}
	return out
}

// rulePrior — условный счёт правила: prior.<алиас> из манифеста пакета или
// fp.rings.prior.
func rulePrior(rule string, fallback lang.Prior, registry *lang.Registry) lang.Prior {
	if i := strings.LastIndex(rule, "."); i > 0 && registry != nil {
		if pkg, ok := registry.Package(rule[:i]); ok {
			if p, ok := pkg.Priors[rule[i+1:]]; ok {
				return p
			}
		}
	}
	return fallback
}

func textSum(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

func decisionStore() (*workspace.Decisions, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewDecisions(fm.DecisionsDir())
}

// judgement — итог колец и арбитра.
type judgement struct {
	// rules — жёсткие правила промпта, которые план соблюдает.
	rules []string
	// decision — запись журнала решений; пусто — кольца выключены.
	decision string
}

// judgePlan прогоняет план сценария через кольца и арбитра и пишет решение
// в журнал. Без промпта нечего проверять: кольца судят ответ модели на
// формальный промпт.
func judgePlan(cmd *cobra.Command, purpose ringsPurpose, in *scriptInput, prompt *fp.Result, trust []string,
	registry *lang.Registry, effects []lang.Effect, calls []lang.Invocation) (judgement, error) {
	if prompt == nil {
		return judgement{}, nil
	}
	cfg, err := loadRingsConfig(purpose)
	if err != nil {
		return judgement{}, err
	}
	if len(trust) > 0 && currentRole != nil && strings.EqualFold(currentRole.Name, rbac.LLM) {
		return judgement{}, refuse(cmd, errors.New("--trust — решение оператора: роль llm правилам не доверяет"))
	}
	if !cfg.enabled {
		rules, err := checkRules(cmd, prompt, effects, calls)
		return judgement{rules: rules}, err
	}

	journal, err := decisionStore()
	if err != nil {
		return judgement{}, err
	}
	all, err := journal.All()
	if err != nil {
		return judgement{}, err
	}
	d := &workspace.Decision{
		Mode:      string(rings.Protocol),
		Purpose:   string(purpose),
		Prompt:    in.prompt.Name,
		PromptSum: textSum(in.prompt.Content),
		Script:    in.script.Name,
		ScriptSum: textSum(in.script.Content),
		WellTrue:  cfg.wellTrue,
	}
	if currentRole != nil {
		d.Role = currentRole.Name
	}
	// С --trust план запускает оператор — тот, кого бюджет и зовёт.
	history := workspace.ForPrompt(all, d.PromptSum)
	if len(trust) == 0 {
		if err := checkBudget(cfg, history); err != nil {
			return judgement{}, err
		}
	}

	trusted := map[string]bool{}
	for _, rule := range trust {
		trusted[rule] = true
	}
	counts := workspace.RuleCounts(all)
	stmts := statements(effects, calls)
	packages := make([]*rings.Package, len(stmts))
	owner := make(map[*rings.Package]*statement, len(stmts))
	for i, st := range stmts {
		c := counts[st.rule]
		prior := rulePrior(st.rule, cfg.prior, registry)
		packages[i] = &rings.Package{
			Statement: st.text,
			Rules:     []string{st.rule},
			Support:   rings.Reliability(c.Successes, c.Uses, rings.Prior(prior)),
			Trusted:   trusted[st.rule],
		}
		owner[packages[i]] = st
	}

	// Кольцо 3: правила «никогда» — запреты промпта — по части трассы,
	// которая принадлежит утверждению. «Всегда» — о плане целиком, это
	// работа арбитра.
	var forbid []*fp.Rule
	for _, rule := range prompt.Program.Rules {
		if rule.Kind == fp.Forbid {
			forbid = append(forbid, rule)
		}
	}
	ring3 := func(p *rings.Package, _ []*rings.Package) string {
		st := owner[p]
		var reasons []string
		for _, o := range arbiter.Check(forbid, st.effects, st.calls) {
			if !o.OK() {
				reasons = append(reasons, arbiter.Describe(o.Rule)+": "+strings.Join(o.Violations, "; "))
			}
		}
		return strings.Join(reasons, "; ")
	}
	_, rejected := rings.Run(packages, cfg.wellTrue, ring3)
	printRings(cmd, cfg, packages)

	// Арбитр смотрит план целиком и тогда, когда кольца его уже отвергли:
	// модель получает все причины разом, а не по одной за попытку.
	passed, failed := runArbiter(cmd, prompt, effects, calls)
	d.Arbiter = workspace.ArbiterPassed
	if len(failed) > 0 {
		d.Arbiter, d.ArbiterReasons = workspace.ArbiterRejected, failed
	}
	for _, p := range packages {
		d.Packages = append(d.Packages, workspace.DecisionPackage{
			Statement: p.Statement, Rules: p.Rules, L: p.Support.L, U: p.Support.U,
			Verdict: string(p.Verdict), Reason: p.Reason,
			Trusted: p.Trusted && !rings.Accept(p.Support, cfg.wellTrue),
		})
	}
	if err := journal.Append(d); err != nil {
		cmd.PrintErrf("внимание: решение не попало в журнал решений: %v\n", err)
	}

	var problems []string
	if len(failed) > 0 {
		problems = append(problems, "план нарушает жёсткие правила промпта: "+strings.Join(failed, "; "))
	}
	if len(rejected) > 0 {
		problems = append(problems, fmt.Sprintf("кольца отвергли утверждений: %d из %d", len(rejected), len(packages)))
	}
	if len(problems) == 0 {
		return judgement{rules: passed, decision: d.ID}, nil
	}
	if n := len(history); n > 0 && history[n-1].Rejected() && history[n-1].Reasons() == d.Reasons() {
		problems = append(problems, "причина та же, что в прошлый раз: дальше нужен оператор")
	}
	return judgement{}, errors.New(strings.Join(problems, "; "))
}

// checkBudget не даёт модели перебирать варианты, пока какой-нибудь не
// проскочит кольца: после fp.rings.budget.plan отказов подряд по одному
// промпту и после второго отказа по той же причине нужен оператор. Он
// уточняет промпт — новый текст начинает новый счёт.
func checkBudget(cfg ringsConfig, history []*workspace.Decision) error {
	streak := 0
	for i := len(history) - 1; i >= 0 && history[i].Rejected(); i-- {
		streak++
	}
	const operator = "нужен оператор: уточните промпт — новый текст начинает новый счёт"
	if n := len(history); streak >= 2 && history[n-1].Reasons() == history[n-2].Reasons() {
		return errors.New("план по этому промпту дважды подряд отвергнут по одной причине; " + operator)
	}
	if cfg.budget > 0 && streak >= cfg.budget {
		return fmt.Errorf("план по этому промпту отвергнут %d раз подряд, бюджет fp.rings.budget.plan исчерпан; %s",
			streak, operator)
	}
	return nil
}

// printRings печатает решение колец. Оценки по умолчанию скрыты: оператор,
// который согласует заявку, не должен их видеть, иначе его решение не
// годится для калибровки (раздел 7.3).
func printRings(cmd *cobra.Command, cfg ringsConfig, packages []*rings.Package) {
	cmd.Printf("\nКольца (режим протокола, порог well_True %.2f):\n", cfg.wellTrue)
	if len(packages) == 0 {
		cmd.Println("  утверждений нет: план ничего не записывает")
		return
	}
	labels := map[rings.Verdict]string{
		rings.WellTrue: "well_True", rings.WellFalse: "well_False", rings.Contradiction: "противоречие",
	}
	const indent = "                "
	var supports []rings.Interval
	for _, p := range packages {
		score := ""
		if cfg.showScores {
			score = fmt.Sprintf("[%.2f; %.2f]  ", p.Support.L, p.Support.U)
		}
		note := ""
		if p.Trusted && !rings.Accept(p.Support, cfg.wellTrue) {
			note = " — доверие оператора"
		}
		cmd.Printf("  %-13s %s%s%s\n", labels[p.Verdict], score, p.Statement, note)
		switch p.Verdict {
		case rings.WellFalse:
			cmd.Printf("%sправило %s проверено мало для порога; принять утверждение без проверки может оператор: --trust %s\n",
				indent, p.Rules[0], p.Rules[0])
		case rings.Contradiction:
			cmd.Printf("%s%s\n", indent, p.Reason)
			cmd.Printf("%sнарушение запрета — на счёт модели и промпта: надёжность правила %s не меняется\n",
				indent, p.Rules[0])
		}
		supports = append(supports, p.Support)
	}
	if cfg.showScores {
		plan := rings.Chain(supports...)
		cmd.Printf("  поддержка плана [%.2f; %.2f]\n", plan.L, plan.U)
	}
}

// noteOperator переносит решение оператора по заявке в журнал решений: из
// него кольца считают надёжность правил. Сбой записи решения не отменяет.
func noteOperator(cmd *cobra.Command, request *workspace.Request, verdict string) {
	if request.Script == nil || request.Script.Prompt == nil || request.Script.Prompt.Decision == "" {
		return
	}
	journal, err := decisionStore()
	if err == nil {
		err = journal.SetOperator(request.Script.Prompt.Decision, verdict, request.ID)
	}
	if err != nil {
		cmd.PrintErrf("внимание: решение не попало в журнал решений: %v\n", err)
	}
}

var fpJournalCmd = &cobra.Command{
	Use:   "journal",
	Short: "Журнал решений: надёжность правил и калибровка",
	Long: "Кольца берут надёжность правил из журнала решений. Правило — операция\n" +
		"скилла или write и save; его надёжность — 95-процентный интервал\n" +
		"бета-распределения по планам, где оно использовалось: верный план прошёл\n" +
		"арбитра, и оператор его согласовал, не отклонил и не откатил. Без журнала\n" +
		"действует условный счёт: prior.<алиас> в манифесте пакета или\n" +
		"fp.rings.prior.\n\n" +
		"Нарушение запрета промпта — ошибка модели и промпта, а не скилла: оно\n" +
		"не снижает надёжность правил и считается отдельно, по ролям и промптам.\n\n" +
		"Калибровка сравнивает поддержку плана с долей верных планов: если при\n" +
		"поддержке 0,7–0,8 верных около 75 %, шкала честная.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadRingsConfig(purposePlan)
		if err != nil {
			return err
		}
		journal, err := decisionStore()
		if err != nil {
			return err
		}
		all, err := journal.All()
		if err != nil {
			return err
		}
		if len(all) == 0 {
			cmd.Println("Журнал решений пуст: планов по формальному промпту ещё не было")
			return nil
		}
		decided := workspace.Decided(all)
		correct := 0
		scores := make([]float64, len(decided))
		outcomes := make([]bool, len(decided))
		for i, d := range decided {
			outcomes[i], _ = d.Outcome()
			if outcomes[i] {
				correct++
			}
			plan := rings.Certain
			for _, p := range d.Packages {
				plan = rings.Chain(plan, rings.Interval{L: p.L, U: p.U})
			}
			scores[i] = plan.L
		}
		cmd.Printf("Журнал решений: планов %d, с исходом %d, верных %d\n", len(all), len(decided), correct)

		seen := map[string]bool{}
		for _, d := range all {
			for _, p := range d.Packages {
				for _, rule := range p.Rules {
					seen[rule] = true
				}
			}
		}
		registry, errs := aliases()
		reportAliasErrors(cmd, errs)
		counts := workspace.RuleCounts(all)
		cmd.Println("\nПравило                      верных/исходов  надёжность")
		for _, rule := range sortedPaths(seen) {
			c := counts[rule]
			r := rings.Reliability(c.Successes, c.Uses, rings.Prior(rulePrior(rule, cfg.prior, registry)))
			cmd.Printf("  %-26s %7d/%-7d [%.2f; %.2f]\n", rule, c.Successes, c.Uses, r.L, r.U)
		}

		// Нарушение запрета — ошибка модели и промпта, а не скилла: оно
		// считается здесь, а не в надёжности правил.
		byPrompt, byRole := workspace.Blames(all)
		cmd.Println("\nНарушения запретов — на счёт модели и промпта:")
		for _, name := range sortedPaths(byRole) {
			b := byRole[name]
			cmd.Printf("  роль %-21s планов %d, с нарушением %d\n", name, b.Plans, b.Violations)
		}
		for _, name := range sortedPaths(byPrompt) {
			if b := byPrompt[name]; b.Violations > 0 {
				cmd.Printf("  промпт %-19s планов %d, с нарушением %d\n", name, b.Plans, b.Violations)
			}
		}

		if len(decided) > 0 {
			cmd.Println("\nКалибровка — поддержка плана и доля верных:")
			for _, bin := range rings.Calibrate(scores, outcomes, 10) {
				if bin.Count > 0 {
					cmd.Printf("  %.1f–%.1f  планов %d, верных %.0f %%\n", bin.From, bin.To, bin.Count, 100*bin.Observed())
				}
			}
		}
		return nil
	},
}

func init() {
	fpCmd.AddCommand(fpJournalCmd)
}
