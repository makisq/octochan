package cmd

import (
	"fmt"
	"strings"

	"octochan/config"
	"octochan/workspace"

	"github.com/spf13/cobra"
)

var historyLimit int

// historyStore открывает журнал операций.
func historyStore() (*workspace.History, error) {
	fm, err := mustFileManager()
	if err != nil {
		return nil, err
	}
	return workspace.NewHistory(fm.HistoryDir())
}

// record записывает событие, не прерывая работу при сбое записи:
// неудача журналирования не должна отменять уже выполненное действие.
func record(cmd *cobra.Command, entry *workspace.Entry) {
	store, err := historyStore()
	if err != nil {
		cmd.PrintErrf("внимание: история недоступна: %v\n", err)
		return
	}
	if err := store.Append(entry); err != nil {
		cmd.PrintErrf("внимание: не удалось записать историю: %v\n", err)
	}
}

var historyCmd = &cobra.Command{
	Use:   "history [id]",
	Short: "Показать журнал операций",
	Long: "Без аргументов перечисляет операции от новых к старым.\n" +
		"С идентификатором (можно указать только начало) показывает подробности.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := historyStore()
		if err != nil {
			return err
		}

		if len(args) == 1 {
			return showHistoryEntry(cmd, store, args[0])
		}

		entries, err := store.List(historyLimit)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			cmd.Printf("История пуста. Журнал: %s\n", store.Dir())
			return nil
		}

		for _, entry := range entries {
			marker := " "
			switch {
			case entry.Reverted():
				marker = "-"
			case entry.Reversible:
				marker = "+"
			}
			cmd.Printf("%s %s\n", marker, entry.Describe())
		}

		cmd.Println("\n+ обратимо, - уже отменено")
		return nil
	},
}

func showHistoryEntry(cmd *cobra.Command, store *workspace.History, id string) error {
	entry, err := store.Get(id)
	if err != nil {
		return err
	}

	cmd.Printf("Запись:   %s\n", entry.ID)
	cmd.Printf("Время:    %s\n", entry.Timestamp.Local().Format("2006-01-02 15:04:05"))
	cmd.Printf("Автор:    %s\n", entry.Author)
	cmd.Printf("Операция: %s (%s)\n", entry.Kind.Russian(), entry.Command)
	if entry.Source != "" {
		cmd.Printf("Источник: %s\n", entry.Source)
	}
	if entry.Target != "" {
		cmd.Printf("Цель:     %s\n", entry.Target)
	}
	cmd.Printf("Итог:     %s\n", entry.Summary)

	switch {
	case entry.Reverted():
		cmd.Printf("Отменено: записью %s\n", entry.RevertedBy)
	case entry.RevertOf != "":
		cmd.Printf("Отменяет: запись %s\n", entry.RevertOf)
	case entry.Reversible:
		cmd.Printf("Откат:    ochan rollback %s\n", entry.ID)
	default:
		cmd.Println("Откат:    невозможен")
	}

	if len(entry.TaskIDs) > 0 {
		cmd.Printf("Задачи:   %s\n", strings.Join(entry.TaskIDs, ", "))
	}

	if len(entry.Changes) > 0 {
		plan := &config.Plan{Changes: entry.Changes}
		cmd.Print("\n" + plan.Render(60))
	}
	return nil
}

var rollbackForce bool

var rollbackCmd = &cobra.Command{
	Use:   "rollback <id>",
	Short: "Отменить изменение файла из истории",
	Long: "Возвращает файл к состоянию до операции.\n\n" +
		"Откатить можно только правку файла. Задачи, созданные во внешней\n" +
		"системе, необратимы: перезапуск уже произошёл, и отменять в октохане\n" +
		"нечего — для таких записей команда откажет и скажет об этом прямо.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := historyStore()
		if err != nil {
			return err
		}

		entry, err := store.Get(args[0])
		if err != nil {
			return err
		}

		if !entry.Reversible {
			return fmt.Errorf("запись %s необратима (%s): откатывать нечего",
				entry.ID, entry.Kind.Russian())
		}
		if entry.Reverted() && !rollbackForce {
			return fmt.Errorf("запись %s уже отменена записью %s; используйте --force, "+
				"чтобы применить откат повторно", entry.ID, entry.RevertedBy)
		}
		if entry.Target == "" || entry.Before == nil {
			return fmt.Errorf("в записи %s нет сохранённого состояния файла", entry.ID)
		}

		// Текущее содержимое запоминаем до перезаписи: откат сам является
		// изменением, и его тоже нужно уметь отменить.
		var current config.ConfigMap
		if loaded, err := config.LoadConfigFrom(cmd.Context(), entry.Target, config.SourceOptions{}); err == nil {
			current = loaded.Data
		}

		if err := config.SaveConfig(entry.Before, entry.Target); err != nil {
			return err
		}

		revert := &workspace.Entry{
			Kind:       workspace.EntryPatch,
			Command:    "rollback " + entry.ID,
			Target:     entry.Target,
			Source:     "история " + entry.ID,
			Summary:    fmt.Sprintf("откат операции %s", entry.ID),
			Before:     current,
			Reversible: current != nil,
			RevertOf:   entry.ID,
		}
		record(cmd, revert)

		if err := store.MarkReverted(entry, revert.ID); err != nil {
			cmd.PrintErrf("внимание: файл восстановлен, но пометить запись не удалось: %v\n", err)
		}

		cmd.Printf("%s восстановлен из записи %s (откат записан как %s)\n",
			entry.Target, entry.ID, revert.ID)
		return nil
	},
}

func init() {
	historyCmd.Flags().IntVar(&historyLimit, "limit", 20, "сколько записей показать (0 — все)")
	rollbackCmd.Flags().BoolVar(&rollbackForce, "force", false, "откатить повторно")
}
