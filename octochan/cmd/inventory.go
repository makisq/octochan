package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"octochan/inventory"
	"octochan/scenario"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// localInventory — файл инвентаря в рабочем каталоге: источник local,
// если в config.yaml он не назван иначе.
const localInventory = "inventory.yaml"

// inventorySources — источники инвентаря: inventory.sources из config.yaml
// и ~/.octochan/inventory.yaml, если он есть.
func inventorySources() (map[string]string, error) {
	sources := map[string]string{}
	for name, ref := range viper.GetStringMapString("inventory.sources") {
		sources[name] = ref
	}
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	local := filepath.Join(fm.BaseDir(), localInventory)
	if _, taken := sources["local"]; !taken {
		if _, err := os.Stat(local); err == nil {
			sources["local"] = local
		}
	}
	return sources, nil
}

// errLimitConfig — --limit у правки конфига: целей у неё нет.
var errLimitConfig = errors.New("--limit отбирает цели сценария и хосты инвентаря; у правки конфига целей нет — " +
	"её цель — файл")

// errNoInventory — инвентаря нет, а он нужен.
var errNoInventory = errors.New("инвентаря нет: источники задаются в config.yaml (inventory.sources) " +
	"или файлом ~/.octochan/inventory.yaml")

// loadInventory читает все источники инвентаря. nil без ошибки — источников
// нет вовсе.
func loadInventory(ctx context.Context) ([]inventory.Host, error) {
	sources, err := inventorySources()
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, nil
	}
	hosts, err := inventory.Load(ctx, sources)
	if hosts == nil && err == nil {
		hosts = []inventory.Host{}
	}
	return hosts, err
}

// resolveTargets разворачивает цели сценария по инвентарю и сужает их
// --limit. Инвентарь читается, только если сценарий выбирает из него.
func resolveTargets(ctx context.Context, parsed *scenario.Scenario, limit []string) error {
	sels, err := inventory.ParseSelectors(limit)
	if err != nil {
		return fmt.Errorf("--limit: %w", err)
	}
	var hosts []inventory.Host
	if parsed.UsesInventory() {
		hosts, err = loadInventory(ctx)
		if err != nil {
			return err
		}
		if hosts == nil {
			return fmt.Errorf("сценарий выбирает цели из инвентаря (host, group), а %w", errNoInventory)
		}
	}
	if err := parsed.ResolveTargets(hosts, sels); err != nil {
		return err
	}
	return checkTriggerScope(parsed)
}

// scriptInventory — хосты, которые увидит inventory() сценария: все
// источники, суженные --limit. Ошибка не прерывает сценарий, который
// инвентарём не пользуется: её вернёт сам inventory().
func scriptInventory(ctx context.Context, limit []string) ([]inventory.Host, error) {
	sels, err := inventory.ParseSelectors(limit)
	if err != nil {
		return nil, fmt.Errorf("--limit: %w", err)
	}
	hosts, err := loadInventory(ctx)
	if err != nil || hosts == nil {
		if len(limit) > 0 && err == nil {
			err = fmt.Errorf("--limit отбирает хосты инвентаря, а %w", errNoInventory)
		}
		return nil, err
	}
	return scopeHosts(inventory.Filter(hosts, sels)), nil
}

func seenStore() (*inventory.Seen, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return inventory.LoadSeen(filepath.Join(fm.BaseDir(), "inventory.seen.json"))
}

var inventoryNew bool

var inventoryCmd = &cobra.Command{
	Use:   "inventory [селектор...]",
	Short: "Цели: хосты инвентаря, их группы и переменные",
	Long: "Инвентарь — откуда берутся цели сценариев. Источники — inventory.sources\n" +
		"в config.yaml (имя: адрес) и ~/.octochan/inventory.yaml; схема адреса\n" +
		"выбирает источник, как у конфигов (file — YAML-файл, остальные приносят\n" +
		"модули). Цель сценария с полем host или group разворачивается в\n" +
		"подходящие хосты, а --limit у plan, apply, request и run сужает цели.\n\n" +
		"Хост, которого ещё не видели, — новый (+): --new показывает только их,\n" +
		"ochan inventory ack отмечает их увиденными.",
	Example: "ochan inventory\nochan inventory group=db_new\nochan inventory --new",
	RunE: func(cmd *cobra.Command, args []string) error {
		sources, hosts, err := selectedHosts(cmd, args)
		if err != nil {
			return err
		}
		seen, err := seenStore()
		if err != nil {
			return err
		}
		fresh := seen.New(hosts)
		if inventoryNew {
			hosts = fresh
		}

		names := make([]string, 0, len(sources))
		for name := range sources {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			cmd.Printf("Источник %s: %s\n", name, sources[name])
		}
		cmd.Println()
		for _, h := range hosts {
			mark := " "
			if seen.IsNew(h) {
				mark = "+"
			}
			cmd.Printf("%s %-16s %-24s %s\n", mark, h.Name, orNone(h.Groups), hostVars(h))
		}
		cmd.Printf("\nХостов: %d, новых: %d (+)\n", len(hosts), len(fresh))
		return nil
	},
}

var inventoryAckCmd = &cobra.Command{
	Use:   "ack [селектор...]",
	Short: "Отметить новые хосты увиденными",
	Long: "Новый хост — тот, которого ещё не видели: ochan watch строит для него план\n" +
		"по политике. ack отмечает новые хосты увиденными — это решение оператора,\n" +
		"а не чтение, и право на него — approve.",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, hosts, err := selectedHosts(cmd, args)
		if err != nil {
			return err
		}
		seen, err := seenStore()
		if err != nil {
			return err
		}
		fresh := seen.New(hosts)
		if err := seen.Mark(fresh, time.Now()); err != nil {
			return err
		}
		for _, h := range fresh {
			cmd.Printf("  %s (%s)\n", h.Name, h.Source)
		}
		cmd.Printf("Отмечены увиденными: %d\n", len(fresh))
		return nil
	},
}

// selectedHosts — источники и хосты инвентаря под селекторами.
func selectedHosts(cmd *cobra.Command, args []string) (map[string]string, []inventory.Host, error) {
	sources, err := inventorySources()
	if err != nil {
		return nil, nil, err
	}
	if len(sources) == 0 {
		return nil, nil, errNoInventory
	}
	sels, err := inventory.ParseSelectors(args)
	if err != nil {
		return nil, nil, err
	}
	hosts, err := inventory.Load(cmd.Context(), sources)
	if err != nil {
		return nil, nil, err
	}
	return sources, inventory.Filter(hosts, sels), nil
}

// hostVars — переменные хоста одной строкой по алфавиту.
func hostVars(h inventory.Host) string {
	keys := make([]string, 0, len(h.Vars))
	for k := range h.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + inventory.Text(h.Vars[k])
	}
	return strings.Join(parts, " ")
}

func init() {
	inventoryCmd.Flags().BoolVar(&inventoryNew, "new", false, "только новые хосты — которых ещё не видели")
	inventoryCmd.AddCommand(inventoryAckCmd)
	rootCmd.AddCommand(inventoryCmd)
}
