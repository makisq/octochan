package cmd

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"octochan/config"
	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var waitDone bool

var applyCmd = &cobra.Command{
	Use:   "apply <сценарий|-> [key=value ...] | apply --request <id>",
	Short: "Выполнить сценарий",
	Long: "Способ выполнения выбирается по полю service самого сценария:\n" +
		"каждый сервис зарегистрирован своим исполнителем. Флага для выбора\n" +
		"внешней системы нет — она такой же исполнитель, как и любой другой.\n\n" +
		"Чтобы применить конфиг к файлу, используйте patch.",
	Args: cobra.MinimumNArgs(0),
	Example: "ochan apply scenario.yaml role=standalone\n" +
		"cat scenario.yaml | ochan apply -",
	RunE: func(cmd *cobra.Command, args []string) error {
		if applyRequestID != "" {
			return applyApprovedRequest(cmd)
		}

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

		log.Printf("apply: сценарий %s, сервис %s, исполнитель %s",
			src.Name, parsed.Service, runner.Kind())
		cmd.Printf("Сервис %s, исполнитель %s\n", parsed.Service, runner.Kind())

		result, err := runner.Apply(cmd.Context(), parsed)

		if result != nil && len(result.Refs) > 0 {
			cmd.Println("Создано:")
			for _, ref := range result.Refs {
				cmd.Printf("  - %s\n", ref)
			}

			// След должен остаться даже при последующей ошибке: сделанное
			// во внешней системе уже сделано.
			record(cmd, &workspace.Entry{
				Kind:       workspace.EntryApply,
				Command:    "apply",
				Source:     src.Name,
				Target:     parsed.Service,
				Summary:    result.Summary,
				TaskIDs:    result.Refs,
				Reversible: false,
			})
			notify(cmd, fmt.Sprintf("Сценарий %s выполнен", src.Name),
				applyMailBody(src.Name, parsed, result), nil)
		}
		if err != nil {
			return err
		}

		if !waitDone && !viper.GetBool("defaults.auto_check_status") {
			return nil
		}
		return waitForRefs(cmd, runner, result)
	},
}

// applyMailBody описывает выполненный сценарий для письма.
//
// В письме перечисляются ссылки на созданное: получатель узнаёт не только
// что сценарий отработал, но и за чем именно идти смотреть.
func applyMailBody(name string, parsed *scenario.Scenario, result *scenario.Result) string {
	body := fmt.Sprintf("Сценарий: %s\nСервис: %s\nИтог: %s\n",
		name, parsed.Service, result.Summary)
	if len(parsed.Targets) > 0 {
		body += fmt.Sprintf("Целей: %d\n", len(parsed.Targets))
	}
	if len(result.Refs) > 0 {
		body += "\nСоздано:\n"
		for _, ref := range result.Refs {
			body += "  - " + ref + "\n"
		}
	}
	return body
}

// applyApprovedRequest выполняет согласованную заявку.
//
// Выполняется текст сценария, сохранённый в самой заявке, а не перечитанный
// с диска файл: между согласованием и выполнением его могли поправить,
// и тогда выполнилось бы не то, на что сказали «да».
func applyApprovedRequest(cmd *cobra.Command) error {
	store, err := requestStore()
	if err != nil {
		return err
	}

	request, err := store.Get(applyRequestID)
	if err != nil {
		return err
	}
	if request.State != workspace.RequestApproved {
		return fmt.Errorf("заявка %s не согласована (%s): выполнять нельзя",
			request.ID, request.State.Russian())
	}
	if request.Script != nil {
		return applyScriptRequest(cmd, store, request)
	}
	if request.Service == "" {
		return applyConfigRequest(cmd, store, request)
	}

	parsed, err := scenario.Parse(request.Source, []byte(request.Payload))
	if err != nil {
		return fmt.Errorf("сохранённый в заявке сценарий не разбирается: %w", err)
	}

	runner, err := scenario.Resolve(parsed.Service)
	if err != nil {
		return err
	}

	cmd.Printf("Заявка %s, сервис %s, исполнитель %s\n", request.ID, parsed.Service, runner.Kind())

	result, applyErr := runner.Apply(cmd.Context(), parsed)

	var refs []string
	if result != nil {
		refs = result.Refs
	}
	if len(refs) > 0 {
		cmd.Println("Создано:")
		for _, ref := range refs {
			cmd.Printf("  - %s\n", ref)
		}

		if err := store.MarkApplied(request, refs); err != nil {
			cmd.PrintErrf("внимание: не удалось отметить заявку выполненной: %v\n", err)
		}
		record(cmd, &workspace.Entry{
			Kind:       workspace.EntryApply,
			Command:    "apply --request " + request.ID,
			Source:     request.Source,
			Target:     parsed.Service,
			Summary:    result.Summary,
			TaskIDs:    refs,
			Reversible: false,
		})
		notify(cmd, fmt.Sprintf("Заявка %s выполнена", request.ID), requestMailBody(request), nil)
	}

	if applyErr != nil {
		return applyErr
	}
	if !waitDone && !viper.GetBool("defaults.auto_check_status") {
		return nil
	}
	return waitForRefs(cmd, runner, result)
}

