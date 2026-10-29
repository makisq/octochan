package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"octochan/config"
	"octochan/inventory"
	"octochan/rbac"
	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

var (
	watchSource   string
	watchPolicy   string
	watchScenario string
	watchEvery    time.Duration
)

var watchCmd = &cobra.Command{
	Use:   "watch --source <источник> (--policy <имя|файл> | --scenario <сценарий>)",
	Short: "Следить за инвентарём: новый хост — план, заявка, а по политике — выполнение",
	Long: "Проход: читает источник инвентаря, находит новые хосты (которых ещё не\n" +
		"видели), для каждого строит план сценария, суженного до этого хоста, и\n" +
		"оформляет заявку. Обработанный хост отмечается увиденным.\n\n" +
		"Проверять и планировать по расписанию ochan может сам; выполнять — только\n" +
		"по зарегистрированной политике (ochan policy add). Заявка, которая в неё\n" +
		"укладывается — метки хоста, параметры сценария, сервис, план без\n" +
		"прерывания обслуживания, предел роли на apply, — согласуется политикой и\n" +
		"выполняется. Всё остальное остаётся заявкой и ждёт человека; причина\n" +
		"печатается. Отозванная или истёкшая политика выключает выполнение со\n" +
		"следующего прохода.\n\n" +
		"Без --every — один проход: для cron. С --every — проходы до остановки.",
	Example: "ochan watch --source cmdb --policy new-servers\n" +
		"ochan watch --source cmdb --policy new-servers.yaml --every 10m\n" +
		"ochan watch --source cmdb --scenario tune.yaml",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		sources, err := inventorySources()
		if err != nil {
			return err
		}
		ref, ok := sources[watchSource]
		if !ok {
			names := make([]string, 0, len(sources))
			for name := range sources {
				names = append(names, name)
			}
			sort.Strings(names)
			return fmt.Errorf("источника инвентаря %q нет; есть: %s (inventory.sources в config.yaml)",
				watchSource, orNone(names))
		}
		name, scenarioPath, err := watchTarget()
		if err != nil {
			return err
		}
		if watchEvery > 0 && watchEvery < time.Minute {
			return fmt.Errorf("--every %s: не чаще раза в минуту", watchEvery)
		}

		for {
			if err := watchPass(cmd, watchSource, ref, name, scenarioPath); err != nil {
				if watchEvery == 0 {
					return err
				}
				cmd.PrintErrf("проход прерван: %v\n", err)
			}
			if watchEvery == 0 {
				return nil
			}
			select {
			case <-cmd.Context().Done():
				return nil
			case <-time.After(watchEvery):
			}
		}
	},
}

// watchTarget — имя политики и сценарий. Файл политики годится, только
// если он тот же, что зарегистрирован: правка файла — не регистрация.
func watchTarget() (policy, scenarioPath string, err error) {
	switch {
	case watchPolicy == "" && watchScenario == "":
		return "", "", fmt.Errorf("укажите --policy (выполнять по политике) или --scenario (только заявки)")
	case watchPolicy == "":
		return "", watchScenario, nil
	case watchScenario != "":
		return "", "", fmt.Errorf("сценарий задаёт политика: --scenario вместе с --policy не нужен")
	}
	store, err := policyStore()
	if err != nil {
		return "", "", err
	}
	name := watchPolicy
	if data, err := os.ReadFile(watchPolicy); err == nil {
		file, err := workspace.ParsePolicy(watchPolicy, data, time.Time{})
		if err != nil {
			return "", "", err
		}
		registered, err := store.Get(file.Name)
		if err != nil {
			return "", "", err
		}
		if registered.Sum != file.Sum {
			return "", "", fmt.Errorf("файл %s отличается от зарегистрированной политики %s: правка файла — не "+
				"регистрация; ochan policy revoke %s и ochan policy add %s", watchPolicy, file.Name, file.Name, watchPolicy)
		}
		name = file.Name
	}
	p, err := store.Get(name)
	if err != nil {
		return "", "", err
	}
	return p.Name, p.Scenario, nil
}

// watchPass — один проход по источнику.
func watchPass(cmd *cobra.Command, source, ref, policyName, scenarioPath string) error {
	now := time.Now()
	hosts, err := inventory.Load(cmd.Context(), map[string]string{source: ref})
	if err != nil {
		return err
	}
	seen, err := seenStore()
	if err != nil {
		return err
	}
	fresh := seen.New(hosts)
	cmd.Printf("%s  %s: хостов %d, новых %d\n", now.Format("2006-01-02 15:04"), source, len(hosts), len(fresh))
	if len(fresh) == 0 {
		return nil
	}

	// Политика перечитывается на каждом проходе: отзыв действует сразу.
	var policy *workspace.Policy
	if policyName != "" {
		store, err := policyStore()
		if err != nil {
			return err
		}
		if policy, err = store.Get(policyName); err == nil {
			err = policy.State(now)
		}
		if err != nil {
			cmd.Printf("  Выполнение без человека выключено: %v. Всё станет заявками.\n", err)
			policy = nil
		}
	}
	requests, err := requestStore()
	if err != nil {
		return err
	}
	for _, h := range fresh {
		outcome, err := watchHost(cmd, requests, h, policy, scenarioPath)
		if err != nil {
			// Хост не отмечается: следующий проход попробует снова.
			cmd.Printf("  %s: %v\n", h.Name, err)
			continue
		}
		cmd.Printf("  %s: %s\n", h.Name, outcome)
		if err := seen.Mark([]inventory.Host{h}, now); err != nil {
			return err
		}
	}
	return nil
}

