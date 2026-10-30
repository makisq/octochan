package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	requestDoc      string
	requestComment  string
	approveYes      bool
	rejectReason    string
	rejectKind      string
	applyRequestID  string
	notifyMail      bool
	requestScript   bool
	requestAfter    []string
	requestsReady   bool
	requestIncident string
	requestsAfter   string
	requestOpts     scriptOptions
	requestPrompt   promptOptions
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
		"каталог и песочница. При выполнении план строится заново и должен совпасть.\n\n" +
		"--after <id> — порядок: заявка выполнится только после указанных. Если\n" +
		"она упадёт, те, после которых она шла, остаются выполненными — сбой их\n" +
		"не откатывает; упавшую не повторяют, а оформляют заново, и заявки после\n" +
		"неё — тоже. Повтор и расписание — дело модуля рабочего процесса.",
	Args: cobra.MinimumNArgs(0),
	Example: "ochan request restart.yaml --doc заявка.docx\n" +
		"ochan request prod.conf changes.conf\n" +
		"ochan request deploy.yaml --after 1c4a\n" +
		"ochan request сценарий.ochan\n" +
		"ochan request --script --sandbox --root ./configs - < сценарий.ochan",
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := requestStore()
		if err != nil {
			return err
		}

		after, err := resolveAfter(store, requestAfter)
		if err != nil {
			return err
		}
		request, err := buildRequest(cmd, args)
		if err != nil {
			return err
		}
		request.After = after
		if request.Incident, err = linkIncident(requestIncident); err != nil {
			return err
		}
		stampRequest(request)
		if err := store.Create(request); err != nil {
			return err
		}
		noteRequestStep(request, "request", request.Summary)

		cmd.Printf("Заявка %s создана (%s)\n", request.ID, request.State.Russian())
		if len(after) > 0 {
			cmd.Printf("Выполнится после: %s\n", strings.Join(after, ", "))
		}
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

		if freeEligible(request) {
			freeRequest(cmd, store, request)
			return nil
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
	if err := resolveTargets(cmd.Context(), parsed, requestOpts.limit); err != nil {
		return nil, err
	}

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
		Limit:     requestOpts.limit,
	}, nil
}

func configRequest(cmd *cobra.Command, args []string) (*workspace.Request, error) {
	if len(requestOpts.limit) > 0 {
		return nil, errLimitConfig
	}
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
	Long: "Заявки от новых к старым. Заявку с --after, которую держат зависимости,\n" +
		"список показывает «ожидает (зависит от …)», а ту, что не выполнится\n" +
		"никогда — зависимость упала или отклонена, — «не выполнится (…)».\n\n" +
		"--ready — согласованные заявки, которые можно выполнять сейчас, в\n" +
		"порядке выполнения: по нему модуль рабочего процесса берёт следующую.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := requestStore()
		if err != nil {
			return err
		}
		all, err := store.List()
		if err != nil {
			return err
		}
		graph := workspace.NewGraph(all)

		if len(args) == 1 {
			request, err := store.Get(args[0])
			if err != nil {
				return err
			}
			return showRequest(cmd, request, graph)
		}

		if requestsReady {
			for _, request := range graph.Ready() {
				cmd.Println(graph.DescribeIn(request))
			}
			return nil
		}
		if requestsAfter != "" {
			return listAfter(cmd, store, all, graph, requestsAfter)
		}
		if len(all) == 0 {
			cmd.Printf("Заявок нет. Хранилище: %s\n", store.Dir())
			return nil
		}

		for _, request := range all {
			cmd.Println(graph.DescribeIn(request))
		}
		return nil
	},
}

func printListing(cmd *cobra.Command, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		cmd.Printf("  | %s\n", line)
	}
}

