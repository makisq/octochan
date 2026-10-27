package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"octochan/config"
	"octochan/i18n"
	"octochan/lang"
	"octochan/rbac"
	"octochan/report"
	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	requestDoc     string
	requestComment string
	approveYes     bool
	rejectReason   string
	applyRequestID string
	notifyMail     bool
	requestScript  bool
	requestOpts    scriptOptions
	requestPrompt  promptOptions
)

// requestStore открывает хранилище заявок.
func requestStore() (*workspace.Requests, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewRequests(fm.RequestsDir())
}

var requestCmd = &cobra.Command{
	Use:   "request <сценарий> | request <база> <изменения> | request --script <файл.ochan | ->",
	Short: "Создать заявку на изменение из плана",
	Long: "Заявка фиксирует план и ждёт решения. Выполнить её можно только\n" +
		"после согласования: ochan apply --request <id>.\n\n" +
		"Заявка отвечает не на вопрос «кому в принципе можно», а на вопрос\n" +
		"«на это конкретное изменение сказали да» — именно это нужно аудиту.\n\n" +
		"Файл .ochan (или --script) — сценарий на языке алиасов: он проверяется,\n" +
		"план строится выполнением в режиме плана, а в заявку попадают его текст,\n" +
		"каталог и песочница. При выполнении план строится заново и должен совпасть.",
	Args: cobra.MinimumNArgs(0),
	Example: "ochan request restart.yaml --doc заявка.docx\n" +
		"ochan request prod.conf changes.conf\n" +
		"ochan request сценарий.ochan\n" +
		"ochan request --script --sandbox --root ./configs - < сценарий.ochan",
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := requestStore()
		if err != nil {
			return err
		}

		request, err := buildRequest(cmd, args)
		if err != nil {
			return err
		}
		if currentRole != nil {
			request.Role = currentRole.Name
		}
		if err := store.Create(request); err != nil {
			return err
		}

		cmd.Printf("Заявка %s создана (%s)\n", request.ID, request.State.Russian())
		cmd.Printf("Изменений: %d\n", request.Plan.Len())
		if request.Script != nil {
			// Тот, кто оформил заявку на сценарий, видит план сразу: из
			// текста сценария его не прочитать.
			cmd.Print("\n" + request.Plan.Render(60) + "\n")
		}

		if requestDoc != "" {
			if err := writeRequestDoc(request, requestDoc); err != nil {
				return err
			}
			cmd.Printf("Документ: %s\n", requestDoc)
		}

		cmd.Printf("Согласовать: ochan approve %s\n", request.ID)
		return nil
	},
}

// buildRequest строит заявку из сценария или из пары конфигов.
func buildRequest(cmd *cobra.Command, args []string) (*workspace.Request, error) {
	if requestScript || (len(args) == 1 && filepath.Ext(args[0]) == lang.AliasExtension) {
		return scriptRequest(cmd, args, &requestOpts, &requestPrompt)
	}
	if requestOpts.any() {
		return nil, fmt.Errorf("песочница относится к сценариям на языке алиасов: укажите --script")
	}
	if requestPrompt.any() {
		return nil, fmt.Errorf("промпт и отчёт модели относятся к сценариям на языке алиасов: укажите --script")
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("укажите сценарий, пару конфигов или --script")
	}
	if len(args) >= 2 && !isOverride(args[1]) {
		return configRequest(cmd, args)
	}
	return scenarioRequest(cmd, args)
}

func scenarioRequest(cmd *cobra.Command, args []string) (*workspace.Request, error) {
	target, overrides, err := scenarioArgs(args)
	if err != nil {
		return nil, err
	}

	src, err := readSource(target)
	if err != nil {
		return nil, err
	}

	parsed, err := parseScenario(src)
	if err != nil {
		return nil, err
	}
	parsed = parsed.WithOverrides(overrides)

	runner, err := scenario.Resolve(parsed.Service)
	if err != nil {
		return nil, err
	}

	plan, err := runner.Plan(cmd.Context(), parsed)
	if err != nil {
		return nil, err
	}

	// Текст сценария сохраняется в заявке, чтобы выполнить ровно то,
	// что согласовали. Перечитывать файл с диска нельзя: между
	// согласованием и выполнением его могли поправить.
	// Параметры из командной строки — тоже часть того, что согласуют:
	// без них при выполнении сценарий был бы другим.
	return &workspace.Request{
		Source:    src.Name,
		Service:   parsed.Service,
		Summary:   fmt.Sprintf("%s через %s, операций: %d", parsed.Service, runner.Kind(), plan.Len()),
		Plan:      plan,
		Payload:   src.Content,
		Overrides: overrides,
	}, nil
}