// scenarioArgs разбирает аргументы команд apply и plan: источник сценария
// и переопределения параметров.
func scenarioArgs(args []string) (string, map[string]string, error) {
	target, err := argOrStdin(args, 0)
	if err != nil {
		return "", nil, fmt.Errorf("укажите файл сценария или подайте его на stdin: %w", err)
	}

	overrides := args
	if len(args) > 0 {
		overrides = args[1:]
	}

	params, err := parseCustomParams(overrides)
	if err != nil {
		return "", nil, err
	}
	return target, params, nil
}

// parseScenario разбирает сценарий и отдельно объясняет частую ошибку —
// поданный вместо сценария конфигурационный файл.
func parseScenario(src source) (*scenario.Scenario, error) {
	parsed, err := scenario.Parse(src.Name, []byte(src.Content))
	if err == nil {
		return parsed, nil
	}

	// Проверяем по содержимому, а не по виду ошибки: conf-файл не доходит
	// до ErrNotScenario, потому что YAML спотыкается раньше — на строке
	// вида «key = value», которую не может положить в структуру.
	if looksLikeConfig(src.Content) {
		return nil, fmt.Errorf("%s похож на конфигурационный файл, а не на сценарий "+
			"(в сценарии должно быть поле service);\n"+
			"чтобы применить конфиг к файлу, используйте: ochan patch <база> %s",
			src.Name, src.Name)
	}
	return nil, err
}

// looksLikeConfig сообщает, что перед нами конфигурация, а не сценарий.
func looksLikeConfig(content string) bool {
	cfg, err := config.ParseConfig(content)
	if err != nil {
		return false
	}

	total := 0
	for _, params := range cfg {
		total += len(params)
	}
	return total > 0
}

// waitForRefs дожидается завершения созданных задач.
//
// Ожидание умеет пока только исполнитель RLM: у него задачи имеют статус,
// который можно опросить. Для остальных ссылок просто говорим, что ждать
// нечего, а не делаем вид, что подождали.
func waitForRefs(cmd *cobra.Command, runner scenario.Runner, result *scenario.Result) error {
	if result == nil || len(result.Refs) == 0 {
		return nil
	}

	waiter, ok := runner.(scenario.Waiter)
	if !ok {
		cmd.Printf("Исполнитель %s не умеет ждать завершения — ждать нечего.\n", runner.Kind())
		return nil
	}

	cmd.Println("\nОжидание завершения...")
	for _, ref := range result.Refs {
		status, err := waiter.Wait(cmd.Context(), ref)
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		cmd.Printf("  %s: завершено (%s)\n", ref, status)
	}
	return nil
}

var saveScenarioCmd = &cobra.Command{
	Use:   "srs <scenario.yaml>",
	Short: "Сохранить сценарий во внутреннее хранилище",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}

		src, err := readSource(args[0])
		if err != nil {
			return err
		}

		// Проверяем сценарий до сохранения, чтобы в хранилище не оседал мусор.
		if _, err := parseScenario(src); err != nil {
			return fmt.Errorf("сценарий не прошёл проверку: %w", err)
		}

		name := strings.TrimSuffix(filepath.Base(args[0]), filepath.Ext(args[0]))
		path, err := fm.SaveScenario(name, []byte(src.Content))
		if err != nil {
			return err
		}

		cmd.Printf("Сценарий сохранён: %s\n", path)
		return nil
	},
}