func showRequest(cmd *cobra.Command, request *workspace.Request, graph workspace.Graph) error {
	cmd.Printf("Заявка:    %s\n", request.ID)
	cmd.Printf("Состояние: %s\n", graph.Status(request))
	for _, id := range request.After {
		state := "нет такой заявки"
		if dep, ok := graph[id]; ok {
			state = graph.Status(dep)
		}
		cmd.Printf("После:     %s (%s)\n", id, state)
	}
	for _, next := range graph.Dependents(request.ID) {
		cmd.Printf("Перед:     %s (%s)\n", next.ID, graph.Status(next))
	}
	if request.Failure != "" {
		cmd.Printf("Сбой:      %s\n", request.Failure)
	}
	if weak := weakPlan(request); len(weak) > 0 {
		cmd.Printf("Внимание:  поддержка плана ниже порога; самые слабые правила: %s\n", strings.Join(weak, ", "))
	}
	if request.Runner != nil && request.State == workspace.RequestRunning {
		cmd.Printf("Выполняет: %s\n", request.Runner)
	}
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
	if len(request.Limit) > 0 {
		cmd.Printf("Цели:      --limit %s\n", strings.Join(request.Limit, " | "))
	}
	if request.Script != nil && len(request.Script.Limit) > 0 {
		cmd.Printf("Инвентарь: --limit %s\n", strings.Join(request.Script.Limit, " | "))
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
		if !slices.Contains(workspace.ReasonKinds, rejectKind) {
			return fmt.Errorf("--reason-kind %q: %s", rejectKind, strings.Join(workspace.ReasonKinds, ", "))
		}
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
		// Заявку, которая не выполнится никогда, не согласуют: её
		// зависимость упала или отклонена.
		if all, err := store.List(); err == nil {
			if broken := workspace.NewGraph(all).Readiness(request).Broken; len(broken) > 0 {
				return fmt.Errorf("заявка %s не выполнится: %s — согласовывать нечего; отклоните её и оформите заново",
					request.ID, joinBlockers(broken))
			}
		}
	}
	if err := store.Decide(request, state, who, comment); err != nil {
		return err
	}
	if state == workspace.RequestApproved {
		noteOperator(cmd, request, workspace.OperatorApproved)
	} else {
		noteOperatorKind(cmd, request, workspace.OperatorRejected, rejectKind)
		if request.Script != nil && request.Script.Prompt != nil && request.Script.Prompt.Decision != "" {
			if rejectKind == workspace.KindPlan {
				cmd.Println("Отказ засчитан правилам плана как их ошибка. Если причина не в плане — " +
					"--reason-kind timing, scope или other.")
			} else {
				cmd.Printf("Отказ не по плану (%s): правилам плана он не засчитан.\n", rejectKind)
			}
		}
	}

	cmd.Printf("Заявка %s: %s (%s)\n", request.ID, request.State.Russian(), request.DecidedBy)
	if state == workspace.RequestApproved {
		cmd.Printf("Выполнить: ochan apply --request %s\n", request.ID)
		if len(request.After) > 0 {
			cmd.Printf("Не раньше, чем выполнены: %s\n", strings.Join(request.After, ", "))
		}
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
	weak := weakPlan(request)
	if (!strings.EqualFold(request.Role, rbac.LLM) && len(weak) == 0) || approveYes {
		return nil
	}

	if strings.EqualFold(request.Role, rbac.LLM) {
		cmd.Println(i18n.T("Заявку создала нейросеть (роль llm). Согласуйте её, только проверив план:"))
	}
	if len(weak) > 0 {
		cmd.Println(i18n.Sprintf("Поддержка плана ниже порога: каждый шаг прошёл кольца, но вместе план держится "+
			"слабее. Самые слабые правила, по порядку: %s. Согласуйте, только проверив план:", strings.Join(weak, ", ")))
	}
	if request.Plan != nil {
		cmd.Print("\n" + request.Plan.Render(60) + "\n")
	}
	if request.Script != nil {
		cmd.Printf("%s %s\n\n", i18n.T("Песочница:"), sandboxSummary(request.Script))
	}
	if !roleInputIsTTY() {
		return i18n.Errorf("такую заявку согласуют, проверив план выше: повторите с --yes")
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

// listAfter — что было после: заявки в окне триггера или заявки, которые
// ждут названную (--after при оформлении).
func listAfter(cmd *cobra.Command, store *workspace.Requests, all []*workspace.Request, graph workspace.Graph, id string) error {
	var found []*workspace.Request
	if strings.Contains(id, "#") {
		found = requestsUnder(all, id)
		triggers, err := triggerStore()
		if err == nil {
			settleTriggers(triggers)
			windows, _ := triggers.Windows()
			for _, w := range windows {
				if w.ID() == id {
					state := w.Outcome
					if w.Open(time.Now()) {
						state = "действует до " + w.Until.Local().Format("15:04")
					}
					cmd.Printf("Окно %s: роль %s, с %s, %s\n", id, w.Role, w.Start.Local().Format("2006-01-02 15:04"), state)
				}
			}
		}
		if len(found) == 0 {
			cmd.Printf("В окне %s заявок не оформляли\n", id)
			return nil
		}
	} else {
		request, err := store.Get(id)
		if err != nil {
			return err
		}
		found = graph.Dependents(request.ID)
		if len(found) == 0 {
			cmd.Printf("После заявки %s ничего не ждёт\n", request.ID)
			return nil
		}
	}
	for _, request := range found {
		cmd.Println(graph.DescribeIn(request))
	}
	return nil
}

// weakPlan — самые слабые правила плана, если его поддержка ниже порога.
func weakPlan(request *workspace.Request) []string {
	if request.Script == nil || request.Script.Prompt == nil {
		return nil
	}
	return request.Script.Prompt.Weak
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
	if len(run.Exec) > 0 {
		var programs []string
		for _, p := range run.Exec {
			programs = append(programs, p.Name+" ("+p.Path+")")
		}
		parts = append(parts, "запуск "+strings.Join(programs, ", "))
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
	requestsCmd.Flags().BoolVar(&requestsReady, "ready", false, "только согласованные, которые можно выполнять сейчас")
	requestsCmd.Flags().StringVar(&requestsAfter, "after", "",
		"что было после: заявки, которые ждут эту, или оформленные в окне триггера (release#1234)")
	requestCmd.Flags().StringSliceVar(&requestAfter, "after", nil, "выполнять только после этих заявок")
	requestCmd.Flags().StringVar(&requestIncident, "incident", "", "инцидент, ради которого заявка: она станет его шагом")
	approveCmd.Flags().StringVar(&requestComment, "comment", "", "комментарий к решению")
	approveCmd.Flags().BoolVar(&approveYes, "yes", false, "план заявки нейросети проверен: согласовать без вопроса")
	rejectCmd.Flags().StringVar(&rejectReason, "reason", "", "причина отказа")
	rejectCmd.Flags().StringVar(&rejectKind, "reason-kind", workspace.KindPlan,
		"в чём причина: plan — план неверен; timing, scope, other — не в плане, правилам не засчитывается")
	applyCmd.Flags().StringVar(&applyRequestID, "request", "", "выполнить согласованную заявку")
	requestCmd.Flags().BoolVar(&requestScript, "script", false, "сценарий на языке алиасов (для файла .ochan подразумевается)")
	requestOpts.register(requestCmd)
	requestPrompt.register(requestCmd)
	applyCmd.Flags().BoolVar(&notifyMail, "notify", false, "отправить уведомление по почте")
	approveCmd.Flags().BoolVar(&notifyMail, "notify", false, "отправить уведомление по почте")
	rejectCmd.Flags().BoolVar(&notifyMail, "notify", false, "отправить уведомление по почте")
}
