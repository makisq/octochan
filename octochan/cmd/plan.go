package cmd

import (
	"fmt"
	"path/filepath"

	"octochan/config"
	"octochan/lang"
	"octochan/scenario"

	"github.com/spf13/cobra"
)

var (
	planScriptMode bool
	planOpts       scriptOptions
)

var planCmd = &cobra.Command{
	Use:   "plan <сценарий> | plan <база> <изменения> | plan --script <файл.ochan | ->",
	Short: "Показать, что произойдёт, ничего не делая",
	Long: "С одним аргументом планирует сценарий: какие операции были бы\n" +
		"выполнены и над чем. С двумя — показывает, какие параметры изменит\n" +
		"patch.\n\n" +
		"Способ выполнения сценария выбирается по его полю service, флага\n" +
		"для этого нет. Изменяющие запросы в режиме плана запрещены на уровне\n" +
		"клиента, поэтому команда не может ничего создать даже по ошибке.\n\n" +
		"Файл .ochan (или --script) — сценарий на языке алиасов. Он проверяется\n" +
		"целиком и выполняется в режиме плана: write и save ничего не пишут,\n" +
		"а печатается, какие файлы и параметры они изменили бы. Чтение файлов\n" +
		"и живых источников при этом настоящее — без него планировать нечего.",
	Args: cobra.MinimumNArgs(0),
	Example: "ochan plan restart.yaml\n" +
		"ochan plan prod.conf changes.conf\n" +
		"ochan plan эталон.conf postgres://dba@db1:5432/postgres\n" +
		"ochan plan сценарий.ochan\n" +
		"ochan plan --script --sandbox --root ./configs - < сценарий.ochan",
	RunE: func(cmd *cobra.Command, args []string) error {
		if planScriptMode || (len(args) == 1 && filepath.Ext(args[0]) == lang.AliasExtension) {
			return planScriptCmd(cmd, args)
		}
		if planOpts.any() {
			return fmt.Errorf("песочница относится к сценариям на языке алиасов: укажите --script")
		}
		if len(args) == 0 {
			return fmt.Errorf("укажите сценарий, пару конфигов или --script")
		}
		// Два аргумента — это всегда сравнение конфигов: у сценария второго
		// источника не бывает, у него есть только переопределения key=value.
		if len(args) >= 2 && !isOverride(args[1]) {
			return planConfig(cmd, args)
		}
		return planScenario(cmd, args)
	},
}

// planScriptCmd планирует сценарий на языке алиасов.
func planScriptCmd(cmd *cobra.Command, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("сценарий на языке алиасов параметров не принимает: значения пишутся в его тексте")
	}
	target, err := argOrStdin(args, 0)
	if err != nil {
		return fmt.Errorf("укажите файл сценария или подайте его на stdin: %w", err)
	}

	box, err := planOpts.build()
	if err != nil {
		return err
	}
	src, err := planOpts.source(target)
	if err != nil {
		return err
	}

	registry, errs := aliases()
	reportAliasErrors(cmd, errs)

	// Ошибка разбора или проверки — не план «с ошибкой», а отказ: ничего
	// не выполнялось, и показывать нечего.
	file, err := prepareScript(registry, src, true)
	if err != nil {
		return err
	}

	effects, runErr := planScript(cmd, registry, file, scriptSetup{box: box}, cmd.OutOrStdout())

	plan := lang.BuildPlan(file.Name, effects)
	if runErr != nil {
		plan.Warn("план неполный: сценарий прервался с ошибкой, показано то, что успело произойти")
	}
	cmd.Print("\n" + plan.Render(60))
	return runErr
}

// isOverride отличает переопределение параметра key=value от имени файла.
func isOverride(arg string) bool {
	for i := 1; i < len(arg); i++ {
		if arg[i] == '=' {
			return true
		}
	}
	return false
}

func planConfig(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	opts := config.SourceOptions{}

	base, err := loadConfigArg(ctx, args[0], opts)
	if err != nil {
		return err
	}
	changes, err := loadConfigArg(ctx, args[1], opts)
	if err != nil {
		return err
	}

	diff := config.CompareConfigs(base.Data, changes.Data, base.Name, changes.Name)
	plan := config.PlanFromDiff(diff, base.Name)
	plan.Command = "patch"
	plan.Source = changes.Name

	plan.Warnings = append(plan.Warnings, base.Notes...)
	plan.Warnings = append(plan.Warnings, changes.Notes...)

	cmd.Print(plan.Render(60))
	return nil
}

func planScenario(cmd *cobra.Command, args []string) error {
	target, overrides, err := scenarioArgs(args)
	if err != nil {
		return err
	}

	src, err := readSource(target)
	if err != nil {
		return err
	}

	parsed, err := parseScenario(src)
	if err != nil {
		return err
	}
	parsed = parsed.WithOverrides(overrides)

	runner, err := scenario.Resolve(parsed.Service)
	if err != nil {
		return err
	}

	plan, err := runner.Plan(cmd.Context(), parsed)
	if err != nil {
		return err
	}

	plan.Source = fmt.Sprintf("%s (%s, исполнитель %s)", src.Name, parsed.Service, runner.Kind())
	cmd.Print(plan.Render(60))

	if plan.Destructive {
		cmd.Printf("\nДля выполнения: ochan apply %s\n", target)
	}
	return nil
}

func init() {
	planCmd.Flags().BoolVar(&planScriptMode, "script", false, "сценарий на языке алиасов (для файла .ochan подразумевается)")
	planOpts.register(planCmd)
}
