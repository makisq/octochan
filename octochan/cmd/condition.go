package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"octochan/config"

	"github.com/spf13/cobra"
)

// lastIfResult хранит исход последнего if для команды else.
// Обе команды выполняются последовательно в одной оболочке, поэтому
// дополнительной синхронизации не требуется.
var lastIfResult *bool

// supportedOperators перечисляется в справке и в сообщении об ошибке.
var supportedOperators = []string{"==", "!=", ">", "<", ">=", "<=", "contains", "startsWith", "endsWith"}

const conditionUsage = "<файл> <[секция.]параметр> <оператор> <значение> <сообщение>"

var ifCmd = &cobra.Command{
	Use:   "if " + conditionUsage,
	Short: "Условная проверка значения в конфиге",
	Example: `ochan if config.conf server.port == 8080 "Порт корректен"
ochan if postgresql.conf shared_buffers '>=' 4GB "Буферов достаточно"`,
	Args: cobra.ExactArgs(5),
	RunE: func(cmd *cobra.Command, args []string) error {
		met, err := runCondition(cmd, args)
		if err != nil {
			return err
		}
		lastIfResult = &met
		return nil
	},
}

// elseCmd раньше был объявлен, но нигде не регистрировался — ~120 строк
// копии ifCmd, до которых нельзя было добраться из CLI.
var elseCmd = &cobra.Command{
	Use:   "else " + conditionUsage,
	Short: "Альтернативная проверка, если предыдущий if не сработал",
	Long: "Работает только внутри интерактивной оболочки (ochan shell):\n" +
		"результат предыдущего if живёт в памяти процесса, а при отдельных\n" +
		"запусках из терминала каждая команда — это новый процесс.",
	Example: `ochan shell
ochan> if config.conf server.port == 8080 "Порт корректен"
ochan> else config.conf server.port != 8080 "Исправьте порт на 8080"`,
	Args: cobra.ExactArgs(5),
	RunE: func(cmd *cobra.Command, args []string) error {
		if lastIfResult == nil {
			return fmt.Errorf("команда else должна идти после команды if " +
				"в одной сессии (запустите 'ochan shell')")
		}
		if *lastIfResult {
			cmd.Println("Предыдущее условие if выполнено, else пропускается")
			return nil
		}

		_, err := runCondition(cmd, args)
		return err
	},
}

// runCondition — общее тело для if и else.
func runCondition(cmd *cobra.Command, args []string) (bool, error) {
	filePath, paramPath, operator, expected, message := args[0], args[1], args[2], args[3], args[4]

	src, err := loadConfigArg(cmd.Context(), filePath, config.SourceOptions{})
	if err != nil {
		return false, err
	}
	cfg := src.Data

	section, key := splitParamPath(paramPath)
	current, exists := cfg[section][key]
	if !exists {
		where := "без секции"
		if section != "" {
			where = "секции " + section
		}
		return false, fmt.Errorf("параметр %s не найден в %s (%s)", key, where, src.Name)
	}

	met, err := evalCondition(current, operator, expected)
	if err != nil {
		return false, err
	}

	if met {
		cmd.Printf("%s\n", message)
		cmd.Printf("   %s = %s %s %s\n", paramPath, current, operator, expected)
	} else {
		cmd.Printf("Условие не выполнено: %s %s %s\n", current, operator, expected)
	}
	return met, nil
}

// splitParamPath разбирает "section.key" и просто "key".
// Прежняя версия требовала ровно две части, поэтому параметры
// postgresql.conf (где секций нет) проверить было нельзя.
func splitParamPath(path string) (section, key string) {
	if idx := strings.LastIndex(path, "."); idx > 0 {
		return path[:idx], path[idx+1:]
	}
	return "", path
}

// evalCondition сравнивает значения.
//
// Равенство считается по нормализованным значениям, как в diff: раньше if
// сравнивал сырые строки, поэтому "8GB" и "8192MB" были разными, хотя diff
// считал их одинаковыми.
func evalCondition(current, operator, expected string) (bool, error) {
	switch operator {
	case "==":
		return config.NormalizeValue(current) == config.NormalizeValue(expected), nil
	case "!=":
		return config.NormalizeValue(current) != config.NormalizeValue(expected), nil
	case "contains":
		return strings.Contains(current, expected), nil
	case "startsWith":
		return strings.HasPrefix(current, expected), nil
	case "endsWith":
		return strings.HasSuffix(current, expected), nil
	case ">", "<", ">=", "<=":
		return compareOrdered(current, expected, operator)
	default:
		return false, fmt.Errorf("неподдерживаемый оператор %q; доступны: %s",
			operator, strings.Join(supportedOperators, ", "))
	}
}

func compareOrdered(current, expected, operator string) (bool, error) {
	// Сначала пробуем числа с учётом единиц измерения (8GB, 100ms),
	// иначе сравниваем лексикографически.
	left, leftOK := numericValue(current)
	right, rightOK := numericValue(expected)

	if leftOK && rightOK {
		switch operator {
		case ">":
			return left > right, nil
		case "<":
			return left < right, nil
		case ">=":
			return left >= right, nil
		case "<=":
			return left <= right, nil
		}
	}

	switch operator {
	case ">":
		return current > expected, nil
	case "<":
		return current < expected, nil
	case ">=":
		return current >= expected, nil
	case "<=":
		return current <= expected, nil
	}
	return false, fmt.Errorf("неподдерживаемый оператор %q", operator)
}

// numericValue извлекает число из нормализованного значения.
func numericValue(value string) (float64, bool) {
	normalized := config.NormalizeValue(value)
	for _, prefix := range []string{"num:", "size:", "dur:"} {
		if strings.HasPrefix(normalized, prefix) {
			n, err := strconv.ParseFloat(strings.TrimPrefix(normalized, prefix), 64)
			return n, err == nil
		}
	}
	return 0, false
}