func parseCustomParams(args []string) (map[string]string, error) {
	if len(args) == 0 {
		return nil, nil
	}

	params := make(map[string]string, len(args))
	for _, arg := range args {
		key, value, found := strings.Cut(arg, "=")
		if !found || key == "" {
			// Раньше некорректный параметр просто писался в лог и терялся.
			return nil, fmt.Errorf("неверный формат параметра %q, ожидается key=value", arg)
		}
		params[key] = value
	}
	return params, nil
}

// serviceInfo — строка таблицы для команды services.
type serviceInfo struct {
	service string
	kind    string
}

func scenarioServices() []serviceInfo {
	names := scenario.Services()
	infos := make([]serviceInfo, 0, len(names))

	for _, name := range names {
		kind := "неизвестен"
		if runner, err := scenario.Resolve(name); err == nil {
			kind = runner.Kind()
		}
		infos = append(infos, serviceInfo{service: name, kind: kind})
	}
	return infos
}

func init() {
	applyCmd.Flags().BoolVarP(&waitDone, "wait", "w", false, "дождаться завершения")
}

// applyConfigRequest выполняет согласованную правку конфига.
//
// Применяются изменения из плана заявки, а не перечитанный файл правок:
// согласовывали план. Перед этим файл сверяется с тем, что было при
// согласовании: если параметр, который правка меняет, успели поправить,
// выполнение отменяется — иначе правка легла бы не на то, что видели.
func applyConfigRequest(cmd *cobra.Command, store *workspace.Requests, request *workspace.Request) error {
	target := request.Target
	switch {
	case target == "" || target == "stdin":
		return fmt.Errorf("заявка %s: правка stdin — записывать некуда; выполните её командой patch", request.ID)
	case config.SourceScheme(target) != "":
		return fmt.Errorf("заявка %s правит живой источник %s: ochan его не записывает — "+
			"выполните правку на сервере (например, ALTER SYSTEM)", request.ID, target)
	}

	current, err := config.LoadConfigFrom(cmd.Context(), target, config.SourceOptions{})
	if err != nil {
		return fmt.Errorf("заявка %s: %w", request.ID, err)
	}

	var drift []string
	for _, change := range request.Plan.Changes {
		now, exists := config.ParamValue(current.Data, change.Name)
		switch change.Kind {
		case config.ChangeAdd:
			if exists {
				drift = append(drift, fmt.Sprintf("  %s: при согласовании не было, сейчас %s", change.Name, now))
			}
		default:
			if !exists {
				drift = append(drift, fmt.Sprintf("  %s: при согласовании %s, сейчас параметра нет", change.Name, change.From))
			} else if now != change.From {
				drift = append(drift, fmt.Sprintf("  %s: при согласовании %s, сейчас %s", change.Name, change.From, now))
			}
		}
	}
	if len(drift) > 0 {
		return fmt.Errorf("%s изменился после согласования заявки %s: правка легла бы не на то, что согласовали; "+
			"оформите новую заявку.\n%s", target, request.ID, strings.Join(drift, "\n"))
	}

	// ApplyPlan правит карту на месте, а прежнее содержимое нужно журналу.
	before := make(config.ConfigMap, len(current.Data))
	for section, params := range current.Data {
		before[section] = make(map[string]string, len(params))
		for key, value := range params {
			before[section][key] = value
		}
	}
	result := config.ApplyPlan(current.Data, request.Plan)
	if err := config.SaveConfig(result, target); err != nil {
		return err
	}
	if err := store.MarkApplied(request, []string{target}); err != nil {
		cmd.PrintErrf("внимание: не удалось отметить заявку выполненной: %v\n", err)
	}
	entry := &workspace.Entry{
		Kind:       workspace.EntryPatch,
		Command:    "apply --request " + request.ID,
		Source:     request.Source,
		Target:     target,
		Summary:    fmt.Sprintf("правка по заявке %s, параметров: %d", request.ID, request.Plan.Len()),
		Changes:    request.Plan.Changes,
		Before:     before,
		Reversible: true,
	}
	record(cmd, entry)
	notify(cmd, fmt.Sprintf("Заявка %s выполнена", request.ID), requestMailBody(request), nil)

	cmd.Printf("Заявка %s выполнена: %s, изменено параметров: %d\n", request.ID, target, request.Plan.Len())
	if entry.ID != "" {
		cmd.Printf("Откат: ochan rollback %s\n", entry.ID)
	}
	return nil
}