// watchHost строит план для нового хоста, оформляет заявку и, если она
// в политике, согласует и выполняет её. Ошибка — хост не обработан.
func watchHost(cmd *cobra.Command, store *workspace.Requests, h inventory.Host, policy *workspace.Policy,
	scenarioPath string) (string, error) {
	src, err := readSource(scenarioPath)
	if err != nil {
		return "", err
	}
	parsed, err := parseScenario(src)
	if err != nil {
		return "", err
	}
	if !parsed.UsesInventory() {
		return "", fmt.Errorf("сценарий %s не берёт цели из инвентаря (host или group)", src.Name)
	}
	// Цели — только этот хост; заявка помнит это --limit, и выполнение
	// развернёт цели по инвентарю так же.
	if err := parsed.ResolveTargets([]inventory.Host{h}, nil); err != nil {
		return "сценарий этот хост не выбирает — отмечен увиденным", nil
	}
	runner, err := scenario.Resolve(parsed.Service)
	if err != nil {
		return "", err
	}
	plan, err := runner.Plan(cmd.Context(), parsed)
	if err != nil {
		return "", fmt.Errorf("план не построен: %w", err)
	}
	if plan.Len() == 0 {
		return "изменений нет", nil
	}

	request := &workspace.Request{
		Source:  src.Name,
		Service: parsed.Service,
		Summary: fmt.Sprintf("%s через %s, операций: %d; новый хост %s (%s)",
			parsed.Service, runner.Kind(), plan.Len(), h.Name, h.Source),
		Plan:    plan,
		Payload: src.Content,
		Limit:   []string{"host=" + h.Name},
		Author:  "ochan watch (" + workspace.CurrentUser() + ")",
	}
	if currentRole != nil {
		request.Role = currentRole.Name
	}
	if err := store.Create(request); err != nil {
		return "", err
	}
	if policy == nil {
		return fmt.Sprintf("заявка %s ждёт решения", request.ID), nil
	}
	if reasons := policyVerdict(cmd, policy, h, parsed, plan, request); len(reasons) > 0 {
		return fmt.Sprintf("заявка %s ждёт решения — вне политики %s: %s",
			request.ID, policy.Name, strings.Join(reasons, "; ")), nil
	}

	request.Policy = policy.Name
	who := fmt.Sprintf("политика %s (зарегистрировал %s)", policy.Name, policy.AddedBy)
	comment := fmt.Sprintf("хост %s под %s, параметры в пределах политики", h.Name, strings.Join(policy.Targets, ", "))
	if err := store.Decide(request, workspace.RequestApproved, who, comment); err != nil {
		return "", err
	}
	cmd.Printf("  %s: заявка %s в политике %s — выполняется\n", h.Name, request.ID, policy.Name)
	if err := applyApprovedRequest(cmd, request.ID); err != nil {
		return fmt.Sprintf("заявка %s согласована политикой %s, выполнение не удалось: %v",
			request.ID, policy.Name, err), nil
	}
	return fmt.Sprintf("заявка %s согласована политикой %s и выполнена", request.ID, policy.Name), nil
}

// policyVerdict — почему заявка не укладывается в политику; пусто —
// укладывается. Право и предел роли на apply проверяются как у команды
// apply и пишутся в журнал аудита: выполнение по политике — тоже
// выполнение.
func policyVerdict(cmd *cobra.Command, p *workspace.Policy, h inventory.Host, parsed *scenario.Scenario,
	plan *config.Plan, request *workspace.Request) []string {
	var out []string
	if parsed.Service != p.Service {
		out = append(out, fmt.Sprintf("сервис %s, а политика — для %s", parsed.Service, p.Service))
	}
	sels, err := inventory.ParseSelectors(p.Targets)
	if err != nil || !inventory.MatchAny(sels, h.Labels()) {
		out = append(out, fmt.Sprintf("хост не подходит под targets (%s)", strings.Join(p.Targets, ", ")))
	}
	out = append(out, paramsOutside(p, parsed)...)
	if plan.Destructive {
		out = append(out, "план прерывает обслуживание — такое только через человека")
	}
	if currentRole != nil && p.AddedRole != "" && strings.EqualFold(p.AddedRole, currentRole.Name) &&
		currentRole.Name != rbac.Master {
		out = append(out, fmt.Sprintf("политику зарегистрировала та же роль %s — свою заявку не согласуют", p.AddedRole))
	}
	if len(out) == 0 && currentRole != nil {
		if err := admit(cmd, currentRole, "apply", []string{"--request", request.ID}); err != nil {
			out = append(out, err.Error())
		}
	}
	return out
}

func init() {
	watchCmd.Flags().StringVar(&watchSource, "source", "", "источник инвентаря (имя из inventory.sources или local)")
	watchCmd.Flags().StringVar(&watchPolicy, "policy", "", "политика: имя или её файл — выполнять то, что в неё укладывается")
	watchCmd.Flags().StringVar(&watchScenario, "scenario", "", "сценарий без политики: только заявки")
	watchCmd.Flags().DurationVar(&watchEvery, "every", 0, "повторять проход с этим промежутком (от 1m); без него — один проход")
	_ = watchCmd.MarkFlagRequired("source")
	rootCmd.AddCommand(watchCmd)
}
