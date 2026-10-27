package cmd

import (
	"errors"
	"sort"
	"strings"

	"octochan/i18n"
	"octochan/scenario"

	"github.com/spf13/cobra"
)

var statusExecutor string

// status — общая команда ядра. Ни про RLM, ни про Linux она не знает:
// состояние того, что создал исполнитель, показывает сам исполнитель
// (scenario.StatusReporter), а команда только выбирает его и печатает
// ответ. Исполнитель указывается флагом — --executor rlm или коротко
// --rlm; если показывать статус умеет только один, флаг не нужен.
var statusCmd = &cobra.Command{
	Use:   "status <ссылка>... [--executor <исполнитель> | --<исполнитель>]",
	Short: "Проверить статус задачи исполнителя",
	Long: "Показывает состояние того, что создал исполнитель сценариев: задачи RLM,\n" +
		"задания на машине — чего угодно, у чего есть статус. Ссылку печатает\n" +
		"apply: «Создано: …».\n\n" +
		"Исполнитель выбирается флагом: --executor rlm или коротко --rlm (флаг\n" +
		"есть у каждого исполнителя, который умеет показывать статус). Если такой\n" +
		"исполнитель один, флаг не нужен. Исполнители приходят модулями: статус\n" +
		"показывает тот, кто реализует scenario.StatusReporter.",
	Args: cobra.MinimumNArgs(1),
	Example: "ochan status 4f1c2a\n" +
		"ochan status 4f1c2a 9b7e01 --rlm\n" +
		"ochan status job-17 --executor linux",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, reporter, err := pickStatusReporter(cmd)
		if err != nil {
			return err
		}

		var failed []string
		for _, ref := range args {
			status, err := reporter.Status(cmd.Context(), ref)
			if status != nil {
				printStatus(cmd, name, status)
			}
			if err != nil && len(args) == 1 {
				return err
			}
			if err != nil {
				cmd.PrintErrln(i18n.Sprintf("ошибка: %s: %v", ref, err))
				failed = append(failed, ref)
			}
		}
		if len(failed) > 0 {
			return i18n.Errorf("статус не получен полностью: %s", strings.Join(failed, ", "))
		}
		return nil
	},
}

// statusReporters — исполнители, которые умеют показывать статус.
func statusReporters() map[string]scenario.StatusReporter {
	reporters := make(map[string]scenario.StatusReporter)
	for name, runner := range scenario.Executors() {
		if reporter, ok := runner.(scenario.StatusReporter); ok {
			reporters[name] = reporter
		}
	}
	return reporters
}

// pickStatusReporter выбирает исполнителя по флагам.
func pickStatusReporter(cmd *cobra.Command) (string, scenario.StatusReporter, error) {
	reporters := statusReporters()
	names := make([]string, 0, len(reporters))
	for name := range reporters {
		names = append(names, name)
	}
	sort.Strings(names)

	var chosen []string
	if statusExecutor != "" {
		chosen = append(chosen, strings.ToLower(statusExecutor))
	}
	for _, name := range names {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed && flag.Value.String() == "true" {
			chosen = append(chosen, name)
		}
	}
	chosen = dedupeStrings(chosen)

	switch {
	case len(reporters) == 0:
		return "", nil, errors.New(i18n.T("ни один подключённый исполнитель не умеет показывать статус"))
	case len(chosen) > 1:
		return "", nil, i18n.Errorf("исполнитель указан дважды: %s", strings.Join(chosen, ", "))
	case len(chosen) == 1:
		reporter, ok := reporters[chosen[0]]
		if !ok {
			return "", nil, i18n.Errorf("исполнитель %s не подключён или не умеет показывать статус; умеют: %s",
				chosen[0], strings.Join(names, ", "))
		}
		return chosen[0], reporter, nil
	case len(reporters) == 1:
		return names[0], reporters[names[0]], nil
	default:
		return "", nil, i18n.Errorf("статус умеют показывать несколько исполнителей (%s): укажите --executor или --%s",
			strings.Join(names, ", "), names[0])
	}
}

// executorFlagAnnotation помечает флаги исполнителей: они зависят от
// установленных модулей, и в справочник и шаблон перевода не входят.
const executorFlagAnnotation = "executor"

// addExecutorFlags даёт status флаг --<исполнитель> для каждого, кто умеет
// показывать статус. Вызывается, когда меняется набор исполнителей: при
// загрузке модулей и после !install.
func addExecutorFlags() {
	for name := range statusReporters() {
		if name == "" || statusCmd.Flags().Lookup(name) != nil {
			continue
		}
		statusCmd.Flags().Bool(name, false, i18n.Sprintf("статус от исполнителя %s", name))
		_ = statusCmd.Flags().SetAnnotation(name, executorFlagAnnotation, []string{name})
	}
}

func printStatus(cmd *cobra.Command, executor string, status *scenario.Status) {
	cmd.Printf("\n%s\n", i18n.Sprintf("Статус %s (%s)", status.Ref, executor))
	cmd.Println("----------------------------")
	if status.State != "" {
		cmd.Printf("%s: %s\n", i18n.T("Состояние"), status.State)
	}
	for _, field := range status.Fields {
		cmd.Printf("%s: %s\n", i18n.T(field.Name), field.Value)
	}
	if len(status.Events) == 0 {
		return
	}
	cmd.Println("\n" + i18n.T("Журнал выполнения:"))
	for _, event := range status.Events {
		cmd.Printf("[%s] %s - %s\n", event.Time, event.Level, event.Message)
	}
}

func dedupeStrings(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// servicesCmd показывает, какие сервисы и каким способом выполняются.
var servicesCmd = &cobra.Command{
	Use:   "services",
	Short: "Показать доступные сервисы сценариев",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		services := scenarioServices()
		if len(services) == 0 {
			cmd.Println("Зарегистрированных сервисов нет.")
			return nil
		}

		cmd.Println("Сервис                                   Исполнитель")
		for _, item := range services {
			cmd.Printf("%-40s %s\n", item.service, item.kind)
		}
		return nil
	},
}

func init() {
	statusCmd.Flags().StringVar(&statusExecutor, "executor", "", "исполнитель, у которого спросить статус: rlm, linux…")
}