func configRequest(cmd *cobra.Command, args []string) (*workspace.Request, error) {
	ctx := cmd.Context()
	opts := config.SourceOptions{}

	base, err := loadConfigArg(ctx, args[0], opts)
	if err != nil {
		return nil, err
	}
	changes, err := loadConfigArg(ctx, args[1], opts)
	if err != nil {
		return nil, err
	}

	diff := config.CompareConfigs(base.Data, changes.Data, base.Name, changes.Name)
	plan := config.PlanFromDiff(diff, base.Name)
	plan.Command = "patch"
	plan.Source = changes.Name
	plan.Warnings = append(plan.Warnings, base.Notes...)
	plan.Warnings = append(plan.Warnings, changes.Notes...)

	// Файл записывается в заявку полным путём: выполнять её могут из
	// другого каталога, и правка должна лечь туда, где её согласовали.
	target := base.Name
	if args[0] != "-" && config.SourceScheme(args[0]) == "" {
		if abs, err := filepath.Abs(args[0]); err == nil {
			target = abs
		}
	}

	return &workspace.Request{
		Source:  changes.Name,
		Target:  target,
		Summary: fmt.Sprintf("правка %s, параметров: %d", base.Name, plan.Len()),
		Plan:    plan,
	}, nil
}

func writeRequestDoc(request *workspace.Request, path string) error {
	doc := report.Document{
		Title:   "Заявка на изменение " + request.ID,
		Author:  request.Author,
		Created: request.Created,
		Plan:    request.Plan,
		Fields: []report.Field{
			{Name: "Идентификатор", Value: request.ID},
			{Name: "Состояние", Value: request.State.Russian()},
			{Name: "Автор", Value: request.Author},
			{Name: "Создана", Value: request.Created.Local().Format("2006-01-02 15:04:05")},
			{Name: "Источник", Value: request.Source},
			{Name: "Итог", Value: request.Summary},
		},
	}
	if request.Service != "" {
		doc.Fields = append(doc.Fields, report.Field{Name: "Сервис", Value: request.Service})
	}
	if request.Target != "" {
		doc.Fields = append(doc.Fields, report.Field{Name: "Цель", Value: request.Target})
	}
	if request.Script != nil {
		doc.Fields = append(doc.Fields,
			report.Field{Name: "Каталог", Value: request.Script.Dir},
			report.Field{Name: "Песочница", Value: sandboxSummary(request.Script)})
		for _, name := range sortedPaths(request.Script.Packages) {
			doc.Fields = append(doc.Fields,
				report.Field{Name: "Пакет " + name, Value: versionOrNone(request.Script.Packages[name])})
		}
		doc.Listing = request.Payload
		if p := request.Script.Prompt; p != nil {
			doc.Fields = append(doc.Fields, report.Field{Name: "Промпт", Value: p.Name})
			for _, rule := range p.Rules {
				doc.Fields = append(doc.Fields, report.Field{Name: "Жёсткое правило", Value: rule + " — соблюдено в плане"})
			}
			doc.Prompt = p.Text
			doc.Report = describeReport(storedInput(p))
		}
	}
	if request.DecidedBy != "" {
		doc.Fields = append(doc.Fields,
			report.Field{Name: "Решение", Value: request.State.Russian() + " — " + request.DecidedBy})
		if request.Decision != "" {
			doc.Comments = append(doc.Comments, request.Decision)
		}
	}

	data, err := report.DOCX(doc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("не удалось записать документ %s: %w", path, err)
	}
	return nil
}

