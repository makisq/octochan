package cmd

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"octochan/embedded"
	"octochan/fp"
	"octochan/rings"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

// Архитектурная модель (раздел 6.4 и уровень 2 манифеста): логика и кольца
// в её коде, ответ она пишет целиком сама. ochan моделью не управляет — он
// проверяет ответ снаружи, как арбитр: ответ в языке, начинается копией
// задачи, каждое решение выводимо из посылок, пропущено только то, что её
// кольцо 2 не приняло. Решение уходит в журнал: исход оператор отмечает
// ochan fp journal amend, и из журнала модель пересчитывает свои параметры
// (llama/fit.py).

// historyRule — правило модели: сужение неточной предпосылки по истории из
// журнала (раздел 5.2).
const historyRule = "fp.history"

// archTask — задача, на которую отвечает архитектурная модель.
type archTask struct {
	name, text string
	plan       *fp.AnswerPlan
	// premises — точные посылки задачи «Цель.атрибут = значение»: история
	// прочтений атрибутов для модели.
	premises []string
}

func premisesOf(prog *fp.Program) []string {
	var out []string
	for _, cell := range prog.Cells {
		for _, set := range cell.Sets {
			if !set.Value.Inexact() && !set.Value.Ref {
				out = append(out, set.Target+" = "+set.Value.Text)
			}
		}
	}
	return out
}

func generateArchitectural(cmd *cobra.Command, gen embedded.Generator, info embedded.Info, prompt string,
	task archTask, opts fp.Options) error {
	ans, err := gen.Generate(cmd.Context(), prompt)
	if err != nil {
		return err
	}
	if ans.Refused {
		return fmt.Errorf("модель отказала: не хватает предпосылок — %s; вопрос к оператору", strings.Join(ans.Missing, "; "))
	}

	var problems []string
	if bad := fp.CheckModelText("model", ans.Text, opts).Syntax(); len(bad) > 0 {
		problems = append(problems, "ответ не в языке: "+bad[0].String())
	}
	var omitted, narrowings []string
	for _, p := range ans.Packages {
		switch {
		case p.Verdict != string(rings.WellTrue):
			omitted = append(omitted, p.Statement)
		case slices.Contains(p.Rules, historyRule):
			narrowings = append(narrowings, p.Statement)
		}
	}
	narrowed, err := task.plan.Narrowings(narrowings)
	if err == nil {
		err = task.plan.Verify(ans.Text, omitted, narrowed)
	}
	if err != nil {
		problems = append(problems, "ответ расходится с логикой ochan: "+err.Error())
	}

	id := recordArchitectural(cmd, info, task, ans, problems)
	out := &embedded.Output{Text: ans.Text, Forced: ans.Forced, Chosen: ans.Chosen}
	for _, line := range strings.Split(strings.TrimPrefix(ans.Text, task.plan.Copy), "\n") {
		if strings.TrimSpace(line) != "" {
			out.Statements = append(out.Statements, embedded.Statement{Text: line})
		}
	}
	var genErr error
	if len(problems) > 0 {
		genErr = errors.New(strings.Join(problems, "; "))
	}
	recordGeneration(cmd, info, 0, out, genErr)

	cmd.PrintErrln("Архитектурная модель: ответ написала она сама — логика и кольца в её коде; ochan проверил его снаружи")
	// Токенов модель не сообщает: ответ она пишет сама, ochan её не ведёт.
	cmd.PrintErrf("Утверждений: %d, токенов: 0, обращений к модели: 1\n", len(out.Statements))
	cmd.PrintErrf("Логика:      утверждений %d — единственные допустимые; выбрано моделью среди допустимых: %d\n",
		ans.Forced, ans.Chosen)
	if len(omitted) > 0 {
		cmd.PrintErrf("Кольцо 2:    не принято %d — вопросы оператору\n", len(omitted))
	}
	for _, q := range ans.Questions {
		what := q.Statement
		if what == "" {
			what = fmt.Sprintf("%s у %s — %s", q.Attr, strings.Join(q.Targets, ", "), strings.Join(q.Values, " или "))
		}
		cmd.PrintErrf("Вопрос оператору: %s; %s\n", what, q.Why)
	}
	if id != "" {
		cmd.PrintErrf("Журнал решений: запись %s; исход — ochan fp journal amend %s --right | --wrong --reason …\n", id, id)
	}
	if genErr != nil {
		return fmt.Errorf("ответа нет: %v", genErr)
	}
	if llmOut != "" {
		if err := os.WriteFile(llmOut, []byte(ans.Text), 0o600); err != nil {
			return err
		}
		cmd.PrintErrf("Ответ: %s\n", llmOut)
		return nil
	}
	cmd.Print(ans.Text)
	return nil
}

// recordArchitectural пишет ответ архитектурной модели в журнал решений:
// её пакеты с правилами и поддержкой, вердикт внешней проверки. Сбой
// записи ответа не отменяет.
func recordArchitectural(cmd *cobra.Command, info embedded.Info, task archTask, ans embedded.Answer, problems []string) string {
	journal, err := decisionStore()
	if err != nil {
		cmd.PrintErrf("внимание: ответ не попал в журнал решений: %v\n", err)
		return ""
	}
	d := &workspace.Decision{
		Mode:      string(rings.Architectural),
		Purpose:   "answer",
		Prompt:    task.name,
		PromptSum: textSum(task.text),
		Script:    info.Model,
		ScriptSum: textSum(ans.Text),
		Arbiter:   workspace.ArbiterPassed,
		Premises:  task.premises,
	}
	if currentRole != nil {
		d.Role = currentRole.Name
	}
	if len(problems) > 0 {
		d.Arbiter, d.ArbiterReasons = workspace.ArbiterRejected, problems
	}
	var accepted []*rings.Package
	for _, p := range ans.Packages {
		d.Packages = append(d.Packages, workspace.DecisionPackage{
			Statement: p.Statement, Rules: p.Rules, L: p.L, U: p.U, Verdict: p.Verdict,
			Chosen: p.Chosen, Score: p.Score, LogProb: p.LogProb, Options: p.Options, Gap: p.Gap,
		})
		if p.Verdict == string(rings.WellTrue) {
			accepted = append(accepted, &rings.Package{Rules: p.Rules, Support: rings.Interval{L: p.L, U: p.U}})
		}
	}
	plan := rings.PlanSupport(accepted)
	d.PlanL, d.PlanU = plan.L, plan.U
	if err := journal.Append(d); err != nil {
		cmd.PrintErrf("внимание: ответ не попал в журнал решений: %v\n", err)
		return ""
	}
	return d.ID
}
