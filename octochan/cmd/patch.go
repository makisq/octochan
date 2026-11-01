package cmd

import (
	"fmt"
	"os"

	"ochan/config"
	"ochan/workspace"

	"github.com/spf13/cobra"
)

var (
	patchOutput string
	patchForce  bool
)

var patchCmd = &cobra.Command{
	Use:   "patch <base_file> <changes_file>",
	Short: "Применить изменения к основному конфигу",
	// Args раньше не задавались вовсе, поэтому `ochan patch` без аргументов
	// падал с index out of range.
	Args:    cobra.ExactArgs(2),
	Example: "ochan patch prod.conf changes.conf -o patched.conf",
	RunE: func(cmd *cobra.Command, args []string) error {
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

		// Патч — это применение плана: в файл переносятся только настоящие
		// изменения, и ровно они попадают в историю. Раньше копировались
		// все параметры подряд, включая совпадающие.
		diff := config.CompareConfigs(base.Data, changes.Data, base.Name, changes.Name)
		plan := config.PlanFromDiff(diff, base.Name)
		plan.Command = "patch"
		plan.Source = changes.Name

		// Путь вывода раньше был захардкожен как "patched.conf" в текущей
		// директории и молча перезаписывался.
		output := patchOutput
		if output == "" {
			output = "patched.conf"
		}

		// Состояние до записи сохраняем целиком: по нему работает rollback.
		var before config.ConfigMap
		if _, statErr := os.Stat(output); statErr == nil {
			if !patchForce {
				return fmt.Errorf("файл %s уже существует, используйте --force для перезаписи", output)
			}
			if loaded, err := config.LoadConfigFrom(ctx, output, opts); err == nil {
				before = loaded.Data
			}
		}

		if plan.Len() == 0 {
			cmd.Println("Изменений нет — файл не тронут.")
			return nil
		}

		result := config.ApplyPlan(base.Data, plan)
		if err := config.SaveConfig(result, output); err != nil {
			return err
		}

		record(cmd, &workspace.Entry{
			Kind:    workspace.EntryPatch,
			Command: "patch",
			Source:  changes.Name,
			Target:  output,
			Summary: fmt.Sprintf("изменено параметров: %d", plan.Len()),
			Changes: plan.Changes,
			Before:  before,
			// Откатить можно только то, что существовало до операции:
			// для нового файла восстанавливать нечего.
			Reversible: before != nil,
		})

		cmd.Printf("Изменено параметров: %d. Результат сохранён в %s\n", plan.Len(), output)
		return nil
	},
}

func init() {
	patchCmd.Flags().StringVarP(&patchOutput, "output", "o", "",
		"куда записать результат (по умолчанию patched.conf)")
	patchCmd.Flags().BoolVar(&patchForce, "force", false,
		"перезаписать существующий файл")
}
