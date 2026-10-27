package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"octochan/inventory"
	"octochan/scenario"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

// Политики автоматического выполнения: что ochan watch выполняет сам, без
// человека. Модель — workspace/policy.go.

func policyStore() (*workspace.Policies, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewPolicies(filepath.Join(fm.BaseDir(), "policies"))
}

// whoAmI — пользователь ОС и роль: для записей «кто решил».
func whoAmI() string {
	if currentRole == nil {
		return workspace.CurrentUser()
	}
	return fmt.Sprintf("%s, роль %s", workspace.CurrentUser(), currentRole.Name)
}

var policyRevokeReason string

var policyCmd = &cobra.Command{
	Use:   "policy",
	Short: "Политики автоматического выполнения: что ochan watch выполняет без человека",
	Long: "Политика — заранее данное согласование с границами: сценарий, белый список\n" +
		"меток целей (селекторы инвентаря), белый список параметров сценария с\n" +
		"допустимыми значениями и срок. ochan watch выполняет сам только то, что\n" +
		"в неё укладывается; всё остальное — заявки, а не изменения.\n\n" +
		"Без подкоманды — список политик, и отозванных тоже.",
	Example: "ochan policy\nochan policy add new-servers.yaml\nochan policy show new-servers\n" +
		"ochan policy revoke new-servers --reason \"миграция закончилась\"",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		store, err := policyStore()
		if err != nil {
			return err
		}
		all, err := store.List()
		if err != nil {
			return err
		}
		if len(all) == 0 {
			cmd.Println("Политик нет: ochan watch ничего не выполняет сам.")
			return nil
		}
		now := time.Now()
		for _, p := range all {
			state := "действует " + p.ExpiryText()
			if err := p.State(now); err != nil {
				state = err.Error()
			}
			cmd.Printf("%-16s %s\n", p.Name, state)
			cmd.Printf("%-16s цели %s; параметры %s\n", "", strings.Join(p.Targets, ", "), strings.Join(p.ParamNames(), ", "))
		}
		return nil
	},
}

var policyAddCmd = &cobra.Command{
	Use:   "add <файл>",
	Short: "Зарегистрировать политику: заранее данное согласование",
	Long: "Политику регистрируют с правом approve: по ней ochan watch согласует\n" +
		"заявки сам. Файл:\n\n" +
		"  name: new-servers\n" +
		"  scenario: tune.yaml        # цели — из инвентаря (host, group)\n" +
		"  targets: [group=db_new]    # белый список меток целей\n" +
		"  params:                    # белый список параметров сценария\n" +
		"    max_connections: 100..500\n" +
		"    work_mem: [16MB, 32MB]\n" +
		"    restart: \"*\"\n" +
		"  expires: 2026-12-31\n\n" +
		"Действующую политику с тем же именем не заменяют: сначала её отзывают.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		data, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		now := time.Now()
		p, err := workspace.ParsePolicy(args[0], data, now)
		if err != nil {
			return err
		}
		if _, err := inventory.ParseSelectors(p.Targets); err != nil {
			return fmt.Errorf("политика %s: targets: %w", p.Name, err)
		}
		parsed, err := policyScenario(p)
		if err != nil {
			return err
		}
		p.Service = parsed.Service
		p.AddedBy = whoAmI()
		if currentRole != nil {
			p.AddedRole = currentRole.Name
		}
		store, err := policyStore()
		if err != nil {
			return err
		}
		if err := store.Add(p, now); err != nil {
			return err
		}
		cmd.Printf("Политика %s зарегистрирована.\n\n", p.Name)
		printPolicy(cmd, p)
		if outside := paramsOutside(p, parsed); len(outside) > 0 {
			cmd.Printf("\nвнимание: сценарий сейчас выходит за политику — watch будет оформлять заявки:\n  %s\n",
				strings.Join(outside, "\n  "))
		}
		cmd.Printf("\nОтозвать: ochan policy revoke %s\n", p.Name)
		return nil
	},
}

