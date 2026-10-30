package cmd

import (
	"errors"
	"fmt"

	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

var compensateCmd = &cobra.Command{
	Use:   "compensate <id>",
	Short: "Оформить обратное действие выполненной заявки — второй согласованной операцией",
	Long: "rollback откатывает файлы точно: прежнее содержимое ochan записал сам.\n" +
		"Во внешней системе прежнего состояния у ochan нет, и обратное действие —\n" +
		"отдельная операция: исполнитель строит её план, ochan оформляет заявку,\n" +
		"её согласуют и выполняют как любую другую (ochan approve, ochan apply\n" +
		"--request). Обратное действие у заявки одно.\n\n" +
		"Если его нет — исполнитель не умеет обратного действия или задача уже\n" +
		"отработала так, что вернуть нечего, — это отказ с причиной, а не\n" +
		"видимость отката.",
	Args:    cobra.ExactArgs(1),
	Example: "ochan compensate 7a8f\nochan approve <id обратного действия>\nochan apply --request <id обратного действия>",
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := requestStore()
		if err != nil {
			return err
		}
		original, err := store.Get(args[0])
		if err != nil {
			return err
		}
		request, err := compensationRequest(cmd, original)
		if err != nil {
			return err
		}
		stampRequest(request)
		if err := store.Create(request); err != nil {
			return err
		}
		if err := store.LinkCompensation(original, request); err != nil {
			return err
		}
		warnDependents(cmd, original.ID)

		cmd.Printf("Заявка %s создана (%s): обратное действие заявки %s\n",
			request.ID, request.State.Russian(), original.ID)
		cmd.Print("\n" + request.Plan.Render(60) + "\n")
		cmd.Printf("Согласовать: ochan approve %s\n", request.ID)
		return nil
	},
}

// compensationRequest строит заявку на обратное действие выполненной.
func compensationRequest(cmd *cobra.Command, original *workspace.Request) (*workspace.Request, error) {
	switch {
	case original.Compensates != "":
		return nil, fmt.Errorf("заявка %s — сама обратное действие заявки %s; вернуть сделанное ею — новая заявка "+
			"на прямое действие", original.ID, original.Compensates)
	case original.CompensatedBy != "":
		return nil, fmt.Errorf("у заявки %s уже есть обратное действие — заявка %s", original.ID, original.CompensatedBy)
	case original.State == workspace.RequestFailed && len(original.AppliedRef) == 0:
		return nil, fmt.Errorf("заявка %s упала, ничего не сделав: возвращать нечего", original.ID)
	case original.State != workspace.RequestApplied && original.State != workspace.RequestFailed:
		return nil, fmt.Errorf("заявка %s %s: обратное действие нужно только выполненной или упавшей на полпути",
			original.ID, original.State.Russian())
	case original.Service == "":
		return nil, fmt.Errorf("заявка %s правила файлы: их откат точный — ochan rollback (запись покажет ochan history)",
			original.ID)
	}

	parsed, err := requestScenario(cmd.Context(), original)
	if err != nil {
		return nil, err
	}
	runner, err := scenario.Resolve(parsed.Service)
	if err != nil {
		return nil, err
	}
	compensator, ok := runner.(scenario.Compensator)
	if !ok {
		return nil, fmt.Errorf("исполнитель %s не умеет обратного действия: вернуть сделанное по заявке %s можно "+
			"только средствами самой системы — ochan этого не обещает", runner.Kind(), original.ID)
	}
	plan, err := compensator.PlanCompensation(cmd.Context(), parsed, original.Plan, original.AppliedRef)
	if err != nil {
		return nil, fmt.Errorf("обратного действия для заявки %s нет: %w", original.ID, err)
	}
	if plan.Len() == 0 {
		return nil, errors.New("исполнитель не нашёл, что возвращать: план обратного действия пуст")
	}
	return &workspace.Request{
		Source:      original.Source,
		Service:     original.Service,
		Summary:     fmt.Sprintf("обратное действие заявки %s через %s, операций: %d", original.ID, runner.Kind(), plan.Len()),
		Plan:        plan,
		Payload:     original.Payload,
		Overrides:   original.Overrides,
		Compensates: original.ID,
	}, nil
}

// applyCompensation выполняет согласованное обратное действие: план
// строится заново и сверяется с согласованным, как у прямого действия.
func applyCompensation(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request,
	parsed *scenario.Scenario, runner scenario.Runner) error {
	compensator, ok := runner.(scenario.Compensator)
	if !ok {
		return fmt.Errorf("исполнитель %s больше не умеет обратного действия: заявку %s выполнить нечем",
			runner.Kind(), request.ID)
	}
	original, err := store.Get(request.Compensates)
	if err != nil {
		return fmt.Errorf("заявка %s — обратное действие заявки %s: %w", request.ID, request.Compensates, err)
	}

	cmd.Printf("Заявка %s — обратное действие заявки %s, исполнитель %s\n", request.ID, original.ID, runner.Kind())
	now, err := compensator.PlanCompensation(cmd.Context(), parsed, original.Plan, original.AppliedRef)
	if err != nil {
		return fmt.Errorf("обратного действия для заявки %s больше нет: %w", original.ID, err)
	}
	if err := planDrift(request, now); err != nil {
		return err
	}
	if err := beginRun(cmd, store, request); err != nil {
		return err
	}
	result, applyErr := compensator.Compensate(cmd.Context(), parsed, request.Plan, original.AppliedRef)
	finishScenarioApply(cmd, store, request, parsed, result, applyErr,
		"apply --request "+request.ID+" (обратное действие "+original.ID+")")
	return applyErr
}

func init() {
	rootCmd.AddCommand(compensateCmd)
}