var requestsCmd = &cobra.Command{
	Use:   "requests [id]",
	Short: "Показать заявки",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := requestStore()
		if err != nil {
			return err
		}

		if len(args) == 1 {
			request, err := store.Get(args[0])
			if err != nil {
				return err
			}
			return showRequest(cmd, request)
		}

		all, err := store.List()
		if err != nil {
			return err
		}
		if len(all) == 0 {
			cmd.Printf("Заявок нет. Хранилище: %s\n", store.Dir())
			return nil
		}

		for _, request := range all {
			cmd.Println(request.Describe())
		}
		return nil
	},
}

func printListing(cmd *cobra.Command, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		cmd.Printf("  | %s\n", line)
	}
}

func showRequest(cmd *cobra.Command, request *workspace.Request) error {
	cmd.Printf("Заявка:    %s\n", request.ID)
	cmd.Printf("Состояние: %s\n", request.State.Russian())
	cmd.Printf("Автор:     %s\n", authorLine(request))
	cmd.Printf("Создана:   %s\n", request.Created.Local().Format("2006-01-02 15:04:05"))
	cmd.Printf("Источник:  %s\n", request.Source)
	if request.Service != "" {
		cmd.Printf("Сервис:    %s\n", request.Service)
	}
	if request.Target != "" {
		cmd.Printf("Цель:      %s\n", request.Target)
	}
	for _, key := range sortedPaths(request.Overrides) {
		cmd.Printf("Параметр:  %s=%s\n", key, request.Overrides[key])
	}
	cmd.Printf("Итог:      %s\n", request.Summary)
	if request.Compensates != "" {
		cmd.Printf("Обратное действие заявки %s\n", request.Compensates)
	}
	if request.CompensatedBy != "" {
		cmd.Printf("Обратное действие: заявка %s\n", request.CompensatedBy)
	}

	if request.DecidedBy != "" {
		cmd.Printf("Решение:   %s, %s (%s)\n",
			request.State.Russian(), request.DecidedBy,
			request.DecidedAt.Local().Format("2006-01-02 15:04:05"))
		if request.Decision != "" {
			cmd.Printf("Коммент.:  %s\n", request.Decision)
		}
	}
	if !request.AppliedAt.IsZero() {
		cmd.Printf("Выполнена: %s\n", request.AppliedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if request.Script != nil {
		cmd.Printf("Каталог:   %s\n", request.Script.Dir)
		cmd.Printf("Песочница: %s\n", sandboxSummary(request.Script))
		if request.Script.Clock != "" {
			// now() и rand() при выполнении вернут то же, что в плане.
			cmd.Printf("Время:     %s, зерно rand %d\n", request.Script.Clock, request.Script.Seed)
		}
		for _, name := range sortedPaths(request.Script.Packages) {
			cmd.Printf("Пакет:     %s %s\n", name, versionOrNone(request.Script.Packages[name]))
		}
		for _, path := range sortedPaths(request.Script.Libraries) {
			cmd.Printf("Алиасы:    %s\n", path)
		}
		// Согласуют не только план, но и текст, который его породил.
		cmd.Print("\nСценарий:\n")
		printListing(cmd, request.Payload)

		// И задачу, по которой модель его написала, и то, что модель
		// решила сама.
		if p := request.Script.Prompt; p != nil {
			cmd.Printf("\nПромпт %s:\n", p.Name)
			printListing(cmd, p.Text)
			if len(p.Rules) > 0 {
				cmd.Print("\nЖёсткие правила промпта (план их соблюдает):\n")
				for _, rule := range p.Rules {
					cmd.Printf("  %s\n", rule)
				}
			}
			printReport(cmd, storedInput(p))
		}
	}

	if request.Plan != nil {
		cmd.Print("\n" + request.Plan.Render(60))
	}

	if requestDoc != "" {
		if err := writeRequestDoc(request, requestDoc); err != nil {
			return err
		}
		cmd.Printf("\nДокумент: %s\n", requestDoc)
	}
	return nil
}

var approveCmd = &cobra.Command{
	Use:   "approve <id>",
	Short: "Согласовать заявку",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return decideRequest(cmd, args[0], workspace.RequestApproved, requestComment)
	},
}

var rejectCmd = &cobra.Command{
	Use:   "reject <id>",
	Short: "Отклонить заявку",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if rejectReason == "" {
			return fmt.Errorf("укажите причину отказа: --reason")
		}
		return decideRequest(cmd, args[0], workspace.RequestRejected, rejectReason)
	},
}

