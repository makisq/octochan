package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"octochan/rlm"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"
)

var authCmd = &cobra.Command{
	Use:   "auth [token]",
	Short: "Установить токен для доступа к RLM API",
	Args:  cobra.MaximumNArgs(1),
	Long: "Токен сохраняется в ~/.octochan/.env с правами 0600.\n" +
		"Альтернатива — переменная окружения " + rlm.EnvTokenVar + ".",
	RunE: func(cmd *cobra.Command, args []string) error {
		fm, err := mustFileManager()
		if err != nil {
			return err
		}

		var token string
		if len(args) == 1 {
			token = strings.TrimSpace(args[0])
		} else {
			token, err = promptForToken(cmd)
			if err != nil {
				return err
			}
		}

		if token == "" {
			return fmt.Errorf("пустой токен")
		}

		if err := rlm.SaveTokenTo(fm.TokenFilePath(), token); err != nil {
			return err
		}

		cmd.Printf("Токен сохранён: %s\n", fm.TokenFilePath())
		return nil
	},
}

// promptForToken читает токен без эха.
// Прежняя версия использовала bufio.Scanner, из-за чего токен оставался
// на экране и в истории терминала.
func promptForToken(cmd *cobra.Command) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		cmd.Print("Введите токен: ")
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return "", fmt.Errorf("не удалось прочитать токен: %w", scanner.Err())
		}
		return strings.TrimSpace(scanner.Text()), nil
	}

	cmd.Print("Введите токен (ввод скрыт): ")
	raw, err := term.ReadPassword(fd)
	cmd.Println()
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать токен: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

var timerCmd = &cobra.Command{
	Use:   "timer [on|off]",
	Short: "Управление напоминанием об истечении токена",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			status := "выключено"
			if viper.GetBool("defaults.token_timer_enabled") {
				status = "включено"
			}
			cmd.Printf("Напоминание о смене токена: %s (время жизни %s)\n",
				status, tokenLifetime())
			return nil
		}

		switch args[0] {
		case "on":
			viper.Set("defaults.token_timer_enabled", true)
			cmd.Println("Напоминание о смене токена включено")
		case "off":
			viper.Set("defaults.token_timer_enabled", false)
			cmd.Println("Напоминание о смене токена выключено")
		default:
			return fmt.Errorf("используйте: timer on или timer off")
		}

		// WriteConfig теперь безопасен: токен в viper не попадает,
		// он живёт в отдельном файле с правами 0600.
		if err := viper.WriteConfig(); err != nil {
			return fmt.Errorf("не удалось сохранить настройки: %w", err)
		}
		return nil
	},
}

func tokenLifetime() time.Duration {
	if d := viper.GetDuration("defaults.token_lifetime"); d > 0 {
		return d
	}
	return 20 * time.Minute
}

// Напоминание работает только в интерактивной оболочке: в одноразовом
// вызове CLI процесс завершается раньше, чем таймер успевает сработать.
//
// Прежняя реализация писала «токен истечёт через 5 минут», затем делала
// time.Sleep(10 * time.Minute) внутри AfterFunc и в конце обнуляла токен
// в viper, ничего на самом деле не отзывая.
var (
	tokenTimerMu   sync.Mutex
	tokenTimerStop chan struct{}
)

func startTokenReminder(out func(string)) {
	if !viper.GetBool("defaults.token_timer_enabled") {
		return
	}

	tokenTimerMu.Lock()
	defer tokenTimerMu.Unlock()

	if tokenTimerStop != nil {
		close(tokenTimerStop)
	}
	stop := make(chan struct{})
	tokenTimerStop = stop

	lifetime := tokenLifetime()
	warnAfter := lifetime - 5*time.Minute
	if warnAfter <= 0 {
		warnAfter = lifetime / 2
	}

	go func() {
		warn := time.NewTimer(warnAfter)
		defer warn.Stop()
		expire := time.NewTimer(lifetime)
		defer expire.Stop()

		select {
		case <-warn.C:
			out("Внимание: токен станет недействителен примерно через " +
				(lifetime - warnAfter).String() + ". Обновите его командой 'auth'.")
		case <-stop:
			return
		}

		select {
		case <-expire.C:
			out("Внимание: время жизни токена истекло. Обновите его командой 'auth'.")
		case <-stop:
		}
	}()
}

func stopTokenReminder() {
	tokenTimerMu.Lock()
	defer tokenTimerMu.Unlock()

	if tokenTimerStop != nil {
		close(tokenTimerStop)
		tokenTimerStop = nil
	}
}
