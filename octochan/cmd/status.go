package cmd

import (
	"octochan/rlm"

	"github.com/spf13/cobra"
)

// status специфичен для исполнителя RLM: у его задач есть опрашиваемый
// статус и журнал событий. Поэтому команда живёт отдельно от apply,
// который про способ выполнения ничего не знает.
var statusCmd = &cobra.Command{
	Use:   "status <task_id>",
	Short: "Проверить статус задачи RLM",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rlm.NewAPIClient()
		if err != nil {
			return err
		}

		taskID := args[0]
		status, err := client.TaskStatusWithEvents(cmd.Context(), taskID)
		if err != nil {
			return err
		}

		cmd.Printf("\nСтатус задачи %s\n", taskID)
		cmd.Println("----------------------------")
		printField(cmd, status, "status", "Состояние")
		printField(cmd, status, "created_at", "Создана")
		printField(cmd, status, "updated_at", "Обновлена")
		if progress, ok := status["progress"].(float64); ok {
			cmd.Printf("Прогресс: %.0f%%\n", progress)
		}

		events, ok := status["events"].([]map[string]any)
		if !ok || len(events) == 0 {
			return nil
		}

		cmd.Println("\nЛоги выполнения:")
		for _, event := range events {
			cmd.Printf("[%v] %v - %v\n", event["timestamp"], event["level"], event["message"])
		}
		return nil
	},
}

func printField(cmd *cobra.Command, data map[string]any, key, label string) {
	if value, ok := data[key]; ok && value != nil {
		cmd.Printf("%s: %v\n", label, value)
	}
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