func decideRequest(cmd *cobra.Command, id string, state workspace.RequestState, comment string) error {
	store, err := requestStore()
	if err != nil {
		return err
	}

	request, err := store.Get(id)
	if err != nil {
		return err
	}
	if err := checkRequestScope(currentRole, request); err != nil {
		return refuse(cmd, err)
	}
	who := ""
	if currentRole != nil {
		who = fmt.Sprintf("%s, роль %s", workspace.CurrentUser(), currentRole.Name)
	}
	if state == workspace.RequestApproved && request.State == workspace.RequestPending {
		if err := checkApprover(cmd, request); err != nil {
			return refuse(cmd, err)
		}
	}
	if err := store.Decide(request, state, who, comment); err != nil {
		return err
	}
	if state == workspace.RequestApproved {
		noteOperator(cmd, request, workspace.OperatorApproved)
	} else {
		noteOperator(cmd, request, workspace.OperatorRejected)
	}

	cmd.Printf("Заявка %s: %s (%s)\n", request.ID, request.State.Russian(), request.DecidedBy)
	if state == workspace.RequestApproved {
		cmd.Printf("Выполнить: ochan apply --request %s\n", request.ID)
	}

	notify(cmd, fmt.Sprintf("Заявка %s %s", request.ID, request.State.Russian()),
		requestMailBody(request), nil)
	return nil
}

// checkApprover — разделение обязанностей при согласовании.
//
// Роль не согласует свою заявку: иначе право request и approve у одной
// роли превращало бы согласование в формальность. Исключение — master:
// на своей машине человек один. Заявку нейросети согласуют только после
// явного подтверждения: подсунутая модели инструкция рассчитывает как раз
// на то, что человек согласует не глядя.
func checkApprover(cmd *cobra.Command, request *workspace.Request) error {
	role := currentRole
	if role == nil {
		return nil
	}
	if request.Role != "" && strings.EqualFold(request.Role, role.Name) && role.Name != rbac.Master {
		return i18n.Errorf("заявку роли %s согласует другая роль: свою заявку не согласуют", request.Role)
	}
	if !strings.EqualFold(request.Role, rbac.LLM) || approveYes {
		return nil
	}

	cmd.Println(i18n.T("Заявку создала нейросеть (роль llm). Согласуйте её, только проверив план:"))
	if request.Plan != nil {
		cmd.Print("\n" + request.Plan.Render(60) + "\n")
	}
	if request.Script != nil {
		cmd.Printf("%s %s\n\n", i18n.T("Песочница:"), sandboxSummary(request.Script))
	}
	if !roleInputIsTTY() {
		return i18n.Errorf("заявку нейросети согласуют, проверив план выше: повторите с --yes")
	}
	answer, err := newCredentialReader(cmd).line(i18n.Sprintf("Чтобы согласовать, введите номер заявки (%s): ", request.ID))
	if err != nil {
		return err
	}
	if strings.TrimSpace(answer) != request.ID {
		return i18n.Errorf("номер не совпал: заявка %s не согласована", request.ID)
	}
	return nil
}

// authorLine — автор заявки и роль, в которой он её оформил.
func authorLine(request *workspace.Request) string {
	if request.Role == "" {
		return request.Author
	}
	return fmt.Sprintf("%s, роль %s", request.Author, request.Role)
}