var policyShowCmd = &cobra.Command{
	Use:   "show <имя>",
	Short: "Показать политику и заявки, согласованные ею",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := policyStore()
		if err != nil {
			return err
		}
		p, err := store.Get(args[0])
		if err != nil {
			return err
		}
		printPolicy(cmd, p)
		requests, err := requestStore()
		if err != nil {
			return err
		}
		all, err := requests.List()
		if err != nil {
			return err
		}
		graph := workspace.NewGraph(all)
		var count int
		for _, r := range all {
			if r.Policy != p.Name {
				continue
			}
			if count == 0 {
				cmd.Println("\nСогласовано политикой:")
			}
			count++
			cmd.Println("  " + graph.DescribeIn(r))
		}
		if count == 0 {
			cmd.Println("\nПолитикой ещё ничего не согласовано.")
		}
		return nil
	},
}

var policyRevokeCmd = &cobra.Command{
	Use:   "revoke <имя>",
	Short: "Отозвать политику: дальше всё — заявками",
	Long: "Отзыв действует со следующего прохода ochan watch, в том числе уже\n" +
		"запущенного с --every. Выполненное по политике остаётся: вернуть его —\n" +
		"ochan rollback или ochan compensate по заявкам из ochan policy show.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := policyStore()
		if err != nil {
			return err
		}
		p, err := store.Revoke(args[0], whoAmI(), policyRevokeReason, time.Now())
		if err != nil {
			return err
		}
		cmd.Printf("Политика %s отозвана (%s). ochan watch больше ничего не выполняет по ней сам.\n", p.Name, p.RevokedBy)
		return nil
	},
}

// policyScenario — сценарий политики. Цели он берёт из инвентаря: watch
// сужает их до нового хоста.
func policyScenario(p *workspace.Policy) (*scenario.Scenario, error) {
	src, err := readSource(p.Scenario)
	if err != nil {
		return nil, fmt.Errorf("политика %s: %w", p.Name, err)
	}
	parsed, err := parseScenario(src)
	if err != nil {
		return nil, fmt.Errorf("политика %s: %w", p.Name, err)
	}
	if !parsed.UsesInventory() {
		return nil, fmt.Errorf("политика %s: сценарий %s не берёт цели из инвентаря (host или group) — "+
			"watch не сузит его до нового хоста", p.Name, p.Scenario)
	}
	return parsed, nil
}

// paramsOutside — параметры сценария вне белого списка политики.
func paramsOutside(p *workspace.Policy, parsed *scenario.Scenario) []string {
	var out []string
	for _, name := range sortedPaths(parsed.Parameters) {
		value := inventory.Text(parsed.Parameters[name])
		rule, ok := p.Params[name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("параметр %s не в params", name))
		case !rule.Allows(value):
			out = append(out, fmt.Sprintf("параметр %s = %s вне допустимого (%s)", name, value, rule))
		}
	}
	return out
}

func printPolicy(cmd *cobra.Command, p *workspace.Policy) {
	state := "действует " + p.ExpiryText()
	if err := p.State(time.Now()); err != nil {
		state = err.Error()
	}
	cmd.Printf("Политика:  %s — %s\n", p.Name, state)
	cmd.Printf("Сценарий:  %s (сервис %s)\n", p.Scenario, p.Service)
	cmd.Printf("Цели:      %s\n", strings.Join(p.Targets, ", "))
	for _, name := range p.ParamNames() {
		cmd.Printf("Параметр:  %s: %s\n", name, p.Params[name])
	}
	cmd.Printf("Добавил:   %s, %s\n", p.AddedBy, p.AddedAt.Local().Format("2006-01-02 15:04"))
	if p.File != "" {
		cmd.Printf("Файл:      %s (sha256 %s…)\n", p.File, p.Sum[:12])
	}
}

func init() {
	policyRevokeCmd.Flags().StringVar(&policyRevokeReason, "reason", "", "почему политика отозвана")
	policyCmd.AddCommand(policyAddCmd, policyShowCmd, policyRevokeCmd)
	rootCmd.AddCommand(policyCmd)
}
