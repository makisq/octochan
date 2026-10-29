package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
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

	if len(entry.Files) > 0 {
		cmd.Println("\nФайлы до операции (к этому вернёт откат):")
		for _, f := range entry.Files {
			if f.Existed {
				cmd.Printf("  %s — был, байт: %d\n", f.Path, len(f.Content))
			} else {
				cmd.Printf("  %s — не было, откат удалит\n", f.Path)
			}
		}
	}
	return nil
}

var (
	rollbackForce   bool
	rollbackNotPlan bool
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback <id>",
	Short: "Отменить изменение файлов из истории",
	Long: "Возвращает файлы к состоянию до операции.\n\n" +
		"Откатить можно правку файла и сценарий, выполненный по заявке: каждый\n" +
		"записанный им файл возвращается к прежнему содержимому, а созданный —\n" +
		"удаляется. Если файл поменяли после операции, откат отказывает и\n" +
		"называет его: без --force чужая правка не стирается.\n\n" +
		"Задачи, созданные во внешней системе, необратимы: перезапуск уже\n" +
		"произошёл, и отменять в октохане нечего — для таких записей команда\n" +
		"откажет и скажет об этом прямо.",
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
		if err := checkEntryScope(currentRole, entry); err != nil {
			return refuse(cmd, err)
		}
		if entry.Reverted() && !rollbackForce {
			return fmt.Errorf("запись %s уже отменена записью %s; используйте --force, "+
				"чтобы применить откат повторно", entry.ID, entry.RevertedBy)
		}
		if id, ok := strings.CutPrefix(entry.Command, "apply --request "); ok {
			warnDependents(cmd, id)
		}
		if len(entry.Files) > 0 {
			return rollbackFiles(cmd, store, entry)
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

// rollbackFiles возвращает файлы, записанные операцией, к состоянию до
// неё; файлы, которые она создала, удаляются.
//
// Сначала проверяются все файлы, потом что-то пишется: откат, которому
// помешает правка, сделанная после операции, отказывает целиком, а не
// останавливается на полпути.
func rollbackFiles(cmd *cobra.Command, store *workspace.History, entry *workspace.Entry) error {
	// Откат сам — изменение, и его тоже можно отменить: его запись
	// хранит то, что он перезапишет.
	current := make([]workspace.FileState, len(entry.Files))
	var changed []string
	for i, f := range entry.Files {
		data, err := os.ReadFile(f.Path)
		exists := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("не удалось прочитать %s: %w", f.Path, err)
		}
		current[i] = workspace.FileState{Path: f.Path, Existed: exists, Content: data,
			After: workspace.StateHash(f.Existed, f.Content)}
		if workspace.StateHash(exists, data) != f.After {
			changed = append(changed, f.Path)
		}
	}
	if len(changed) > 0 && !rollbackForce {
		return fmt.Errorf("после операции %s изменились файлы: %s; откат стёр бы эти правки — "+
			"проверьте их и используйте --force, чтобы откатить всё равно", entry.ID, strings.Join(changed, ", "))
	}

	var (
		restored []workspace.FileState
		failure  error
	)
	for i, f := range entry.Files {
		if err := restoreFile(f); err != nil {
			failure = fmt.Errorf("%s: %w", f.Path, err)
			break
		}
		restored = append(restored, current[i])
		if f.Existed {
			cmd.Printf("восстановлен %s\n", f.Path)
		} else {
			cmd.Printf("удалён %s: его создала операция\n", f.Path)
		}
	}

	revert := &workspace.Entry{
		Kind:       workspace.EntryScript,
		Command:    "rollback " + entry.ID,
		Source:     "история " + entry.ID,
		Summary:    fmt.Sprintf("откат операции %s, файлов: %d", entry.ID, len(restored)),
		Files:      restored,
		Reversible: len(restored) > 0,
		RevertOf:   entry.ID,
	}
	if len(restored) > 0 {
		record(cmd, revert)
	}
	if failure != nil {
		// Запись не помечается отменённой: отменена она не вся.
		return fmt.Errorf("откат прерван на %w; восстановлено файлов: %d из %d (записано как %s)",
			failure, len(restored), len(entry.Files), orDash(revert.ID))
	}

	if err := store.MarkReverted(entry, revert.ID); err != nil {
		cmd.PrintErrf("внимание: файлы восстановлены, но пометить запись не удалось: %v\n", err)
	}
	// Откат сценария, выполненного по заявке, — исход его плана: оператор
	// его исправил, и кольца засчитают это правилам плана.
	if id, ok := strings.CutPrefix(entry.Command, "apply --request "); ok {
		if requests, err := requestStore(); err == nil {
			if request, err := requests.Get(id); err == nil {
				if rollbackNotPlan {
					// Откат не из-за плана — исход плана прежний.
					cmd.Println("Откат не по плану (--not-plan): правилам плана он не засчитан.")
				} else {
					noteOperator(cmd, request, workspace.OperatorRolledBack)
				}
			}
		}
	}
	cmd.Printf("Операция %s откатена, файлов: %d (откат записан как %s)\n", entry.ID, len(restored), revert.ID)
	return nil
}

// restoreFile возвращает файл к сохранённому состоянию.
func restoreFile(f workspace.FileState) error {
	if !f.Existed {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(f.Path, f.Content, 0o644)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func init() {
	historyCmd.Flags().IntVar(&historyLimit, "limit", 20, "сколько записей показать (0 — все)")
	rollbackCmd.Flags().BoolVar(&rollbackForce, "force", false,
		"откатить повторно или поверх файлов, изменившихся после операции")
	rollbackCmd.Flags().BoolVar(&rollbackNotPlan, "not-plan", false,
		"откат не из-за плана: правилам плана по формальному промпту не засчитывать")
}