func requestMailBody(request *workspace.Request) string {
	body := fmt.Sprintf("Заявка: %s\nСостояние: %s\nАвтор: %s\nИсточник: %s\nИтог: %s\n",
		request.ID, request.State.Russian(), request.Author, request.Source, request.Summary)
	if request.DecidedBy != "" {
		body += fmt.Sprintf("Решение принял: %s\n", request.DecidedBy)
	}
	if request.Decision != "" {
		body += fmt.Sprintf("Комментарий: %s\n", request.Decision)
	}
	if request.Plan != nil {
		body += "\n" + request.Plan.Render(60)
	}
	return body
}

func versionOrNone(version string) string {
	if version == "" {
		return "без версии"
	}
	return version
}

// sandboxSummary описывает песочницу заявки одной строкой.
func sandboxSummary(run *workspace.ScriptRun) string {
	if !run.Sandboxed {
		return "нет: сценарию доступно всё, что доступно выполняющему"
	}
	parts := []string{"каталоги " + strings.Join(run.Roots, ", ")}
	if len(run.Env) > 0 {
		parts = append(parts, "окружение "+strings.Join(run.Env, ", "))
	}
	if len(run.Live) > 0 {
		parts = append(parts, "живые источники "+strings.Join(run.Live, ", "))
	}
	if len(run.Net) > 0 {
		parts = append(parts, "сеть "+strings.Join(run.Net, ", "))
	}
	return strings.Join(parts, "; ")
}

// notify отправляет письмо, если почта настроена.
//
// Ненастроенная почта — не ошибка: письмо просто не нужно. А вот сбой
// отправки при настроенной почте показывается, но работу не прерывает:
// изменение уже сделано, и откатывать его из-за письма неправильно.
func notify(cmd *cobra.Command, subject, body string, attachments []report.Attachment) {
	if !notifyMail && !viper.GetBool("defaults.notify_always") {
		return
	}

	cfg := report.MailConfig{
		Host:     viper.GetString("defaults.smtp_host"),
		Port:     viper.GetInt("defaults.smtp_port"),
		From:     viper.GetString("defaults.smtp_from"),
		To:       viper.GetStringSlice("defaults.smtp_to"),
		Username: viper.GetString("defaults.smtp_user"),
		Password: os.Getenv("OCTOCHAN_SMTP_PASSWORD"),
		StartTLS: viper.GetBool("defaults.smtp_starttls"),
	}

	err := report.Send(cfg, report.Mail{Subject: subject, Body: body, Attachments: attachments})
	switch {
	case err == nil:
		cmd.Printf("Уведомление отправлено: %s\n", strings.Join(cfg.To, ", "))
	case isNotConfigured(err):
		cmd.PrintErrf("внимание: уведомление не отправлено: %v\n", err)
	default:
		cmd.PrintErrf("внимание: не удалось отправить уведомление: %v\n", err)
	}
}

// isNotConfigured отличает «почта не настроена» от сбоя отправки.
func isNotConfigured(err error) bool { return errors.Is(err, report.ErrMailNotConfigured) }

func init() {
	requestCmd.Flags().StringVar(&requestDoc, "doc", "", "выгрузить заявку в файл .docx")
	requestsCmd.Flags().StringVar(&requestDoc, "doc", "", "выгрузить заявку в файл .docx")
	approveCmd.Flags().StringVar(&requestComment, "comment", "", "комментарий к решению")
	approveCmd.Flags().BoolVar(&approveYes, "yes", false, "план заявки нейросети проверен: согласовать без вопроса")
	rejectCmd.Flags().StringVar(&rejectReason, "reason", "", "причина отказа")
	applyCmd.Flags().StringVar(&applyRequestID, "request", "", "выполнить согласованную заявку")
	requestCmd.Flags().BoolVar(&requestScript, "script", false, "сценарий на языке алиасов (для файла .ochan подразумевается)")
	requestOpts.register(requestCmd)
	requestPrompt.register(requestCmd)
	applyCmd.Flags().BoolVar(&notifyMail, "notify", false, "отправить уведомление по почте")
	approveCmd.Flags().BoolVar(&notifyMail, "notify", false, "отправить уведомление по почте")
	rejectCmd.Flags().BoolVar(&notifyMail, "notify", false, "отправить уведомление по почте")
}
